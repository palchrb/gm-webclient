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
// Multiple sessions for the same phone share one account.
// Validates credentials only once per phone number (not per session).
func (sm *SessionManager) RestoreSessions(store *SessionStore, logger *slog.Logger) int {
	entries := store.Load()
	if len(entries) == 0 {
		return 0
	}

	// Group entries by phone — validate and create account only once per phone
	validatedPhones := map[string]bool{} // phone → validated ok
	restored := 0

	for _, entry := range entries {
		// Skip phones that already failed validation
		if validated, exists := validatedPhones[entry.Phone]; exists && !validated {
			continue
		}

		// First session for this phone — validate credentials
		if _, exists := validatedPhones[entry.Phone]; !exists {
			auth := gm.NewHermesAuth(gm.WithLogger(logger))
			auth.AccessToken = entry.AccessToken
			auth.RefreshToken = entry.RefreshToken
			auth.InstanceID = entry.InstanceID
			auth.ExpiresAt = entry.ExpiresAt

			if auth.TokenExpired() {
				if err := auth.RefreshHermesToken(context.Background()); err != nil {
					sm.logger.Warn("Restored session expired, skipping phone",
						"phone", entry.Phone, "error", err)
					validatedPhones[entry.Phone] = false
					continue
				}
			}

			api := gm.NewHermesAPI(auth, gm.WithAPILogger(logger))
			if _, err := api.GetConversations(context.Background(), gm.WithLimit(1)); err != nil {
				sm.logger.Warn("Restored session credentials invalid, skipping phone",
					"phone", entry.Phone, "error", err)
				validatedPhones[entry.Phone] = false
				continue
			}

			// Create the account (first session creates it)
			session, err := sm.CreateSession(entry.Phone, auth, logger)
			if err != nil {
				sm.logger.Warn("Failed to recreate session", "phone", entry.Phone, "error", err)
				validatedPhones[entry.Phone] = false
				continue
			}

			sm.mu.Lock()
			delete(sm.sessions, session.ID)
			session.ID = entry.SessionID
			sm.sessions[entry.SessionID] = session
			sm.mu.Unlock()

			validatedPhones[entry.Phone] = true
			restored++
			sm.logger.Info("Restored session (primary)", "phone", entry.Phone)
			continue
		}

		// Additional session for an already-validated phone — just create session
		// (account already exists, getOrCreateAccount will reuse it without restarting)
		session, err := sm.CreateSession(entry.Phone,
			sm.accounts[entry.Phone].Auth, logger)
		if err != nil {
			continue
		}

		sm.mu.Lock()
		delete(sm.sessions, session.ID)
		session.ID = entry.SessionID
		sm.sessions[entry.SessionID] = session
		sm.mu.Unlock()

		restored++
		sm.logger.Info("Restored session (additional)", "phone", entry.Phone)
	}

	return restored
}

func (sm *SessionManager) persistSessions(store *SessionStore) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	store.Save(sm.sessions)
}
