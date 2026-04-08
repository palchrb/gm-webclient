package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// NtfyStore persists per-phone ntfy preferences on disk. The preference
// survives logout, session expiry, and server restarts — it represents a
// user preference, not session state.
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

// NtfyPrefs holds per-phone ntfy configuration.
//
// FullText is a tri-state override:
//   - nil   → follow the server-wide NtfyConfig.FullMessage setting
//   - true  → always include the full message body
//   - false → always send just "New message" (privacy/notification-length)
type NtfyPrefs struct {
	Enabled  bool  `json:"enabled"`
	FullText *bool `json:"fullText,omitempty"`
}

// Load returns the stored ntfy preferences for a phone number.
// Returns zero value (Enabled=false, FullText=nil) if no state has been persisted.
func (s *NtfyStore) Load(phone string) NtfyPrefs {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path(phone))
	if err != nil {
		return NtfyPrefs{}
	}

	var prefs NtfyPrefs
	if err := json.Unmarshal(data, &prefs); err != nil {
		return NtfyPrefs{}
	}
	return prefs
}

// Save persists ntfy preferences for a phone number.
func (s *NtfyStore) Save(phone string, prefs NtfyPrefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.path(phone)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(prefs, "", "  ")
	return os.WriteFile(p, data, 0o600)
}
