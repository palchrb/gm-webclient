package connector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

func TestShouldBridgeAsMedia(t *testing.T) {
	mk := func(msgType event.MessageType, mime string, withURL bool) *bridgev2.MatrixMessage {
		content := &event.MessageEventContent{MsgType: msgType}
		if mime != "" {
			content.Info = &event.FileInfo{MimeType: mime}
		}
		if withURL {
			content.URL = "mxc://example.org/file"
		}
		return &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Content: content}}
	}

	assert.True(t, shouldBridgeAsMedia(mk(event.MsgImage, "image/png", false)))
	assert.True(t, shouldBridgeAsMedia(mk(event.MessageType("m.whatever"), "image/png", false)))
	assert.True(t, shouldBridgeAsMedia(mk(event.MessageType("m.whatever"), "", true)))
	assert.False(t, shouldBridgeAsMedia(mk(event.MsgText, "", false)))
}

func TestDetectMIMEFromContent(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	assert.Equal(t, "image/png", detectMIMEFromContent(event.MsgImage, png))

	unknown := []byte{0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, "image/jpeg", detectMIMEFromContent(event.MsgImage, unknown))
	assert.Equal(t, "audio/ogg", detectMIMEFromContent(event.MsgAudio, unknown))
	assert.Equal(t, "application/octet-stream", detectMIMEFromContent(event.MsgVideo, unknown))
	assert.Equal(t, "image/jpeg", detectMIMEFromContent(msgTypeSticker, unknown))
}
