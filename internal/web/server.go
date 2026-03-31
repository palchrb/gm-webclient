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
	sessions *SessionManager
	logger   *slog.Logger
	mux      *http.ServeMux
}

// NewServer creates a new web server.
func NewServer(logger *slog.Logger) *Server {
	s := &Server{
		sessions: NewSessionManager(logger),
		logger:   logger,
		mux:      http.NewServeMux(),
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
	s.mux.HandleFunc("POST /api/messages/send", s.requireSession(s.handleSendMessage))
	s.mux.HandleFunc("POST /api/messages/{convId}/{msgId}/read", s.requireSession(s.handleMarkAsRead))
	s.mux.HandleFunc("GET /api/media", s.requireSession(s.handleGetMediaURL))
	s.mux.HandleFunc("POST /api/chat/new", s.requireSession(s.handleNewChat))

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
