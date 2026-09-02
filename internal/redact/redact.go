// Package redact blanks credentials and personal identifiers from strings
// that are about to be logged. Debug logging dumps raw request/response
// bodies, which is invaluable when Garmin's protocol drifts, but those
// bodies carry bearer tokens, refresh tokens, the SMS verification code,
// FCM device secrets and the user's phone number. Secrets() removes the
// values of the known secret-bearing fields and masks phone numbers,
// leaving everything else (message payloads, IDs, status fields) intact.
package redact

import "regexp"

var (
	// JSON string values under keys that hold credentials → "***".
	secretJSONKeyRe = regexp.MustCompile(`(?i)("(?:accessToken|refreshToken|verificationCode|securityToken|pnsHandle|access_token|refresh_token|token)"\s*:\s*")[^"]*(")`)
	// JSON string values under keys that hold a phone number → masked.
	phoneJSONKeyRe = regexp.MustCompile(`(?i)("(?:smsNumber|phoneNumber|phone_number|phone|sender)"\s*:\s*")([^"]*)(")`)
	// HTTP auth schemes whose credential follows on the same line.
	authSchemeRe = regexp.MustCompile(`(?i)\b(Bearer|AidLogin)\s+[^\s",]+`)
	// Form/query encoded registration tokens (GCM "token=..." responses).
	tokenParamRe = regexp.MustCompile(`(?i)\btoken=[^&\s]+`)
	// Captures the phone value from a phoneJSONKeyRe match.
	phoneValueRe = regexp.MustCompile(`(?i)(":\s*")([^"]*)(")$`)
)

// Secrets returns s with credentials blanked and phone numbers masked.
func Secrets(s string) string {
	s = secretJSONKeyRe.ReplaceAllString(s, `${1}***${2}`)
	s = phoneJSONKeyRe.ReplaceAllStringFunc(s, func(m string) string {
		return phoneValueRe.ReplaceAllStringFunc(m, func(v string) string {
			sub := phoneValueRe.FindStringSubmatch(v)
			return sub[1] + Phone(sub[2]) + sub[3]
		})
	})
	s = authSchemeRe.ReplaceAllString(s, `${1} ***`)
	s = tokenParamRe.ReplaceAllString(s, `token=***`)
	return s
}

// Phone masks the middle of a phone number: "+4712345678" → "+47…5678".
// Values too short to mask meaningfully are returned unchanged.
func Phone(p string) string {
	if len(p) <= 8 {
		return p
	}
	return p[:3] + "…" + p[len(p)-4:]
}
