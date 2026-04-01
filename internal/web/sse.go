package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// SSEEvent is an event sent to browser clients via Server-Sent Events.
type SSEEvent struct {
	Type string
	Data any
}

// SSEBroker manages SSE subscribers for a single user account.
type SSEBroker struct {
	subscribers     map[chan SSEEvent]struct{}
	onNoSubscribers func(SSEEvent)
	mu              sync.RWMutex
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		subscribers: make(map[chan SSEEvent]struct{}),
	}
}

func (b *SSEBroker) OnNoSubscribers(fn func(SSEEvent)) {
	b.onNoSubscribers = fn
}

func (b *SSEBroker) Subscribe() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *SSEBroker) Unsubscribe(ch chan SSEEvent) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	for range ch {
	}
}

func (b *SSEBroker) Publish(event SSEEvent) {
	b.mu.RLock()
	count := len(b.subscribers)
	b.mu.RUnlock()

	if count == 0 {
		if b.onNoSubscribers != nil {
			b.onNoSubscribers(event)
		}
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (b *SSEBroker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	acct := session.Account

	// Start shared SignalR + FCM for this account (idempotent)
	s.sessions.EnsureSignalR(acct)
	s.sessions.EnsureFCM(acct)

	// Subscribe to the shared SSE broker
	ch := acct.SSE.Subscribe()
	s.logger.Info("SSE subscriber connected",
		"phone", acct.Phone,
		"subscribers", acct.SSE.SubscriberCount(),
	)
	defer func() {
		acct.SSE.Unsubscribe(ch)
		s.logger.Info("SSE subscriber disconnected",
			"phone", acct.Phone,
			"subscribers", acct.SSE.SubscriberCount(),
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
