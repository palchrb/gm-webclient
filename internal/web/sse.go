package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// randomHex returns a hex-encoded random string of n bytes (so 2n hex chars).
func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// SSEEvent is an event sent to browser clients via Server-Sent Events.
type SSEEvent struct {
	Type string
	Data any
}

// sseSubscriber is a single browser tab subscribed to the broker. The
// `visible` flag is updated by the client via /api/session/visibility so
// the broker can distinguish "tab is open and watching" from "tab exists
// but is backgrounded" — the latter should still receive push notifications
// even though the SSE channel itself stays alive for fast resume.
type sseSubscriber struct {
	ch      chan SSEEvent
	visible bool
}

// SSEBroker manages SSE subscribers for a single user account.
//
// Two distinct counts are tracked:
//   - SubscriberCount        — total open SSE channels (used by SignalR pause)
//   - VisibleSubscriberCount — channels marked visible (used by push decision)
//
// This decoupling lets us keep SignalR + the SSE channel warm during a brief
// background window so resume is instant, while still firing push the moment
// the tab loses focus.
type SSEBroker struct {
	subscribers     map[string]*sseSubscriber // keyed by client-generated ID
	onNoSubscribers func(SSEEvent)            // called when publishing with no visible tabs
	onEveryPublish  func(SSEEvent)            // called on every publish (always-push mode)
	mu              sync.RWMutex
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		subscribers: make(map[string]*sseSubscriber),
	}
}

func (b *SSEBroker) OnNoSubscribers(fn func(SSEEvent)) {
	b.onNoSubscribers = fn
}

func (b *SSEBroker) OnEveryPublish(fn func(SSEEvent)) {
	b.onEveryPublish = fn
}

// Subscribe registers a new SSE channel under the given client ID. New
// subscribers start visible — the client tells us if it's actually
// backgrounded via SetVisible().
func (b *SSEBroker) Subscribe(clientID string) chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	// If a client reconnects with the same ID (rare), drop the stale entry
	// so the new channel takes over cleanly.
	if old, ok := b.subscribers[clientID]; ok {
		close(old.ch)
	}
	b.subscribers[clientID] = &sseSubscriber{ch: ch, visible: true}
	b.mu.Unlock()
	return ch
}

func (b *SSEBroker) Unsubscribe(clientID string) {
	b.mu.Lock()
	if sub, ok := b.subscribers[clientID]; ok {
		delete(b.subscribers, clientID)
		// Drain the channel so any in-flight publish doesn't block.
		go func(ch chan SSEEvent) {
			for range ch {
			}
		}(sub.ch)
	}
	b.mu.Unlock()
}

// SetVisible updates the visibility flag for a subscriber. Returns true if
// the subscriber was found.
func (b *SSEBroker) SetVisible(clientID string, visible bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub, ok := b.subscribers[clientID]
	if !ok {
		return false
	}
	sub.visible = visible
	return true
}

func (b *SSEBroker) Publish(event SSEEvent) {
	b.mu.RLock()
	visibleCount := 0
	for _, sub := range b.subscribers {
		if sub.visible {
			visibleCount++
		}
	}
	b.mu.RUnlock()

	// Always-push mode: fire on every message regardless of active tabs
	if b.onEveryPublish != nil {
		b.onEveryPublish(event)
	}

	// Push when no VISIBLE subscribers — hidden tabs don't count even
	// though they keep the SSE channel open.
	if visibleCount == 0 {
		if b.onEveryPublish == nil && b.onNoSubscribers != nil {
			b.onNoSubscribers(event)
		}
	}

	// Fan out to ALL subscribers (visible and hidden) so a backgrounded
	// tab still has the events queued when it comes back to foreground.
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		select {
		case sub.ch <- event:
		default:
		}
	}
}

// SubscriberCount returns the total number of open SSE channels regardless
// of visibility. Used by SignalR idle-pause logic.
func (b *SSEBroker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// VisibleSubscriberCount returns the number of subscribers currently marked
// as visible. Used by the push-decision logic.
func (b *SSEBroker) VisibleSubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, sub := range b.subscribers {
		if sub.visible {
			count++
		}
	}
	return count
}

// handleSSE handles the SSE event stream endpoint.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	clientID := r.URL.Query().Get("clientId")
	if clientID == "" {
		// Fallback so old clients still work — generate a per-connection ID.
		// They won't be able to update their visibility but everything else
		// continues to function.
		clientID = "anon-" + randomHex(16)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	acct := session.Account

	// Start shared SignalR + FCM for this account (idempotent)
	s.sessions.EnsureSignalR(acct)
	s.sessions.EnsureFCM(acct)

	// Subscribe to the shared SSE broker
	ch := acct.SSE.Subscribe(clientID)
	s.logger.Info("SSE browser tab connected",
		"phone", acct.Phone,
		"clientId", clientID,
		"activeTabs", acct.SSE.SubscriberCount(),
		"visibleTabs", acct.SSE.VisibleSubscriberCount(),
	)
	defer func() {
		acct.SSE.Unsubscribe(clientID)
		s.logger.Info("SSE browser tab disconnected",
			"phone", acct.Phone,
			"clientId", clientID,
			"activeTabs", acct.SSE.SubscriberCount(),
			"visibleTabs", acct.SSE.VisibleSubscriberCount(),
		)
	}()

	writeSSEEvent(w, "connected", nil)
	flusher.Flush()

	for {
		select {
		case event := <-ch:
			writeSSEEvent(w, event.Type, event.Data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleSetVisibility lets a client mark its SSE subscriber as visible or
// hidden without closing the underlying connection. Used so the push-when-
// no-active-tab logic fires immediately when the tab is backgrounded,
// while still keeping SignalR + the SSE channel warm for fast resume.
func (s *Server) handleSetVisibility(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		ClientID string `json:"clientId"`
		Visible  bool   `json:"visible"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.ClientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "clientId is required"})
		return
	}

	found := session.Account.SSE.SetVisible(req.ClientID, req.Visible)
	if !found {
		// Not necessarily an error — the SSE connection may have just dropped.
		writeJSON(w, http.StatusOK, map[string]any{"updated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":     true,
		"visibleTabs": session.Account.SSE.VisibleSubscriberCount(),
	})
}

func writeSSEEvent(w http.ResponseWriter, eventType string, data any) {
	fmt.Fprintf(w, "event: %s\n", eventType)
	if data != nil {
		jsonData, err := json.Marshal(data)
		if err == nil {
			fmt.Fprintf(w, "data: %s\n", jsonData)
		}
	} else {
		fmt.Fprintf(w, "data: {}\n")
	}
	fmt.Fprintf(w, "\n")
}
