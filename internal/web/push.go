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
func (srv *Server) sendWebPush(acct *UserAccount, event SSEEvent) {
	srv.logger.Info("sendWebPush called",
		"phone", acct.Phone,
		"eventType", event.Type,
		"dataType", fmt.Sprintf("%T", event.Data),
	)

	if event.Type != "message" {
		srv.logger.Debug("sendWebPush: skipping non-message event", "type", event.Type)
		return
	}
	if srv.vapidKeys == nil {
		srv.logger.Warn("sendWebPush: VAPID keys not configured")
		return
	}

	// Extract notification content from the message (skip own messages)
	payload := buildPushPayload(event.Data, acct.Phone)
	if payload == nil {
		srv.logger.Info("sendWebPush: payload nil (own message or unknown type)",
			"phone", acct.Phone,
		)
		return
	}
	// Use the sender as the notification title when available so users can
	// see at a glance who the message is from in the browser notification.
	if from := payload["from"]; from != "" {
		payload["title"] = from
	}
	srv.logger.Info("sendWebPush: sending notification",
		"phone", acct.Phone,
		"title", payload["title"],
		"body", payload["body"],
	)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return
	}

	acct.pushMu.RLock()
	subs := make([]*webpush.Subscription, 0, len(acct.PushSubscriptions))
	endpoints := make([]string, 0, len(acct.PushSubscriptions))
	for ep, sub := range acct.PushSubscriptions {
		subs = append(subs, sub)
		endpoints = append(endpoints, ep)
	}
	acct.pushMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	var expiredEndpoints []string

	for i, sub := range subs {
		resp, err := webpush.SendNotification(payloadJSON, sub, &webpush.Options{
			Subscriber:      "mailto:garmin-web@localhost",
			VAPIDPublicKey:  srv.vapidKeys.PublicKey,
			VAPIDPrivateKey: srv.vapidKeys.PrivateKey,
			TTL:             86400,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			srv.logger.Error("Web push send failed", "phone", acct.Phone, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			expiredEndpoints = append(expiredEndpoints, endpoints[i])
		}
	}

	// Clean up expired subscriptions
	if len(expiredEndpoints) > 0 {
		acct.pushMu.Lock()
		for _, ep := range expiredEndpoints {
			delete(acct.PushSubscriptions, ep)
		}
		acct.pushMu.Unlock()

		if srv.pushStore != nil {
			srv.pushStore.Save(acct.Phone, acct.PushSubscriptions)
		}
	}
}

func buildPushPayload(data any, phone string) map[string]string {
	switch msg := data.(type) {
	case gm.MessageModel:
		// Don't push notifications for our own sent messages
		if msg.From != nil {
			from := *msg.From
			if from == phone || from == gm.PhoneToHermesUserID(phone) {
				return nil
			}
		}
		p := map[string]string{
			"title":          "Garmin Messenger",
			"conversationId": msg.ConversationID.String(),
		}
		// Include raw sender so downstream (ntfy, web push) can render a
		// sender-aware title. MessengerApp senders give us an E.164 phone
		// like "+4740847119"; inReach devices give a Hermes UUID.
		if msg.From != nil && *msg.From != "" {
			p["from"] = *msg.From
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
	acct := session.Account

	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid subscription"})
		return
	}
	if sub.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint is required"})
		return
	}

	acct.pushMu.Lock()
	if acct.PushSubscriptions == nil {
		acct.PushSubscriptions = make(map[string]*webpush.Subscription)
	}
	acct.PushSubscriptions[sub.Endpoint] = &sub
	acct.pushMu.Unlock()

	if srv.pushStore != nil {
		srv.pushStore.Save(acct.Phone, acct.PushSubscriptions)
	}

	srv.logger.Info("Push subscription added", "phone", acct.Phone, "endpoint", sub.Endpoint[:min(50, len(sub.Endpoint))]+"...")
	writeJSON(w, http.StatusOK, map[string]string{"status": "subscribed"})
}

// handlePushUnsubscribe removes a browser push subscription.
func (srv *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	acct := session.Account

	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint is required"})
		return
	}

	acct.pushMu.Lock()
	delete(acct.PushSubscriptions, req.Endpoint)
	acct.pushMu.Unlock()

	if srv.pushStore != nil {
		srv.pushStore.Save(acct.Phone, acct.PushSubscriptions)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}
