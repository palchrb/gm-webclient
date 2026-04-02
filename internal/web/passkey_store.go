package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-webauthn/webauthn/webauthn"
)

// PasskeyStore persists WebAuthn credentials per phone number on disk.
// Credentials survive logout and session expiry — they represent device
// identity, not login state.
type PasskeyStore struct {
	dataDir string
	mu      sync.RWMutex
}

func NewPasskeyStore(dataDir string) *PasskeyStore {
	return &PasskeyStore{dataDir: dataDir}
}

func (s *PasskeyStore) path(phone string) string {
	return filepath.Join(s.dataDir, "passkeys", phone+".json")
}

// Load returns stored credentials for a phone number.
func (s *PasskeyStore) Load(phone string) []webauthn.Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path(phone))
	if err != nil {
		return nil
	}

	var creds []webauthn.Credential
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil
	}
	return creds
}

// Save persists credentials for a phone number.
func (s *PasskeyStore) Save(phone string, creds []webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.path(phone)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(creds, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

// HasCredentials returns true if the phone has any stored passkeys.
func (s *PasskeyStore) HasCredentials(phone string) bool {
	creds := s.Load(phone)
	return len(creds) > 0
}
