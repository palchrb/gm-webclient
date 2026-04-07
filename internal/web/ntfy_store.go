package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// NtfyStore persists per-phone ntfy enable/disable state on disk.
// The preference survives logout, session expiry, and server restarts —
// it represents a user preference, not session state.
type NtfyStore struct {
	dataDir string
	mu      sync.RWMutex
}

func NewNtfyStore(dataDir string) *NtfyStore {
	return &NtfyStore{dataDir: dataDir}
}

func (s *NtfyStore) path(phone string) string {
	return filepath.Join(s.dataDir, "ntfy", phone+".json")
}

type ntfyPrefs struct {
	Enabled bool `json:"enabled"`
}

// Load returns the stored ntfy enabled state for a phone number.
// Returns false if no state has been persisted.
func (s *NtfyStore) Load(phone string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path(phone))
	if err != nil {
		return false
	}

	var prefs ntfyPrefs
	if err := json.Unmarshal(data, &prefs); err != nil {
		return false
	}
	return prefs.Enabled
}

// Save persists the ntfy enabled state for a phone number.
func (s *NtfyStore) Save(phone string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.path(phone)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(ntfyPrefs{Enabled: enabled}, "", "  ")
	return os.WriteFile(p, data, 0o600)
}
