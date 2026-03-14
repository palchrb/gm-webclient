package connector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// GarminClient is the per-login network client.
// It wraps the slush-dev library's HermesAPI and HermesSignalR.
type GarminClient struct {
	connector *GarminConnector
	userLogin *bridgev2.UserLogin
	phone     string            // logged-in user's phone number
	auth      *gm.HermesAuth    // shared auth; passed to both api and sr
	api       *gm.HermesAPI     // REST client
	sr        *gm.HermesSignalR // SignalR real-time client
	log       zerolog.Logger
}

var _ bridgev2.NetworkAPI = (*GarminClient)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*GarminClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*GarminClient)(nil)

func newGarminClient(gc *GarminConnector, login *bridgev2.UserLogin, auth *gm.HermesAuth, phone string) *GarminClient {
	hermesLog := login.Log.With().Str("component", "hermes").Logger()
	hermesLogger := slog.New(newZerologSlogHandler(hermesLog))
	api := gm.NewHermesAPI(auth, gm.WithAPILogger(hermesLogger))
	sr := gm.NewHermesSignalR(auth,
		gm.WithSignalRLogger(hermesLogger),
	)
	return &GarminClient{
		connector: gc,
		userLogin: login,
		phone:     phone,
		auth:      auth,
		api:       api,
		sr:        sr,
		log:       login.Log.With().Str("component", "garmin-client").Logger(),
	}
}

// ─── bridgev2.NetworkAPI ──────────────────────────────────────────────────────

// Connect validates the session and starts the SignalR listener.
func (c *GarminClient) Connect(ctx context.Context) {
	// Validate session with a lightweight call.
	if _, err := c.api.GetConversations(ctx, gm.WithLimit(1)); err != nil {
		c.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "garmin-auth-error",
			Message:    "Failed to connect to Garmin Messenger: " + err.Error(),
		})
		return
	}

	c.userLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})

	// Register all SignalR callbacks before starting.
	c.sr.OnMessage(func(msg gm.MessageModel) {
		c.handleIncomingMessage(msg)
	})

	c.sr.OnStatusUpdate(func(upd gm.MessageStatusUpdate) {
		c.handleStatusUpdate(upd)
	})

	c.sr.OnOpen(func() {
		c.log.Info().Msg("SignalR connected to Garmin Messenger")
		c.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateConnected,
		})
	})

	c.sr.OnClose(func() {
		c.log.Warn().Msg("SignalR disconnected")
		c.userLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "garmin-signalr-disconnected",
			Message:    "Disconnected from Garmin Messenger real-time service",
		})
	})

	// Start() blocks until ctx is cancelled; the library handles reconnects.
	go func() {
		if err := c.sr.Start(ctx); err != nil && ctx.Err() == nil {
			c.log.Err(err).Msg("SignalR Start returned error")
		}
	}()

	// The framework only sets the space room avatar on initial creation.
	// Update it on every connect so it reflects the current bot avatar from config.
	go c.ensureSpaceAvatar(ctx)
}

// ensureSpaceAvatar updates the space room's m.room.avatar to match the
// NetworkIcon returned by GetName() (which uses the bot's configured avatar).
// Called on every Connect() so that avatar changes in config.yaml take effect
// on the next restart without needing to recreate the space room.
func (c *GarminClient) ensureSpaceAvatar(ctx context.Context) {
	icon := c.connector.br.Network.GetName().NetworkIcon
	if icon == "" {
		return
	}
	spaceRoom, err := c.userLogin.GetSpaceRoom(ctx)
	if err != nil || spaceRoom == "" {
		return
	}
	if _, err := c.userLogin.Bridge.Bot.SendState(ctx, spaceRoom, event.StateRoomAvatar, "", &event.Content{
		Parsed: &event.RoomAvatarEventContent{URL: icon},
	}, time.Now()); err != nil {
		c.log.Warn().Err(err).Msg("Failed to update space room avatar")
	}
}

// Disconnect stops the SignalR connection cleanly.
func (c *GarminClient) Disconnect() {
	c.sr.Stop()
	c.api.Close()
}

// IsLoggedIn returns true if the auth session has credentials.
// Must not do IO.
func (c *GarminClient) IsLoggedIn() bool {
	return c.phone != ""
}

