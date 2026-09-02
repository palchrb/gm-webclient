package garminmessenger

import "regexp"

// Debug logging dumps raw request/response bodies, which is invaluable when
// Garmin's protocol drifts — but those bodies carry bearer tokens, refresh
// tokens and the SMS verification code. redactSecrets blanks the values of
// the known secret-bearing fields and leaves everything else (message
// payloads, IDs, status fields) intact.
var (
	secretJSONKeyRe = regexp.MustCompile(`(?i)("(?:accessToken|refreshToken|verificationCode|securityToken|pnsHandle|access_token|refresh_token|token)"\s*:\s*")[^"]*(")`)
	authSchemeRe    = regexp.MustCompile(`(?i)\b(Bearer|AidLogin)\s+[^\s",]+`)
	tokenParamRe    = regexp.MustCompile(`(?i)\btoken=[^&\s]+`)
)

func redactSecrets(s string) string {
	s = secretJSONKeyRe.ReplaceAllString(s, `${1}***${2}`)
	s = authSchemeRe.ReplaceAllString(s, `${1} ***`)
	s = tokenParamRe.ReplaceAllString(s, `token=***`)
	return s
}
