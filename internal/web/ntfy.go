package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// NtfyConfig holds ntfy.sh integration settings.
type NtfyConfig struct {
	BaseURL     string // e.g. "https://ntfy.sh" or self-hosted URL
	HMACKey     []byte // secret for deriving per-phone topics
	ClickURL    string // optional: URL opened when user taps the notification
	FullMessage bool   // if true, include message body; if false, just "New message"
}

// LoadOrGenerateNtfyHMACKey reads or creates a 32-byte HMAC key for ntfy topic derivation.
func LoadOrGenerateNtfyHMACKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "ntfy_hmac_key")

	data, err := os.ReadFile(path)
	if err == nil && len(data) >= 32 {
		return data[:32], nil
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating ntfy HMAC key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating directory for ntfy HMAC key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("saving ntfy HMAC key: %w", err)
	}

	return key, nil
}

// NtfyTopicForPhone derives a deterministic, unguessable ntfy topic from a phone number.
func NtfyTopicForPhone(hmacKey []byte, phone string) string {
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(phone))
	return "gm-" + hex.EncodeToString(mac.Sum(nil))[:24]
}

// sendNtfy sends a push notification to ntfy for the given account.
func (srv *Server) sendNtfy(acct *UserAccount, event SSEEvent) {
	if srv.ntfyConfig == nil {
		return
	}
	if !acct.NtfyEnabled {
		return
	}
	if event.Type != "message" {
		return
	}

	payload := buildPushPayload(event.Data, acct.Phone)
	if payload == nil {
		return
	}

	topic := NtfyTopicForPhone(srv.ntfyConfig.HMACKey, acct.Phone)

	// Per-user FullText override takes precedence over the server-wide default.
	fullText := srv.ntfyConfig.FullMessage
	if acct.NtfyFullText != nil {
		fullText = *acct.NtfyFullText
	}

	message := "New message"
	if fullText {
		message = payload["body"]
	}

	// Use the sender phone as the ntfy title when available; fall back to
	// the generic app name. This gives the notification a "who sent it"
	// header even when FullText is off for privacy.
	title := payload["title"]
	if from := payload["from"]; from != "" {
		title = from
	}

	ntfyPayload := map[string]any{
		"topic":    topic,
		"title":    title,
		"message":  message,
		"priority": 4,
		"tags":     []string{"speech_balloon"},
	}

	srv.logger.Info("ntfy: sending notification",
		"phone", acct.Phone,
		"from", payload["from"],
		"title", title,
		"fullText", fullText,
	)
	if srv.ntfyConfig.ClickURL != "" {
		clickURL := srv.ntfyConfig.ClickURL
		if convId, ok := payload["conversationId"]; ok && convId != "" {
			clickURL += "#conversation/" + convId
		}
		ntfyPayload["click"] = clickURL
	}

	body, err := json.Marshal(ntfyPayload)
	if err != nil {
		srv.logger.Error("Failed to marshal ntfy payload", "error", err)
		return
	}

	url := strings.TrimRight(srv.ntfyConfig.BaseURL, "/")
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		srv.logger.Error("Failed to create ntfy request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		srv.logger.Error("Failed to send ntfy notification", "phone", acct.Phone, "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode == 429 {
		srv.logger.Warn("ntfy rate limited", "phone", acct.Phone)
		return
	}
	if resp.StatusCode >= 400 {
		srv.logger.Error("ntfy returned error", "phone", acct.Phone, "status", resp.StatusCode)
		return
	}

	srv.logger.Debug("ntfy notification sent", "phone", acct.Phone, "topic", topic)
}

// handleGetNtfyInfo returns ntfy configuration for the authenticated user.
func (srv *Server) handleGetNtfyInfo(w http.ResponseWriter, r *http.Request) {
	if srv.ntfyConfig == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}

	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	phone := session.Phone()
	topic := NtfyTopicForPhone(srv.ntfyConfig.HMACKey, phone)
	baseURL := strings.TrimRight(srv.ntfyConfig.BaseURL, "/")
	userID := gm.PhoneToHermesUserID(phone)

	// Build ntfy:// deep link for Android app (not supported on iOS)
	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	appURL := "ntfy://" + host + "/" + topic + "?display=Garmin+Messenger"

	// Resolve effective FullText: per-user override takes precedence,
	// otherwise fall back to the server-wide default.
	effectiveFullText := srv.ntfyConfig.FullMessage
	fullTextIsOverride := false
	if session.Account.NtfyFullText != nil {
		effectiveFullText = *session.Account.NtfyFullText
		fullTextIsOverride = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            true,
		"subscribed":         session.Account.NtfyEnabled,
		"topic":              topic,
		"server":             baseURL,
		"appUrl":             appURL,
		"userId":             userID,
		"fullText":           effectiveFullText,
		"fullTextIsOverride": fullTextIsOverride,
		"serverDefaultFullText": srv.ntfyConfig.FullMessage,
	})
}

// handleNtfySubscribe toggles ntfy push notifications for the authenticated user.
func (srv *Server) handleNtfySubscribe(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	session.Account.NtfyEnabled = req.Enabled

	// Persist as a per-phone preference (survives logout/restart), preserving
	// any existing FullText override.
	if srv.ntfyStore != nil {
		prefs := NtfyPrefs{Enabled: req.Enabled, FullText: session.Account.NtfyFullText}
		if err := srv.ntfyStore.Save(session.Phone(), prefs); err != nil {
			srv.logger.Warn("Failed to persist ntfy preference", "phone", session.Phone(), "error", err)
		}
	}

	srv.logger.Info("ntfy subscription changed", "phone", session.Phone(), "enabled", req.Enabled)
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

// handleSetNtfyFullText sets the per-user full-text override for ntfy
// notifications. Request body accepts {"fullText": true|false} to override
// the server default, or {"fullText": null} to clear the override.
func (srv *Server) handleSetNtfyFullText(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		FullText *bool `json:"fullText"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	session.Account.NtfyFullText = req.FullText

	if srv.ntfyStore != nil {
		prefs := NtfyPrefs{Enabled: session.Account.NtfyEnabled, FullText: req.FullText}
		if err := srv.ntfyStore.Save(session.Phone(), prefs); err != nil {
			srv.logger.Warn("Failed to persist ntfy fullText preference", "phone", session.Phone(), "error", err)
		}
	}

	// Compute effective value for the response.
	effective := srv.ntfyConfig != nil && srv.ntfyConfig.FullMessage
	if req.FullText != nil {
		effective = *req.FullText
	}
	srv.logger.Info("ntfy fullText changed", "phone", session.Phone(), "override", req.FullText, "effective", effective)
	writeJSON(w, http.StatusOK, map[string]any{
		"fullText":           effective,
		"fullTextIsOverride": req.FullText != nil,
	})
}
