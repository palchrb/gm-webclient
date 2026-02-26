package connector

import (
	"context"
	"fmt"

	gm "github.com/slush-dev/garmin-messenger"
	"maunium.net/go/mautrix/bridgev2"
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

	// --- Text body ---
	if body != "" {
		bodyText := body

		// InReach devices often send a location alongside the text.
		if msg.Location != nil {
			lat := derefFloat64(msg.Location.LatitudeDegrees)
			lon := derefFloat64(msg.Location.LongitudeDegrees)
			bodyText += fmt.Sprintf("\n\n📍 Location: %.6f, %.6f", lat, lon)
			if alt := derefFloat64(msg.Location.ElevationMeters); alt != 0 {
				bodyText += fmt.Sprintf(" (%.0fm)", alt)
			}
			if spd := derefFloat64(msg.Location.SpeedKnots); spd != 0 {
				bodyText += fmt.Sprintf(", %.1f km/h", spd*1.852)
			}
		}

		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    bodyText,
			},
		})
	}

	// --- Location-only message (pure GPS ping from InReach, no text) ---
	if body == "" && msg.Location != nil {
		lat := derefFloat64(msg.Location.LatitudeDegrees)
		lon := derefFloat64(msg.Location.LongitudeDegrees)
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgLocation,
				Body:    fmt.Sprintf("Location: %.6f, %.6f", lat, lon),
				GeoURI:  fmt.Sprintf("geo:%.6f,%.6f", lat, lon),
			},
		})
	}

	// --- Media attachment (AVIF image or OGG audio from Garmin) ---
	// Download from Garmin, transcode if needed, reupload to Matrix.
	if derefStr(msg.MediaID) != "" && len(parts) == 0 {
		mediaPart, err := c.bridgeIncomingMedia(ctx, intent, msg)
		if err != nil {
			// Don't drop the message — fall back to a notice.
			c.log.Warn().Err(err).Str("msg_id", msg.MessageID).Msg("Failed to bridge media")
			parts = append(parts, &bridgev2.ConvertedMessagePart{
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgNotice,
					Body:    fmt.Sprintf("[Media attachment (%s) — could not be downloaded]", derefStr(msg.MediaType)),
				},
			})
		} else {
			parts = append(parts, mediaPart)
		}
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
//
// Garmin sends AVIF images and OGG audio.
// We convert AVIF → JPEG because most Matrix clients don't support AVIF.
// OGG is kept as-is since it's widely supported.
func (c *GarminClient) bridgeIncomingMedia(
	ctx context.Context,
	intent bridgev2.MatrixAPI,
	msg gm.MessageModel,
) (*bridgev2.ConvertedMessagePart, error) {
	mediaID := derefStr(msg.MediaID)
	mediaType := gm.MediaType(derefStr(msg.MediaType))

	// Download from Garmin using the REST API.
	data, err := c.api.DownloadMedia(
		ctx,
		derefStr(msg.UUID),
		mediaID,
		msg.MessageID,
		msg.ConversationID,
		mediaType,
	)
	if err != nil {
		return nil, fmt.Errorf("DownloadMedia: %w", err)
	}

	// Transcode and determine Matrix event type.
	var uploadData []byte
	var mxMsgType event.MessageType
	var mimeType string
	var filename string

	switch mediaType {
	case gm.MediaTypeImageAvif:
		// AVIF → JPEG for broad client compatibility.
		uploadData, err = FromAVIF(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("AVIF→JPEG: %w", err)
		}
		mxMsgType = event.MsgImage
		mimeType = "image/jpeg"
		filename = "image.jpg"

	case gm.MediaTypeAudioOgg:
		// OGG is already well-supported in Matrix clients.
		uploadData = data
		mxMsgType = event.MsgAudio
		mimeType = "audio/ogg"
		filename = "voice.ogg"

	default:
		return nil, fmt.Errorf("unknown Garmin media type: %s", mediaType)
	}

	// Upload to Matrix media repository via the ghost user's intent.
	mxcURI, encryptedFile, err := intent.UploadMedia(ctx, "", uploadData, filename, mimeType)
	if err != nil {
		return nil, fmt.Errorf("UploadMedia: %w", err)
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

	return &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: content,
	}, nil
}

// derefFloat64 safely dereferences a *float64.
func derefFloat64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
