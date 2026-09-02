package web

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestCeremony_PopReturnsOnceAndExpires(t *testing.T) {
	phone := "+4711111111"
	data := &webauthn.SessionData{Challenge: "c1"}

	storeCeremony(phone, data)
	if got := popCeremony(phone); got != data {
		t.Fatal("expected stored ceremony back")
	}
	if popCeremony(phone) != nil {
		t.Fatal("ceremony must be single-use")
	}

	// Expired ceremonies are rejected even if still in the map.
	storeCeremony(phone, data)
	pendingCeremoniesMu.Lock()
	c := pendingCeremonies[phone]
	c.createdAt = time.Now().Add(-ceremonyTTL - time.Second)
	pendingCeremonies[phone] = c
	pendingCeremoniesMu.Unlock()
	if popCeremony(phone) != nil {
		t.Fatal("expired ceremony must not be returned")
	}
}

func TestCeremony_StorePrunesExpiredOthers(t *testing.T) {
	stale := "+4722222222"
	storeCeremony(stale, &webauthn.SessionData{Challenge: "old"})
	pendingCeremoniesMu.Lock()
	c := pendingCeremonies[stale]
	c.createdAt = time.Now().Add(-ceremonyTTL - time.Second)
	pendingCeremonies[stale] = c
	pendingCeremoniesMu.Unlock()

	storeCeremony("+4733333333", &webauthn.SessionData{Challenge: "new"})

	pendingCeremoniesMu.Lock()
	_, stillThere := pendingCeremonies[stale]
	pendingCeremoniesMu.Unlock()
	if stillThere {
		t.Fatal("expired ceremony should have been pruned on store")
	}
	popCeremony("+4733333333")
}
