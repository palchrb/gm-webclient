package redact

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	in := `{"instanceId":"abc","accessAndRefreshToken":{"accessToken":"AAA.BBB","refreshToken":"RRR","expiresIn":3600},` +
		`"verificationCode":"123456","pnsHandle":"fcm-xyz","messageBody":"hello there","token":"t0k",` +
		`"smsNumber":"+4712345678","sender":"+4798765432","securityToken":"sec"}` +
		` Authorization: Bearer eyJhbGci AidLogin 123:456 token=abc123&x=1`
	out := Secrets(in)

	for _, secret := range []string{"AAA.BBB", `"RRR"`, "123456", "fcm-xyz", "eyJhbGci", "123:456", "abc123", `"t0k"`, `"sec"`, "+4712345678", "+4798765432"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	for _, keep := range []string{`"instanceId":"abc"`, `"expiresIn":3600`, `"messageBody":"hello there"`, `&x=1`} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret %q was damaged: %s", keep, out)
		}
	}
	for _, want := range []string{`"accessToken":"***"`, "Bearer ***", "token=***", `"smsNumber":"+47…5678"`, `"sender":"+47…5432"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in: %s", want, out)
		}
	}
}

func TestPhone(t *testing.T) {
	cases := map[string]string{
		"+4712345678":  "+47…5678",
		"+12125550123": "+12…0123",
		"+1234567":     "+1234567",
		"":             "",
	}
	for in, want := range cases {
		if got := Phone(in); got != want {
			t.Errorf("Phone(%q) = %q, want %q", in, got, want)
		}
	}
}
