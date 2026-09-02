package garminmessenger

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	in := `{"instanceId":"abc","accessAndRefreshToken":{"accessToken":"AAA.BBB","refreshToken":"RRR","expiresIn":3600},` +
		`"verificationCode":"123456","pnsHandle":"fcm-xyz","messageBody":"hello there","token":"t0k"}` +
		` Authorization: Bearer eyJhbGci AidLogin 123:456 token=abc123&x=1`
	out := redactSecrets(in)

	for _, secret := range []string{"AAA.BBB", `"RRR"`, "123456", "fcm-xyz", "eyJhbGci", "123:456", "abc123", `"t0k"`} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	for _, keep := range []string{`"instanceId":"abc"`, `"expiresIn":3600`, `"messageBody":"hello there"`, `&x=1`} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret %q was damaged: %s", keep, out)
		}
	}
	if !strings.Contains(out, `"accessToken":"***"`) || !strings.Contains(out, "Bearer ***") || !strings.Contains(out, "token=***") {
		t.Errorf("expected *** placeholders: %s", out)
	}
}
