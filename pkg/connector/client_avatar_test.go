package connector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

func TestChooseAvatarURL(t *testing.T) {
	str := func(s string) *string { return &s }
	members := []gm.UserInfoModel{
		{Address: str("+111"), ImageUrl: str("https://example.com/me.png")},
		{Address: str("+222"), FriendlyName: str("Alice"), ImageUrl: str("https://example.com/alice.png")},
		{Address: str("+333"), FriendlyName: str("Bob")},
	}

	assert.Equal(t, "https://example.com/alice.png", chooseAvatarURL(members, "+111"))
	noImages := []gm.UserInfoModel{{Address: str("+222")}, {Address: str("+333")}}
	assert.Equal(t, "", chooseAvatarURL(noImages, "+111"))
}
