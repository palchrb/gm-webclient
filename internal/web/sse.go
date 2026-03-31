package web

import (
	"context"
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

// SSEBroker manages SSE subscribers for a single user session.
type SSEBroker struct {
	subscribers     map[chan SSEEvent]struct{}
	onNoSubscribers func(SSEEvent) // called when publishing with zero subscribers
	mu              sync.RWMutex
}

// NewSSEBroker creates a new SSE broker.
func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		subscribers: make(map[chan SSEEvent]struct{}),
	}
}

// OnNoSubscribers sets a callback invoked when Publish is called
// but there are no active SSE subscribers (e.g., all browser tabs closed).
func (b *SSEBroker) OnNoSubscribers(fn func(SSEEvent)) {
	b.onNoSubscribers = fn
}

// Subscribe creates a new subscriber channel.
func (b *SSEBroker) Subscribe() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (b *SSEBroker) Unsubscribe(ch chan SSEEvent) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	// Drain remaining events to prevent goroutine leak
	for range ch {
	}
}

// Publish sends an event to all subscribers.
// If there are no subscribers and onNoSubscribers is set, it calls that instead.
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
			// Drop events if subscriber is too slow
		}
	}
}

// SubscriberCount returns the number of active subscribers.
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
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Start SignalR and FCM on first SSE connection
	s.ensureSignalR(session, r.Context())
	s.sessions.StartFCM(session)

	ch := session.SSE.Subscribe()
	defer session.SSE.Unsubscribe(ch)

	// Send initial connected event
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

// ensureSignalR starts SignalR for the session if not already started.
func (s *Server) ensureSignalR(session *UserSession, ctx context.Context) {
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.signalRStarted {
		return
	}

	srCtx, cancel := context.WithCancel(context.Background())
	session.signalRCancel = cancel

	go func() {
		s.logger.Info("Starting SignalR", "phone", session.Phone)
		if err := session.SignalR.Start(srCtx); err != nil {
			s.logger.Error("SignalR start failed", "phone", session.Phone, "error", err)
			session.SSE.Publish(SSEEvent{Type: "error", Data: map[string]string{"message": "SignalR connection failed: " + err.Error()}})
			session.mu.Lock()
			session.signalRStarted = false
			session.mu.Unlock()
			return
		}
		s.logger.Info("SignalR connected", "phone", session.Phone)
	}()

	session.signalRStarted = true
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
