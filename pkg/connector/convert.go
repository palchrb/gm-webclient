package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// convertMessage converts a gm.MessageModel into Matrix events.
// Called by the simplevent.Message handler inside the bridge event loop.
// All fields on gm.MessageModel are pointers — use derefStr()/derefFloat64().
func (c *GarminClient) convertMessage(
	ctx context.Context,
	_ *bridgev2.Portal,
	intent bridgev2.MatrixAPI,
	msg gm.MessageModel,
) (*bridgev2.ConvertedMessage, error) {
	var parts []*bridgev2.ConvertedMessagePart

	body := derefStr(msg.MessageBody)
	// Strip invisible delimiter characters Garmin uses in reaction message bodies
	// (\u200b zero-width space, \u200a hair space, \u2009 thin space).
	body = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200a', '\u2009':
			return -1
		}
		return r
	}, body)

	bodyText := body
	if msg.UserLocation != nil {
		lat := derefFloat64(msg.UserLocation.LatitudeDegrees)
		lon := derefFloat64(msg.UserLocation.LongitudeDegrees)
		if bodyText == "" {
			// Keep this as plain text to avoid large map previews in Matrix clients.
			bodyText = fmt.Sprintf("📍 %.6f, %.6f", lat, lon)
		} else {
			bodyText += fmt.Sprintf("\n\n📍 Location: %.6f, %.6f", lat, lon)
		}
		if alt := derefFloat64(msg.UserLocation.ElevationMeters); alt != 0 {
			bodyText += fmt.Sprintf(" (%.0fm)", alt)
		}
		// GroundVelocityMetersPerSecond * 3.6 = km/h
		if spd := derefFloat64(msg.UserLocation.GroundVelocityMetersPerSecond); spd != 0 {
			bodyText += fmt.Sprintf(", %.1f km/h", spd*3.6)
		}
	}

	// --- Media attachment (AVIF image or OGG audio from Garmin) ---
	// Download from Garmin, transcode if needed, reupload to Matrix.
	// Note: process media even when there's also a location/text body.
	if msg.MediaID != nil {
		mediaPart, transcription, err := c.bridgeIncomingMedia(ctx, intent, msg)
		if err != nil {
			// Don't drop the message. If we have text/location content, include the
			// media failure as a suffix to keep one Matrix part and avoid duplicate
			// DB writes with empty part IDs.
			mediaTypeStr := ""
			if msg.MediaType != nil {
				mediaTypeStr = string(*msg.MediaType)
			}
			c.log.Warn().Err(err).Str("msg_id", msg.MessageID.String()).Msg("Failed to bridge media")
			if bodyText != "" {
				parts = append(parts, &bridgev2.ConvertedMessagePart{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgText,
						Body:    bodyText + fmt.Sprintf("\n\n[Media attachment (%s) — could not be downloaded]", mediaTypeStr),
					},
				})
			} else {
				parts = append(parts, &bridgev2.ConvertedMessagePart{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgNotice,
						Body:    fmt.Sprintf("[Media attachment (%s) — could not be downloaded]", mediaTypeStr),
					},
				})
			}
		} else {
			// Transcription comes from the REST detail response (SignalR omits it).
			// Prefer REST-sourced transcription; fall back to SignalR if somehow present.
			effectiveTrans := transcription
			if effectiveTrans == nil {
				effectiveTrans = msg.Transcription
			}
			if effectiveTrans != nil {
				if t := strings.TrimSpace(*effectiveTrans); t != "" {
					if bodyText != "" {
						bodyText += "\n" + t
					} else {
						bodyText = t
					}
				}
			}
			if bodyText != "" {
				// Save the filename before overwriting body with the caption/transcription.
				mediaPart.Content.FileName = mediaPart.Content.Body
				mediaPart.Content.Body = bodyText
			}
			parts = append(parts, mediaPart)
		}
	}

	// --- Text fallback when there's no media attachment ---
	if len(parts) == 0 && bodyText != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    bodyText,
			},
		})
	}

	// Fallback: don't silently drop messages.
	if len(parts) == 0 {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    "[Empty Garmin Messenger message]",
			},
		})
	}

	return &bridgev2.ConvertedMessage{Parts: parts}, nil
}

