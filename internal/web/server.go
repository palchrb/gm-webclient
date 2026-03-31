package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
)

//go:embed static
var staticFiles embed.FS

// Server is the HTTP server for the Garmin Messenger web client.
type Server struct {
	sessions       *SessionManager
	vapidKeys      *VAPIDKeys
	pushStore      *PushSubscriptionStore
	logger         *slog.Logger
	mux            *http.ServeMux
	phoneWhitelist map[string]bool // nil = allow all, non-nil = only listed phones
}

// ServerOption configures the Server.
type ServerOption func(*Server)

// WithPhoneWhitelist restricts login to the specified phone numbers.
// Phone numbers should include the country code (e.g. "+4712345678").
// An empty list disables the whitelist (allows all).
func WithPhoneWhitelist(phones []string) ServerOption {
	return func(s *Server) {
		if len(phones) == 0 {
			return
		}
		s.phoneWhitelist = make(map[string]bool, len(phones))
		for _, p := range phones {
			s.phoneWhitelist[p] = true
		}
	}
}

// WithSessionDays sets the cookie/session TTL in days.
func WithSessionDays(days int) ServerOption {
	return func(s *Server) {
		if days > 0 {
			s.sessions.sessionDays = days
		}
	}
}

// NewServer creates a new web server.
// dataDir is the base directory for persistent data (FCM creds, VAPID keys, push subscriptions).
// Empty disables FCM and push.
func NewServer(logger *slog.Logger, dataDir string, vapidKeys *VAPIDKeys, opts ...ServerOption) *Server {
	var pushStore *PushSubscriptionStore
	if dataDir != "" {
		pushStore = NewPushSubscriptionStore(dataDir)
	}

	s := &Server{
		sessions:  NewSessionManager(logger, dataDir, defaultSessionDays),
		vapidKeys: vapidKeys,
		pushStore: pushStore,
		logger:    logger,
		mux:       http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Static files (embedded)
	staticFS, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))

	// Auth endpoints (no session required)
	s.mux.HandleFunc("POST /api/auth/request-otp", s.handleRequestOTP)
	s.mux.HandleFunc("POST /api/auth/confirm-otp", s.handleConfirmOTP)
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/logout", s.requireSession(s.handleLogout))

	// API endpoints (session required)
	s.mux.HandleFunc("GET /api/conversations", s.requireSession(s.handleGetConversations))
	s.mux.HandleFunc("GET /api/conversations/{id}", s.requireSession(s.handleGetConversationDetail))
	s.mux.HandleFunc("GET /api/conversations/{id}/members", s.requireSession(s.handleGetConversationMembers))
	s.mux.HandleFunc("POST /api/conversations/{id}/leave", s.requireSession(s.handleLeaveConversation))
	s.mux.HandleFunc("POST /api/messages/send", s.requireSession(s.handleSendMessage))
	s.mux.HandleFunc("POST /api/messages/{convId}/{msgId}/read", s.requireSession(s.handleMarkAsRead))
	s.mux.HandleFunc("GET /api/media", s.requireSession(s.handleGetMediaURL))
	s.mux.HandleFunc("GET /api/media/proxy", s.requireSession(s.handleProxyMedia))
	s.mux.HandleFunc("POST /api/media/send", s.requireSession(s.handleSendMedia))
	s.mux.HandleFunc("POST /api/chat/new", s.requireSession(s.handleNewChat))

	// Push notification endpoints
	s.mux.HandleFunc("GET /api/push/vapid-key", s.handleGetVAPIDKey)
	s.mux.HandleFunc("POST /api/push/subscribe", s.requireSession(s.handlePushSubscribe))
	s.mux.HandleFunc("DELETE /api/push/subscribe", s.requireSession(s.handlePushUnsubscribe))

	// SSE events (session required)
	s.mux.HandleFunc("GET /api/events", s.requireSession(s.handleSSE))

	// Serve static files for everything else
	s.mux.Handle("/", fileServer)
}

// requireSession wraps a handler that needs an authenticated session.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := s.sessions.GetFromRequest(r)
		if session == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		session.Touch()
		r = r.WithContext(withSession(r.Context(), session))
		next(w, r)
	}
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}
