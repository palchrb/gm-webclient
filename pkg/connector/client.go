package connector

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
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

const msgTypeSticker event.MessageType = "m.sticker"

func newGarminClient(gc *GarminConnector, login *bridgev2.UserLogin, auth *gm.HermesAuth, phone string) *GarminClient {
	api := gm.NewHermesAPI(auth)
	sr := gm.NewHermesSignalR(auth)
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
	return &event.RoomFeatures{
		MaxTextLength: 160,
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

	portalAvatarURL := chooseAvatarURL(members, c.phone)

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

	if portalAvatarURL != "" {
		info.Avatar = c.avatarFromURL(portalAvatarURL)
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
	if phone, name, avatarURL := c.lookupPhoneFromUUID(ctx, string(ghost.ID)); phone != "" {
		identifers := []string{"tel:" + phone}
		displayName := name
		if displayName == "" {
			displayName = phone
		}
		info := &bridgev2.UserInfo{
			Identifiers: identifers,
			Name:        ptrStr(displayName),
		}
		if avatarURL != "" {
			info.Avatar = c.avatarFromURL(avatarURL)
		}
		return info, nil
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

	case event.MsgImage, event.MsgAudio, event.MsgFile, msgTypeSticker:
		result, err = c.sendMedia(ctx, msg, meta.RecipientPhones)

	default:
		if shouldBridgeAsMedia(msg) {
			result, err = c.sendMedia(ctx, msg, meta.RecipientPhones)
			break
		}
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
	if srcMime == "" {
		srcMime = detectMIMEFromContent(msg.Content.MsgType, data)
	}
	garminMediaType := GarminMediaType(srcMime)
	if garminMediaType == "" {
		return nil, fmt.Errorf("unsupported media MIME type for Garmin: %s (msgtype=%s)", srcMime, msg.Content.MsgType)
	}

	// Transcode if necessary.
	var transcoded []byte
	switch garminMediaType {
	case gm.MediaTypeImageAvif:
		if srcMime == "image/avif" {
			transcoded = data // already correct format
		} else {
			transcoded, err = ToAVIF(ctx, data, srcMime)
			if err != nil {
				return nil, fmt.Errorf("transcode to AVIF: %w", err)
			}
		}
	case gm.MediaTypeAudioOgg:
		if srcMime == "audio/ogg" {
			transcoded = data
		} else {
			transcoded, err = ToOGG(ctx, data, srcMime)
			if err != nil {
				return nil, fmt.Errorf("transcode to OGG: %w", err)
			}
		}
	}

	result, err := c.api.SendMediaMessage(
		ctx,
		recipients,
		msg.Content.Body,
		transcoded,
		garminMediaType,
	)
	if err != nil {
		return nil, fmt.Errorf("SendMediaMessage: %w", err)
	}
	return result, nil
}

func shouldBridgeAsMedia(msg *bridgev2.MatrixMessage) bool {
	if msg == nil || msg.Content == nil {
		return false
	}
	if msg.Content.URL != "" || msg.Content.File != nil {
		return true
	}
	mime := msg.Content.GetInfo().MimeType
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/") {
		return true
	}
	return false
}

func detectMIMEFromContent(msgType event.MessageType, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	mime := http.DetectContentType(data)
	// Common fallback values from DetectContentType are too generic.
	if mime == "application/octet-stream" {
		switch msgType {
		case event.MsgImage, msgTypeSticker:
			return "image/jpeg"
		case event.MsgAudio:
			return "audio/ogg"
		}
	}
	return mime
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
func (c *GarminClient) lookupPhoneFromUUID(ctx context.Context, hermesUUID string) (string, string, string) {
	convs, err := c.api.GetConversations(ctx, gm.WithLimit(50))
	if err != nil {
		return "", "", ""
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
				return addr, derefStr(m.FriendlyName), derefStr(m.ImageUrl)
			}
		}
	}
	return "", "", ""
}

// chooseAvatarURL picks a remote participant avatar URL for a portal room.
func chooseAvatarURL(members []gm.UserInfoModel, myPhone string) string {
	for _, m := range members {
		addr := derefStr(m.Address)
		if addr == "" || addr == myPhone {
			continue
		}
		if img := derefStr(m.ImageUrl); img != "" {
			return img
		}
	}
	return ""
}

func (c *GarminClient) avatarFromURL(url string) *bridgev2.Avatar {
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(url),
		Get: func(ctx context.Context) ([]byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, fmt.Errorf("avatar download HTTP %d", resp.StatusCode)
			}
			var buf bytes.Buffer
			if _, err = buf.ReadFrom(resp.Body); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		},
	}
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
