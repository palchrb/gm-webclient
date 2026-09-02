package garminmessenger

import "github.com/yourusername/matrix-garmin-messenger/internal/redact"

// redactSecrets is applied to every request/response body before it is
// logged at Debug. See internal/redact for what is blanked.
func redactSecrets(s string) string { return redact.Secrets(s) }
