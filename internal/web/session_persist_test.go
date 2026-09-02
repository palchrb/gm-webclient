package web

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// encryptRaw seals arbitrary plaintext with the store's key, so tests can
// plant legacy-format files on disk.
func encryptRaw(t *testing.T, ss *SessionStore, plaintext []byte) {
	t.Helper()
	nonce := make([]byte, ss.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatal(err)
	}
	ct := ss.gcm.Seal(nonce, nonce, plaintext, nil)
	if err := os.MkdirAll(ss.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ss.path(), ct, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStore_RoundTripV2(t *testing.T) {
	dir := t.TempDir()
	ss, err := NewSessionStore(dir, "k1", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	last := time.Now().Add(-time.Hour).Truncate(time.Second)
	in := persistedState{
		Accounts: []persistedAccount{{Phone: "+4711111111", AccessToken: "a", RefreshToken: "r", InstanceID: "i", ExpiresAt: 123, NtfyEnabled: true}},
		Sessions: []persistedSession{{SessionID: "s1", Phone: "+4711111111", LastActivity: last}},
	}
	ss.Save(in)

	// File must not be readable as plaintext JSON.
	raw, _ := os.ReadFile(filepath.Join(dir, "sessions.enc"))
	if json.Valid(raw) {
		t.Fatal("sessions.enc is plaintext JSON")
	}

	out := ss.Load()
	if out.Version != 2 {
		t.Fatalf("version = %d, want 2", out.Version)
	}
	if len(out.Accounts) != 1 || out.Accounts[0] != in.Accounts[0] {
		t.Fatalf("accounts mismatch: %+v", out.Accounts)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != "s1" || !out.Sessions[0].LastActivity.Equal(last) {
		t.Fatalf("sessions mismatch: %+v", out.Sessions)
	}
}

func TestSessionStore_WrongKeyYieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	ss1, _ := NewSessionStore(dir, "right", testLogger())
	ss1.Save(persistedState{Accounts: []persistedAccount{{Phone: "+4711111111", InstanceID: "i"}}})
	ss2, _ := NewSessionStore(dir, "wrong", testLogger())
	if out := ss2.Load(); len(out.Accounts) != 0 || len(out.Sessions) != 0 {
		t.Fatalf("expected empty state with wrong key, got %+v", out)
	}
}

func TestSessionStore_LoadsLegacyFlatFormat(t *testing.T) {
	dir := t.TempDir()
	ss, _ := NewSessionStore(dir, "k", testLogger())
	legacy := []legacySession{
		{SessionID: "s1", Phone: "+4711111111", AccessToken: "a1", RefreshToken: "r1", InstanceID: "i1", ExpiresAt: 1, NtfyEnabled: true},
		{SessionID: "s2", Phone: "+4711111111", AccessToken: "a1", RefreshToken: "r1", InstanceID: "i1", ExpiresAt: 1},
		{SessionID: "s3", Phone: "+4722222222", AccessToken: "a2", RefreshToken: "r2", InstanceID: "i2", ExpiresAt: 2},
	}
	b, _ := json.Marshal(legacy)
	encryptRaw(t, ss, b)

	out := ss.Load()
	if len(out.Accounts) != 2 {
		t.Fatalf("expected 2 deduplicated accounts, got %d", len(out.Accounts))
	}
	if out.Accounts[0].Phone != "+4711111111" || !out.Accounts[0].NtfyEnabled || out.Accounts[0].RefreshToken != "r1" {
		t.Fatalf("first account not migrated correctly: %+v", out.Accounts[0])
	}
	if len(out.Sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(out.Sessions))
	}
}

func TestPersistSessions_WritesAccountsWithoutSessions(t *testing.T) {
	dir := t.TempDir()
	ss, _ := NewSessionStore(dir, "k", testLogger())
	sm := NewSessionManager(testLogger(), "", 30)

	auth := gm.NewHermesAuth(gm.WithLogger(testLogger()))
	auth.AccessToken, auth.RefreshToken, auth.InstanceID, auth.ExpiresAt = "a", "r", "inst", 42
	acct := sm.RestoreAccount("+4711111111", auth, testLogger())
	acct.NtfyEnabled = true

	// An account with no browser session must still be persisted (push survives restart).
	sm.persistSessions(ss)
	out := ss.Load()
	if len(out.Accounts) != 1 || out.Accounts[0].InstanceID != "inst" || !out.Accounts[0].NtfyEnabled {
		t.Fatalf("account not persisted: %+v", out.Accounts)
	}
	if len(out.Sessions) != 0 {
		t.Fatalf("expected no sessions, got %d", len(out.Sessions))
	}

	// Attach a session and check it round-trips with its activity time.
	if !sm.RestoreSession("cookie123", "+4711111111", time.Now().Add(-time.Minute)) {
		t.Fatal("RestoreSession should find the account")
	}
	if sm.RestoreSession("x", "+4799999999", time.Time{}) {
		t.Fatal("RestoreSession must fail for unknown phone")
	}
	sm.persistSessions(ss)
	out = ss.Load()
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != "cookie123" || out.Sessions[0].LastActivity.IsZero() {
		t.Fatalf("session not persisted: %+v", out.Sessions)
	}

	// Accounts without a Garmin instance are skipped (nothing to restore).
	noInst := gm.NewHermesAuth(gm.WithLogger(testLogger()))
	sm.RestoreAccount("+4722222222", noInst, testLogger())
	sm.persistSessions(ss)
	if out := ss.Load(); len(out.Accounts) != 1 {
		t.Fatalf("expected instance-less account to be skipped, got %d accounts", len(out.Accounts))
	}
}

func TestCredentialsUpdatedHookRepersists(t *testing.T) {
	sm := NewSessionManager(testLogger(), "", 30)
	called := make(chan struct{}, 1)
	sm.onCredentialsUpdated = func() { called <- struct{}{} }

	auth := gm.NewHermesAuth(gm.WithLogger(testLogger()))
	auth.InstanceID = "i"
	sm.RestoreAccount("+4711111111", auth, testLogger())
	if auth.OnCredentialsUpdated == nil {
		t.Fatal("account auth should have the credentials hook wired")
	}
	auth.OnCredentialsUpdated()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("hook did not fire")
	}
}