// bridgeIncomingMedia downloads a Garmin media attachment, transcodes it to a
// Matrix-friendly format, and reuploads it to the Matrix media repository.
// It also returns any transcription found in the REST detail response.
//
// Garmin sends AVIF images and OGG audio.
// Keep AVIF as-is (no conversion) and keep OGG as-is.
func (c *GarminClient) bridgeIncomingMedia(
	ctx context.Context,
	intent bridgev2.MatrixAPI,
	msg gm.MessageModel,
) (*bridgev2.ConvertedMessagePart, *string, error) {
	if msg.MediaID == nil {
		return nil, nil, fmt.Errorf("message has no media ID")
	}
	if msg.MediaType == nil {
		return nil, nil, fmt.Errorf("message has no media type")
	}

	msgUUID, transcription, err := c.resolveMediaMessageDetails(ctx, msg)
	if err != nil {
		return nil, nil, err
	}

	mediaID := *msg.MediaID
	mediaType := *msg.MediaType

	// Transcription only applies to audio; clear it for other media types.
	if mediaType != gm.MediaTypeAudioOgg {
		transcription = nil
	}

	// Download from Garmin using the REST API.
	data, err := c.api.DownloadMedia(
		ctx,
		msgUUID,
		mediaID,
		msg.MessageID,
		msg.ConversationID,
		mediaType,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("DownloadMedia: %w", err)
	}

	// Determine Matrix event type and MIME.
	var uploadData []byte
	var mxMsgType event.MessageType
	var mimeType string
	var filename string

	switch mediaType {
	case gm.MediaTypeImageAvif:
		uploadData = data
		mxMsgType = event.MsgImage
		mimeType = "image/avif"
		filename = "image.avif"

	case gm.MediaTypeAudioOgg:
		uploadData = data
		mxMsgType = event.MsgAudio
		mimeType = "audio/ogg"
		filename = "voice.ogg"

	default:
		return nil, nil, fmt.Errorf("unknown Garmin media type: %s", mediaType)
	}

	// Upload to Matrix media repository via the ghost user's intent.
	mxcURI, encryptedFile, err := intent.UploadMedia(ctx, "", uploadData, filename, mimeType)
	if err != nil {
		return nil, nil, fmt.Errorf("UploadMedia: %w", err)
	}

	content := &event.MessageEventContent{
		MsgType: mxMsgType,
		Body:    filename,
		URL:     mxcURI,
		File:    encryptedFile,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(uploadData),
		},
	}

	if mxMsgType == event.MsgAudio {
		durationMS := ProbeAudioDurationMS(ctx, uploadData, "ogg")
		if durationMS > 0 {
			content.Info.Duration = durationMS
		}
		content.MSC3245Voice = &event.MSC3245Voice{}
		content.MSC1767Audio = &event.MSC1767Audio{
			Duration: durationMS,
			Waveform: []int{},
		}
	}

	return &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: content,
	}, transcription, nil
}

// resolveMediaMessageDetails finds the UUID and transcription required for
// media message handling.
//
// SignalR events may omit MessageModel.UUID and MessageModel.Transcription.
// In that case, fetch recent conversation details and find the matching entry.
// As a final fallback, use MessageID as UUID (best-effort).
func (c *GarminClient) resolveMediaMessageDetails(ctx context.Context, msg gm.MessageModel) (uuid.UUID, *string, error) {
	if msg.UUID != nil {
		return *msg.UUID, msg.Transcription, nil
	}

	detail, err := c.api.GetConversationDetail(ctx, msg.ConversationID, gm.WithDetailLimit(100))
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("message has no UUID and lookup failed: %w", err)
	}

	for _, m := range detail.Messages {
		if m.MessageID != msg.MessageID {
			continue
		}
		if m.UUID != nil {
			return *m.UUID, m.Transcription, nil
		}
		break
	}

	c.log.Warn().
		Str("msg_id", msg.MessageID.String()).
		Str("conversation_id", msg.ConversationID.String()).
		Msg("Media message UUID not present in detail response, falling back to MessageID")
	return msg.MessageID, nil, nil
}

// isReactionBody reports whether a Garmin message body is a reaction.
// Garmin encodes reactions as: \u200b{emoji}\u200b to \u200a{quoted}\u200a
func isReactionBody(s string) bool {
	return strings.HasPrefix(s, "\u200b")
}

// extractReactionEmoji returns the emoji from a Garmin reaction body.
func extractReactionEmoji(s string) string {
	s = strings.TrimPrefix(s, "\u200b")
	if idx := strings.Index(s, "\u200b"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// buildReactionBody constructs a Garmin-compatible reaction message body.
// Format mirrors what the native app sends: \u200b{emoji}\u200b to \u200a{original}\u200a
func buildReactionBody(emoji, originalBody string) string {
	return "\u200b" + emoji + "\u200b to \u200a" + originalBody + "\u200a"
}

// resolveReactionParentID fetches the parentMessageId for a reaction message via REST.
// SignalR pushes never include parentMessageId; the server populates it based on the
// quoted body text, and it's only available in the conversation detail response.
func (c *GarminClient) resolveReactionParentID(ctx context.Context, msg gm.MessageModel) (networkid.MessageID, error) {
	if msg.ParentMessageID != nil {
		return networkid.MessageID(msg.ParentMessageID.String()), nil
	}
	detail, err := c.api.GetConversationDetail(ctx, msg.ConversationID, gm.WithDetailLimit(100))
	if err != nil {
		return "", fmt.Errorf("reaction parent lookup failed: %w", err)
	}
	for _, m := range detail.Messages {
		if m.MessageID == msg.MessageID {
			if m.ParentMessageID != nil {
				return networkid.MessageID(m.ParentMessageID.String()), nil
			}
			break
		}
	}
	return "", fmt.Errorf("parentMessageId not found for reaction %s", msg.MessageID)
}

// resolveReactionOriginalBody fetches the message body of the target message for
// use when constructing an outgoing reaction from Matrix to Garmin.
func (c *GarminClient) resolveReactionOriginalBody(ctx context.Context, conversationID uuid.UUID, targetMsgID uuid.UUID) (string, error) {
	detail, err := c.api.GetConversationDetail(ctx, conversationID, gm.WithDetailLimit(100))
	if err != nil {
		return "", fmt.Errorf("reaction target lookup failed: %w", err)
	}
	for _, m := range detail.Messages {
		if m.MessageID == targetMsgID {
			return derefStr(m.MessageBody), nil
		}
	}
	return "", fmt.Errorf("target message %s not found in conversation", targetMsgID)
}

// derefFloat64 safely dereferences a *float64.
func derefFloat64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
