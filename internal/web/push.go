package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPIDKeys holds the VAPID key pair for Web Push.
type VAPIDKeys struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
}

// LoadOrGenerateVAPIDKeys loads VAPID keys from disk, or generates and saves them.
func LoadOrGenerateVAPIDKeys(dataDir string) (*VAPIDKeys, error) {
	path := filepath.Join(dataDir, "vapid_keys.json")

	data, err := os.ReadFile(path)
	if err == nil {
		var keys VAPIDKeys
		if err := json.Unmarshal(data, &keys); err != nil {
			return nil, fmt.Errorf("parsing VAPID keys: %w", err)
		}
		if keys.PublicKey != "" && keys.PrivateKey != "" {
			return &keys, nil
		}
	}

	// Generate new keys
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, fmt.Errorf("generating VAPID keys: %w", err)
	}

	keys := &VAPIDKeys{PublicKey: pub, PrivateKey: priv}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating VAPID keys directory: %w", err)
	}
	keysJSON, _ := json.MarshalIndent(keys, "", "  ")
	if err := os.WriteFile(path, keysJSON, 0o600); err != nil {
		return nil, fmt.Errorf("saving VAPID keys: %w", err)
	}

	return keys, nil
}

// PushSubscriptionStore manages per-phone push subscription persistence.
type PushSubscriptionStore struct {
	dataDir string
	mu      sync.RWMutex
}

// NewPushSubscriptionStore creates a new store.
func NewPushSubscriptionStore(dataDir string) *PushSubscriptionStore {
	return &PushSubscriptionStore{dataDir: dataDir}
}

func (s *PushSubscriptionStore) path(phone string) string {
	return filepath.Join(s.dataDir, "push", phone, "subscriptions.json")
}

// Load returns stored subscriptions for a phone number.
func (s *PushSubscriptionStore) Load(phone string) map[string]*webpush.Subscription {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path(phone))
	if err != nil {
		return make(map[string]*webpush.Subscription)
	}

	var subs map[string]*webpush.Subscription
	if err := json.Unmarshal(data, &subs); err != nil {
		return make(map[string]*webpush.Subscription)
	}
	return subs
}

// Save persists subscriptions for a phone number.
func (s *PushSubscriptionStore) Save(phone string, subs map[string]*webpush.Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.path(phone)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(subs, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

// sendWebPush sends a push notification to all of a session's push subscribers.
func (srv *Server) sendWebPush(session *UserSession, event SSEEvent) {
	if event.Type != "message" {
		return
	}
	if srv.vapidKeys == nil {
		return
	}

	// Extract notification content from the message
	payload := buildPushPayload(event.Data)
	if payload == nil {
		return
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return
	}

	session.pushMu.RLock()
	subs := make([]*webpush.Subscription, 0, len(session.PushSubscriptions))
	endpoints := make([]string, 0, len(session.PushSubscriptions))
	for ep, sub := range session.PushSubscriptions {
		subs = append(subs, sub)
		endpoints = append(endpoints, ep)
	}
	session.pushMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	var expiredEndpoints []string

	for i, sub := range subs {
		resp, err := webpush.SendNotification(payloadJSON, sub, &webpush.Options{
			Subscriber:      "mailto:garmin-web@localhost",
			VAPIDPublicKey:  srv.vapidKeys.PublicKey,
			VAPIDPrivateKey: srv.vapidKeys.PrivateKey,
			TTL:             120,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			srv.logger.Error("Web push send failed", "phone", session.Phone, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			expiredEndpoints = append(expiredEndpoints, endpoints[i])
		}
	}

	// Clean up expired subscriptions
	if len(expiredEndpoints) > 0 {
		session.pushMu.Lock()
		for _, ep := range expiredEndpoints {
			delete(session.PushSubscriptions, ep)
		}
		session.pushMu.Unlock()

		if srv.pushStore != nil {
			srv.pushStore.Save(session.Phone, session.PushSubscriptions)
		}
	}
}

func buildPushPayload(data any) map[string]string {
	switch msg := data.(type) {
	case gm.MessageModel:
		p := map[string]string{
			"title":          "Garmin Messenger",
			"conversationId": msg.ConversationID.String(),
		}
		if msg.MessageBody != nil {
			p["body"] = *msg.MessageBody
		} else {
			p["body"] = "New message"
		}
		return p
	default:
		return nil
	}
}

// handleGetVAPIDKey returns the VAPID public key.
func (srv *Server) handleGetVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if srv.vapidKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "push notifications not configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": srv.vapidKeys.PublicKey})
}

// handlePushSubscribe stores a browser push subscription.
func (srv *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid subscription"})
		return
	}
	if sub.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint is required"})
		return
	}

	session.pushMu.Lock()
	if session.PushSubscriptions == nil {
		session.PushSubscriptions = make(map[string]*webpush.Subscription)
	}
	session.PushSubscriptions[sub.Endpoint] = &sub
	session.pushMu.Unlock()

	if srv.pushStore != nil {
		srv.pushStore.Save(session.Phone, session.PushSubscriptions)
	}

	srv.logger.Info("Push subscription added", "phone", session.Phone, "endpoint", sub.Endpoint[:min(50, len(sub.Endpoint))]+"...")
	writeJSON(w, http.StatusOK, map[string]string{"status": "subscribed"})
}

// handlePushUnsubscribe removes a browser push subscription.
func (srv *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint is required"})
		return
	}

	session.pushMu.Lock()
	delete(session.PushSubscriptions, req.Endpoint)
	session.pushMu.Unlock()

	if srv.pushStore != nil {
		srv.pushStore.Save(session.Phone, session.PushSubscriptions)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}