// LogoutRemote invalidates the remote session.
// Garmin has no explicit logout endpoint; clearing the session file is enough.
func (c *GarminClient) LogoutRemote(_ context.Context) {
	sessDir := c.connector.sessionDir(c.userLogin.ID)
	credFile := sessDir + "/hermes_credentials.json"
	if err := removeFile(credFile); err != nil {
		c.log.Warn().Err(err).Msg("Failed to remove session file on logout")
	}
}

// GetCapabilities returns Matrix room feature limits.
func (c *GarminClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	imageMIMEs := map[string]event.CapabilitySupportLevel{
		"image/jpeg": event.CapLevelFullySupported,
		"image/png":  event.CapLevelFullySupported,
		"image/webp": event.CapLevelFullySupported,
		"image/avif": event.CapLevelFullySupported,
	}
	audioMIMEs := map[string]event.CapabilitySupportLevel{
		"audio/ogg":  event.CapLevelFullySupported,
		"audio/mpeg": event.CapLevelFullySupported,
		"audio/mp4":  event.CapLevelFullySupported,
		"audio/wav":  event.CapLevelFullySupported,
		"audio/webm": event.CapLevelFullySupported,
	}
	return &event.RoomFeatures{
		MaxTextLength: 160,
		Reaction:      event.CapLevelFullySupported,
		File: event.FileFeatureMap{
			event.MsgImage:    {MimeTypes: imageMIMEs, Caption: event.CapLevelPartialSupport},
			event.MsgAudio:    {MimeTypes: audioMIMEs},
			event.MsgFile:     {MimeTypes: audioMIMEs},
			event.CapMsgVoice: {MimeTypes: audioMIMEs},
		},
	}
}

// GetChatInfo returns the Matrix room configuration for a Garmin conversation.
func (c *GarminClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	convID, err := uuid.Parse(string(portal.ID))
	if err != nil {
		return nil, fmt.Errorf("invalid conversation ID %q: %w", portal.ID, err)
	}
	members, err := c.api.GetConversationMembers(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("get members for %s: %w", portal.ID, err)
	}

	var chatMembers []bridgev2.ChatMember

	// Add ourselves.
	chatMembers = append(chatMembers, bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{
			IsFromMe: true,
			// Our own Hermes UUID derived from our phone number.
			Sender: ghostIDFromHermesID(gm.PhoneToHermesUserID(c.phone)),
		},
		Membership: event.MembershipJoin,
		PowerLevel: ptrInt(50),
	})

	// Add remote members. UserInfoModel.Address is the phone number.
	for _, m := range members {
		addr := derefStr(m.Address)
		if addr == c.phone {
			continue // skip ourselves
		}
		hermesID := gm.PhoneToHermesUserID(addr)
		chatMembers = append(chatMembers, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{
				Sender: ghostIDFromHermesID(hermesID),
			},
			Membership: event.MembershipJoin,
			PowerLevel: ptrInt(50),
		})
	}

	// Collect recipient phone numbers (everyone except ourselves).
	// These are required for sending messages from Matrix to Garmin,
	// because the Garmin API sends by phone number, not conversation ID.
	var recipientPhones []string
	for _, m := range members {
		addr := derefStr(m.Address)
		if addr != "" && addr != c.phone {
			recipientPhones = append(recipientPhones, addr)
		}
	}

	info := &bridgev2.ChatInfo{
		Members: &bridgev2.ChatMemberList{
			IsFull:  true,
			Members: chatMembers,
		},
		ExtraUpdates: func(_ context.Context, portal *bridgev2.Portal) bool {
			meta, ok := portal.Metadata.(*PortalMetadata)
			if !ok {
				meta = &PortalMetadata{}
				portal.Metadata = meta
			}
			if !slicesEqual(meta.RecipientPhones, recipientPhones) {
				meta.RecipientPhones = recipientPhones
				return true // metadata changed
			}
			return false
		},
	}

	// Group chats (>2 members) get a comma-separated name.
	if len(members) > 2 {
		name := buildGroupName(members, c.phone)
		info.Name = &name
	}

	return info, nil
}

