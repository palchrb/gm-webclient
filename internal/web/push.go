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

// Delete removes the persisted subscriptions for a phone number.
func (s *PushSubscriptionStore) Delete(phone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.RemoveAll(filepath.Dir(s.path(phone)))
}

// pushTTL is how long the push service should hold a notification for an
// offline device. Messages must not be lost just because the phone was
// asleep for a few minutes.
const pushTTL = 24 * 60 * 60 // seconds

// sendWebPush sends a push notification to all of a session's push subscribers.
//
// Privacy: the payload is content-free — it carries only the conversation ID.
// The service worker fetches the message preview from this server over the
// session cookie, so message text never transits Google/Apple/Mozilla push
// services, and nothing about the message is logged here.
func (srv *Server) sendWebPush(acct *UserAccount, event SSEEvent) {
	if event.Type != "message" {
		return
	}
	if srv.vapidKeys == nil {
		srv.logger.Warn("sendWebPush: VAPID keys not configured")
		return
	}

	payload := buildPushPayload(event.Data, acct.Phone)
	if payload == nil {
		return // own message or unknown type
	}
	// Only the identifiers go over the wire. Sender and body stay on the
	// server; the service worker fetches them over the session cookie and
	// renders "Garmin: <sender>" locally. (ntfy has its own opt-in path.)
	wire := map[string]string{
		"conversationId": payload["conversationId"],
		"messageId":      payload["messageId"],
	}
	srv.logger.Debug("sendWebPush: sending notification",
		"phone", acct.Phone,
		"conversationId", wire["conversationId"],
	)
	payloadJSON, err := json.Marshal(wire)
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
			TTL:             pushTTL,
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
		// Internal payload. Web push sends only the IDs (see sendWebPush);
		// ntfy may include from/body when the user has opted in.
		p := map[string]string{
			"title":          "Garmin Messenger",
			"conversationId": msg.ConversationID.String(),
			"messageId":      msg.MessageID.String(),
		}
		// Raw sender so ntfy can render a sender-aware title. MessengerApp
		// senders give us an E.164 phone like "+4740847119"; inReach devices
		// give a Hermes UUID.
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

// clearPushSubscriptions removes all push subscriptions for an account, both
// in memory and on disk. Used by full logout so a logged-out browser never
// receives further notifications.
func (srv *Server) clearPushSubscriptions(acct *UserAccount) {
	acct.pushMu.Lock()
	acct.PushSubscriptions = make(map[string]*webpush.Subscription)
	acct.pushMu.Unlock()
	if srv.pushStore != nil {
		srv.pushStore.Delete(acct.Phone)
	}
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
