module github.com/yourusername/matrix-garmin-messenger

go 1.24

require (
	github.com/rs/zerolog v1.33.0
	github.com/slush-dev/garmin-messenger v1.2.7
	maunium.net/go/mautrix v0.21.0
)

// Replace with the actual version tag or commit hash once published.
// Until published, use:
//   go get github.com/slush-dev/garmin-messenger@main
// and remove this replace directive.
// replace github.com/slush-dev/garmin-messenger => ../garmin-messenger/lib/go