// GetUserInfo returns ghost profile data (displayname, identifiers).
// The ghost ID is the Hermes UUID, but we want to show a human-friendly name.
func (c *GarminClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	// ghost.ID is the Hermes UUID. We don't have a reverse-lookup from UUID
	// to phone without a conversation context, so we use the UUID as the name
	// unless we can find it in a conversation's member list.
	//
	// Attempt a lookup by checking active conversations (best-effort).
	if phone, name := c.lookupPhoneFromUUID(ctx, string(ghost.ID)); phone != "" {
		identifers := []string{"tel:" + phone}
		displayName := name
		if displayName == "" {
			displayName = phone
		}
		return &bridgev2.UserInfo{
			Identifiers: identifers,
			Name:        ptrStr(displayName),
		}, nil
	}

	// Fallback: use the Hermes UUID itself as the display name.
	id := string(ghost.ID)
	return &bridgev2.UserInfo{
		Name: ptrStr(id),
	}, nil
}

// IsThisUser checks whether a ghost ID belongs to the logged-in user.
func (c *GarminClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return ghostIDFromHermesID(gm.PhoneToHermesUserID(c.phone)) == userID
}

// ─── Matrix → Garmin ─────────────────────────────────────────────────────────

// HandleMatrixMessage sends a Matrix message (text or media) to Garmin.
// The Garmin API sends by phone number, so we read recipients from PortalMetadata.
// Media is transcoded via ffmpeg: images → AVIF, audio → OGG.
func (c *GarminClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	meta, ok := msg.Portal.Metadata.(*PortalMetadata)
	if !ok || len(meta.RecipientPhones) == 0 {
		return nil, fmt.Errorf("portal has no recipient phone numbers — cannot send")
	}

	var result *gm.SendMessageV2Response
	var err error

	switch msg.Content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		result, err = c.api.SendMessage(ctx, meta.RecipientPhones, msg.Content.Body)

	case event.MsgImage, event.MsgAudio, event.MsgFile:
		result, err = c.sendMedia(ctx, msg, meta.RecipientPhones)

	default:
		return nil, fmt.Errorf("unsupported message type: %s", msg.Content.MsgType)
	}

	if err != nil {
		return nil, fmt.Errorf("send to garmin: %w", err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:       networkid.MessageID(result.MessageID.String()),
			SenderID: ghostIDFromHermesID(gm.PhoneToHermesUserID(c.phone)),
		},
	}, nil
}

