package fcm

import "github.com/yourusername/matrix-garmin-messenger/internal/redact"

// redactSecrets is applied to headers and bodies logged by the debug
// round-tripper. See internal/redact for what is blanked.
func redactSecrets(s string) string { return redact.Secrets(s) }
