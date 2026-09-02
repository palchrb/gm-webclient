package fcm

import "regexp"

// The debug round-tripper dumps GCM/FCM requests and responses. Those carry
// the device security token (AidLogin header, securityToken fields) and the
// FCM registration token; redactSecrets blanks them and leaves the rest of
// the payload readable for protocol debugging.
var (
	secretJSONKeyRe = regexp.MustCompile(`(?i)("(?:securityToken|token|accessToken|refreshToken)"\s*:\s*")[^"]*(")`)
	authSchemeRe    = regexp.MustCompile(`(?i)\b(Bearer|AidLogin)\s+[^\s",]+`)
	tokenParamRe    = regexp.MustCompile(`(?i)\btoken=[^&\s]+`)
)

func redactSecrets(s string) string {
	s = secretJSONKeyRe.ReplaceAllString(s, `${1}***${2}`)
	s = authSchemeRe.ReplaceAllString(s, `${1} ***`)
	s = tokenParamRe.ReplaceAllString(s, `token=***`)
	return s
}