// sendMedia downloads the Matrix media, transcodes it to AVIF/OGG, and sends
// it to Garmin using api.SendMediaMessage.
func (c *GarminClient) sendMedia(ctx context.Context, msg *bridgev2.MatrixMessage, recipients []string) (*gm.SendMessageV2Response, error) {
	// Download from Matrix media repo via the bridge bot's Matrix client.
	data, err := msg.Portal.Bridge.Bot.DownloadMedia(ctx, msg.Content.URL, msg.Content.File)
	if err != nil {
		return nil, fmt.Errorf("download Matrix media: %w", err)
	}

	srcMime := msg.Content.GetInfo().MimeType

	// Determine target Garmin media type and transcode.
	var transcoded []byte
	var gmMediaType gm.MediaType

	switch {
	case isImageMIME(srcMime):
		gmMediaType = gm.MediaTypeImageAvif
		transcoded, err = ToGarminAVIF(ctx, data, srcMime)
		if err != nil {
			return nil, fmt.Errorf("transcode to AVIF: %w", err)
		}
	case isAudioMIME(srcMime):
		gmMediaType = gm.MediaTypeAudioOgg
		transcoded, err = ToGarminOGG(ctx, data, srcMime)
		if err != nil {
			return nil, fmt.Errorf("transcode to OGG: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported media MIME type for Garmin: %s", srcMime)
	}

	// Build extra options (e.g. duration for audio).
	var extraOpts []gm.SendMessageOption
	if gmMediaType == gm.MediaTypeAudioOgg {
		if durationMS := ProbeAudioDurationMS(ctx, transcoded, "ogg"); durationMS > 0 {
			extraOpts = append(extraOpts, gm.WithMediaMetadata(gm.MediaMetadata{DurationMs: &durationMS}))
			c.log.Debug().Int("durationMs", durationMS).Msg("Probed OGG duration for Garmin send")
		}
	}

	// GetCaption() returns non-empty only when Body and FileName differ,
	// i.e. the user actually typed a caption. When no caption is given,
	// Body == FileName (or FileName is empty), so GetCaption() returns "".
	result, err := c.api.SendMediaMessage(ctx, recipients, msg.Content.GetCaption(), transcoded, gmMediaType, extraOpts...)
	if err != nil {
		return nil, fmt.Errorf("SendMediaMessage: %w", err)
	}
	return result, nil
}

// ─── Garmin → Matrix ─────────────────────────────────────────────────────────

// handleIncomingMessage is the sr.OnMessage callback.
// gm.MessageModel uses uuid.UUID for IDs and *time.Time for timestamps.
func (c *GarminClient) handleIncomingMessage(msg gm.MessageModel) {
	if msg.ConversationID == (uuid.UUID{}) {
		c.log.Warn().Msg("Received message with zero ConversationID — ignoring")
		return
	}

	convIDStr := msg.ConversationID.String()
	msgIDStr := msg.MessageID.String()
	senderRaw := derefStr(msg.From)

	// The Garmin API returns the sender as either a phone number (+47...)
	// or a Hermes UUID, depending on the message source. Normalize to a
	// Hermes UUID so ghost IDs are consistent with GetChatInfo (which
	// always derives UUIDs from phone numbers via PhoneToHermesUserID).
	senderUUID := normalizeSenderID(senderRaw)

	c.userLogin.Bridge.QueueRemoteEvent(c.userLogin, &simplevent.Message[gm.MessageModel]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(ctx zerolog.Context) zerolog.Context {
				return ctx.
					Str("garmin_msg_id", msgIDStr).
					Str("conversation_id", convIDStr).
					Str("sender_raw", senderRaw).
					Str("sender_uuid", senderUUID)
			},
			PortalKey: networkid.PortalKey{
				ID:       portalIDFromConversation(convIDStr),
				Receiver: c.userLogin.ID,
			},
			CreatePortal: true,
			Sender: bridgev2.EventSender{
				Sender: ghostIDFromHermesID(senderUUID),
				// IsFromMe is true if the sender UUID matches our own.
				IsFromMe: senderUUID == gm.PhoneToHermesUserID(c.phone),
			},
			Timestamp: derefTime(msg.SentAt),
		},
		Data:               msg,
		ID:                 networkid.MessageID(msgIDStr),
		ConvertMessageFunc: c.convertMessage,
	})

	// Mark as delivered via SignalR (real-time, preferred over REST).
	c.sr.MarkAsDelivered(msg.ConversationID, msg.MessageID)
}

// PreHandleMatrixReaction is called first to identify the reaction for deduplication.
func (c *GarminClient) PreHandleMatrixReaction(_ context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID:     ghostIDFromHermesID(gm.PhoneToHermesUserID(c.phone)),
		Emoji:        msg.Content.RelatesTo.Key,
		MaxReactions: 1,
	}, nil
}

// HandleMatrixReaction sends a Matrix reaction to Garmin as a plain text message.
// The emoji is sent as the message body; Garmin does not have a native reaction API.
func (c *GarminClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	meta, ok := msg.Portal.Metadata.(*PortalMetadata)
	if !ok || len(meta.RecipientPhones) == 0 {
		return nil, fmt.Errorf("portal has no recipient phone numbers — cannot send reaction")
	}
	if _, err := c.api.SendMessage(ctx, meta.RecipientPhones, msg.Content.RelatesTo.Key); err != nil {
		return nil, fmt.Errorf("send reaction to Garmin: %w", err)
	}
	return &database.Reaction{}, nil
}

// HandleMatrixReactionRemove is called when a reaction is redacted on Matrix.
// Garmin has no reaction removal API, so we silently ignore this.
func (c *GarminClient) HandleMatrixReactionRemove(_ context.Context, _ *bridgev2.MatrixReactionRemove) error {
	return nil
}

// handleStatusUpdate is the sr.OnStatusUpdate callback.
func (c *GarminClient) handleStatusUpdate(upd gm.MessageStatusUpdate) {
	if upd.MessageStatus == nil {
		return
	}
	msgStatus := *upd.MessageStatus
	if msgStatus != gm.MessageStatusRead && msgStatus != gm.MessageStatusDelivered {
		return
	}

	convIDStr := upd.MessageID.ConversationID.String()
	msgIDStr := upd.MessageID.MessageID.String()

	c.userLogin.Bridge.QueueRemoteEvent(c.userLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventReadReceipt,
			PortalKey: networkid.PortalKey{
				ID:       portalIDFromConversation(convIDStr),
				Receiver: c.userLogin.ID,
			},
		},
		LastTarget: networkid.MessageID(msgIDStr),
	})
}

