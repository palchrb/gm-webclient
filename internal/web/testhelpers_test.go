package web

import (
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// newFakeAuth returns a HermesAuth with placeholder credentials. Nothing in
// these tests talks to Garmin; the object only needs to look "logged in".
func newFakeAuth() *gm.HermesAuth {
	a := gm.NewHermesAuth(gm.WithLogger(testLogger()))
	a.AccessToken = "access"
	a.RefreshToken = "refresh"
	a.InstanceID = "instance"
	a.ExpiresAt = 4102444800 // 2100-01-01
	return a
}
