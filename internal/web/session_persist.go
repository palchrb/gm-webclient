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

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// SessionStore handles encrypted persistence of session credentials.
type SessionStore struct {
	dataDir string
	gcm     cipher.AEAD
	logger  *slog.Logger
}

// persistedSession is the plaintext structure written (after encryption) to disk.
type persistedSession struct {
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

// Save encrypts and persists all active sessions.
func (ss *SessionStore) Save(sessions map[string]*UserSession) {
	entries := make([]persistedSession, 0, len(sessions))
	for _, s := range sessions {
		entries = append(entries, persistedSession{
			SessionID:    s.ID,
			Phone:        s.Account.Phone,
			AccessToken:  s.Account.Auth.AccessToken,
			RefreshToken: s.Account.Auth.RefreshToken,
			InstanceID:   s.Account.Auth.InstanceID,
			ExpiresAt:    s.Account.Auth.ExpiresAt,
		})
	}

	plaintext, err := json.Marshal(entries)
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

func (ss *SessionStore) Load() []persistedSession {
	data, err := os.ReadFile(ss.path())
	if err != nil {
		return nil
	}

	nonceSize := ss.gcm.NonceSize()
	if len(data) < nonceSize {
		ss.logger.Warn("Encrypted session file too short")
		return nil
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := ss.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		ss.logger.Warn("Failed to decrypt sessions (wrong SESSION_KEY?)", "error", err)
		return nil
	}

	var entries []persistedSession
	if err := json.Unmarshal(plaintext, &entries); err != nil {
		ss.logger.Warn("Failed to parse decrypted sessions", "error", err)
		return nil
	}
	return entries
}

func (ss *SessionStore) Delete() {
	os.Remove(ss.path())
}

// RestoreSessions recreates sessions from encrypted storage.
// With hard pruning, only one session per phone is kept (the first valid one).
func (sm *SessionManager) RestoreSessions(store *SessionStore, logger *slog.Logger) int {
	entries := store.Load()
	if len(entries) == 0 {
		return 0
	}

	// Only restore one session per phone (the first valid one)
	restoredPhones := map[string]bool{}
	restored := 0

	for _, entry := range entries {
		// Skip already-restored phones (hard prune)
		if restoredPhones[entry.Phone] {
			sm.logger.Info("Skipping duplicate session for phone (hard prune)", "phone", entry.Phone)
			continue
		}

		auth := gm.NewHermesAuth(gm.WithLogger(logger))
		auth.AccessToken = entry.AccessToken
		auth.RefreshToken = entry.RefreshToken
		auth.InstanceID = entry.InstanceID
		auth.ExpiresAt = entry.ExpiresAt

		if auth.TokenExpired() {
			if err := auth.RefreshHermesToken(context.Background()); err != nil {
				sm.logger.Warn("Restored session expired, skipping",
					"phone", entry.Phone, "error", err)
				continue
			}
		}

		api := gm.NewHermesAPI(auth, gm.WithAPILogger(logger))
		if _, err := api.GetConversations(context.Background(), gm.WithLimit(1)); err != nil {
			sm.logger.Warn("Restored session credentials invalid, skipping",
				"phone", entry.Phone, "error", err)
			continue
		}

		session, err := sm.CreateSession(entry.Phone, auth, logger)
		if err != nil {
			sm.logger.Warn("Failed to recreate session", "phone", entry.Phone, "error", err)
			continue
		}

		// Replace auto-generated session ID with the persisted one
		sm.mu.Lock()
		delete(sm.sessions, session.ID)
		session.ID = entry.SessionID
		sm.sessions[entry.SessionID] = session
		sm.mu.Unlock()

		restoredPhones[entry.Phone] = true
		restored++
		sm.logger.Info("Restored session", "phone", entry.Phone, "cookieId", entry.SessionID[:8]+"...")
	}

	return restored
}

func (sm *SessionManager) persistSessions(store *SessionStore) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	store.Save(sm.sessions)
}