// ─── IdentifierResolvingNetworkAPI ───────────────────────────────────────────

// ResolveIdentifier searches existing conversations for a member matching
// the given phone number. Enables the `start-chat` bot command.
func (c *GarminClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	convs, err := c.api.GetConversations(ctx, gm.WithLimit(100))
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}

	normalized := normalizePhone(identifier)
	targetUUID := gm.PhoneToHermesUserID("+" + normalized)

	for _, conv := range convs.Conversations {
		for _, memberUUID := range conv.MemberIDs {
			if strings.ToLower(memberUUID) != targetUUID {
				continue
			}
			// Found a matching conversation.
			convIDStr := conv.ConversationID.String()
			portalKey := networkid.PortalKey{
				ID:       portalIDFromConversation(convIDStr),
				Receiver: c.userLogin.ID,
			}
			ghost, err := c.userLogin.Bridge.GetGhostByID(ctx, ghostIDFromHermesID(memberUUID))
			if err != nil {
				return nil, fmt.Errorf("get ghost: %w", err)
			}
			portal, err := c.userLogin.Bridge.GetPortalByKey(ctx, portalKey)
			if err != nil {
				return nil, fmt.Errorf("get portal: %w", err)
			}
			ghostInfo, _ := c.GetUserInfo(ctx, ghost)
			portalInfo, _ := c.GetChatInfo(ctx, portal)
			return &bridgev2.ResolveIdentifierResponse{
				Ghost:    ghost,
				UserID:   ghostIDFromHermesID(memberUUID),
				UserInfo: ghostInfo,
				Chat: &bridgev2.CreateChatResponse{
					Portal:     portal,
					PortalKey:  portalKey,
					PortalInfo: portalInfo,
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("no Garmin Messenger conversation found with %s", identifier)
}

// ─── Internal helpers ────────────────────────────────────────────────────────

// lookupPhoneFromUUID scans conversations to find a member matching hermesUUID.
// Returns (phone, displayName) or ("", "") if not found.
func (c *GarminClient) lookupPhoneFromUUID(ctx context.Context, hermesUUID string) (string, string) {
	convs, err := c.api.GetConversations(ctx, gm.WithLimit(50))
	if err != nil {
		return "", ""
	}
	for _, conv := range convs.Conversations {
		members, err := c.api.GetConversationMembers(ctx, conv.ConversationID)
		if err != nil {
			continue
		}
		for _, m := range members {
			addr := derefStr(m.Address)
			if addr == "" {
				continue
			}
			if gm.PhoneToHermesUserID(addr) == strings.ToLower(hermesUUID) {
				return addr, derefStr(m.FriendlyName)
			}
		}
	}
	return "", ""
}

// buildGroupName builds a display name for a group conversation.
func buildGroupName(members []gm.UserInfoModel, myPhone string) string {
	name := ""
	for _, m := range members {
		addr := derefStr(m.Address)
		if addr == myPhone {
			continue
		}
		if name != "" {
			name += ", "
		}
		if n := derefStr(m.FriendlyName); n != "" {
			name += n
		} else {
			name += addr
		}
	}
	return name
}

// derefTime safely dereferences a *time.Time, falling back to time.Now().
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Now()
	}
	return *t
}

// derefStr safely dereferences a *string pointer.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrInt(v int) *int       { return &v }
func ptrStr(v string) *string { return &v }

// normalizePhone strips non-digit characters for comparison.
func normalizePhone(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			out = append(out, byte(r))
		}
	}
	return string(out)
}

// normalizeSenderID ensures a sender identifier from the Garmin API is always
// a Hermes UUID. The API may return either a phone number (+47...) or a UUID.
// Phone numbers are converted to UUIDs via PhoneToHermesUserID; UUIDs are
// lowercased for consistent matching.
func normalizeSenderID(raw string) string {
	if strings.HasPrefix(raw, "+") {
		return gm.PhoneToHermesUserID(raw)
	}
	return strings.ToLower(raw)
}

// slicesEqual compares two string slices for equality.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// removeFile removes a file, ignoring "not found" errors.
func removeFile(path string) error {
	err := removeFileImpl(path)
	if err != nil && !isNotExist(err) {
		return err
	}
	return nil
}
