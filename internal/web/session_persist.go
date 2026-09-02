package web

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// SessionStore handles encrypted persistence of session credentials.
type SessionStore struct {
	dataDir string
	gcm     cipher.AEAD
	logger  *slog.Logger
}

// persistedState is the plaintext structure written (after encryption) to disk.
//
// Accounts and sessions are persisted separately: an account (Garmin tokens,
// one per phone) can outlive all of its browser sessions so FCM push keeps
// working after a browser-only logout and across restarts. Sessions carry
// only the cookie ID and last-activity time.
type persistedState struct {
	Version  int                `json:"version"`
	Accounts []persistedAccount `json:"accounts"`
	Sessions []persistedSession `json:"sessions"`
}

type persistedAccount struct {
	Phone        string  `json:"phone"`
	AccessToken  string  `json:"accessToken"`
	RefreshToken string  `json:"refreshToken"`
	InstanceID   string  `json:"instanceId"`
	ExpiresAt    float64 `json:"expiresAt"`
}

type persistedSession struct {
	SessionID    string    `json:"sessionId"`
	Phone        string    `json:"phone"`
	LastActivity time.Time `json:"lastActivity"`
}

// legacySession is the pre-v2 on-disk format: one flat entry per session with
// the account tokens duplicated into each. Still readable for upgrades.
type legacySession struct {
	SessionID    string  `json:"sessionId"`
	Phone        string  `json:"phone"`
	AccessToken  string  `json:"accessToken"`
	RefreshToken string  `json:"refreshToken"`
	InstanceID   string  `json:"instanceId"`
	ExpiresAt    float64 `json:"expiresAt"`
}

func NewSessionStore(dataDir, sessionKey string, logger *slog.Logger) (*SessionStore, error) {
	hash := sha256.Sum256([]byte(sessionKey))
	block, err := aes.NewCipher(hash[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SessionStore{dataDir: dataDir, gcm: gcm, logger: logger}, nil
}

func (ss *SessionStore) path() string {
	return filepath.Join(ss.dataDir, "sessions.enc")
}

// Save encrypts and persists the given state.
func (ss *SessionStore) Save(state persistedState) {
	state.Version = 2
	plaintext, err := json.Marshal(state)
	if err != nil {
		ss.logger.Error("Failed to marshal sessions", "error", err)
		return
	}

	nonce := make([]byte, ss.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		ss.logger.Error("Failed to generate nonce", "error", err)
		return
	}

	ciphertext := ss.gcm.Seal(nonce, nonce, plaintext, nil)

	if err := os.MkdirAll(filepath.Dir(ss.path()), 0o755); err != nil {
		ss.logger.Error("Failed to create sessions directory", "error", err)
		return
	}
	if err := os.WriteFile(ss.path(), ciphertext, 0o600); err != nil {
		ss.logger.Error("Failed to write encrypted sessions", "error", err)
	}
}

// Load decrypts and parses persisted state. Understands both the current
// format and the legacy flat-array format.
func (ss *SessionStore) Load() persistedState {
	var state persistedState

	data, err := os.ReadFile(ss.path())
	if err != nil {
		return state
	}

	nonceSize := ss.gcm.NonceSize()
	if len(data) < nonceSize {
		ss.logger.Warn("Encrypted session file too short")
		return state
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := ss.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		ss.logger.Warn("Failed to decrypt sessions (wrong SESSION_KEY?)", "error", err)
		return state
	}

	if len(plaintext) > 0 && plaintext[0] == '[' {
		var legacy []legacySession
		if err := json.Unmarshal(plaintext, &legacy); err != nil {
			ss.logger.Warn("Failed to parse legacy sessions", "error", err)
			return state
		}
		seen := map[string]bool{}
		for _, l := range legacy {
			if !seen[l.Phone] {
				seen[l.Phone] = true
				state.Accounts = append(state.Accounts, persistedAccount{
					Phone: l.Phone, AccessToken: l.AccessToken, RefreshToken: l.RefreshToken,
					InstanceID: l.InstanceID, ExpiresAt: l.ExpiresAt,
				})
			}
			state.Sessions = append(state.Sessions, persistedSession{SessionID: l.SessionID, Phone: l.Phone})
		}
		return state
	}

	if err := json.Unmarshal(plaintext, &state); err != nil {
		ss.logger.Warn("Failed to parse decrypted sessions", "error", err)
		return persistedState{}
	}
	return state
}

func (ss *SessionStore) Delete() {
	os.Remove(ss.path())
}

// RestoreSessions recreates accounts and sessions from encrypted storage.
// Returns the number of accounts restored.
func (sm *SessionManager) RestoreSessions(store *SessionStore, logger *slog.Logger) int {
	state := store.Load()
	if len(state.Accounts) == 0 {
		return 0
	}

	restored := 0
	for _, entry := range state.Accounts {
		auth := gm.NewHermesAuth(gm.WithLogger(logger))
		auth.AccessToken = entry.AccessToken
		auth.RefreshToken = entry.RefreshToken
		auth.InstanceID = entry.InstanceID
		auth.ExpiresAt = entry.ExpiresAt

		if auth.TokenExpired() {
			if err := auth.RefreshHermesToken(context.Background()); err != nil {
				sm.logger.Warn("Restored account expired, skipping", "phone", entry.Phone, "error", err)
				continue
			}
		}

		api := gm.NewHermesAPI(auth, gm.WithAPILogger(logger))
		if _, err := api.GetConversations(context.Background(), gm.WithLimit(1)); err != nil {
			sm.logger.Warn("Restored account credentials invalid, skipping", "phone", entry.Phone, "error", err)
			continue
		}

		sm.RestoreAccount(entry.Phone, auth, logger)
		restored++
		sm.logger.Info("Restored account", "phone", entry.Phone)
	}

	sessionTTL := time.Duration(sm.sessionDays) * 24 * time.Hour
	for _, s := range state.Sessions {
		if !s.LastActivity.IsZero() && time.Since(s.LastActivity) > sessionTTL {
			continue // expired while we were down
		}
		if sm.RestoreSession(s.SessionID, s.Phone, s.LastActivity) {
			sm.logger.Info("Restored session", "phone", s.Phone, "cookieId", s.SessionID[:8]+"...")
		}
	}

	return restored
}

func (sm *SessionManager) persistSessions(store *SessionStore) {
	sm.mu.RLock()
	state := persistedState{
		Accounts: make([]persistedAccount, 0, len(sm.accounts)),
		Sessions: make([]persistedSession, 0, len(sm.sessions)),
	}
	for phone, acct := range sm.accounts {
		a := acct.Auth
		if a == nil || a.InstanceID == "" {
			continue
		}
		state.Accounts = append(state.Accounts, persistedAccount{
			Phone: phone, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			InstanceID: a.InstanceID, ExpiresAt: a.ExpiresAt,
		})
	}
	for _, s := range sm.sessions {
		s.mu.Lock()
		last := s.LastActivity
		s.mu.Unlock()
		state.Sessions = append(state.Sessions, persistedSession{
			SessionID: s.ID, Phone: s.Account.Phone, LastActivity: last,
		})
	}
	sm.mu.RUnlock()
	store.Save(state)
}
