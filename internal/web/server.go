package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/go-webauthn/webauthn/webauthn"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

//go:embed static
var staticFiles embed.FS

// Server is the HTTP server for the Garmin Messenger web client.
type Server struct {
	sessions       *SessionManager
	sessionStore   *SessionStore // nil when SESSION_KEY is not set
	vapidKeys      *VAPIDKeys
	pushStore      *PushSubscriptionStore
	passkeyStore   *PasskeyStore          // nil when dataDir is empty
	ntfyStore      *NtfyStore             // nil when dataDir is empty
	webAuthn       *webauthn.WebAuthn     // nil when ORIGIN is not set
	ntfyConfig     *NtfyConfig            // nil when NTFY_URL is not set
	pushAlways     bool                   // send web push even when browser tabs are open
	logger         *slog.Logger
	mux            *http.ServeMux
	phoneWhitelist map[string]bool // nil = allow all, non-nil = only listed phones
}

// ServerOption configures the Server.
type ServerOption func(*Server)

// WithPhoneWhitelist restricts login to the specified phone numbers.
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

// WithPushAlways controls whether web push notifications are sent even when
// browser tabs are open. When true (default), push is always sent.
func WithPushAlways(always bool) ServerOption {
	return func(s *Server) {
		s.pushAlways = always
	}
}

// WithNtfyConfig enables ntfy.sh push notification forwarding.
func WithNtfyConfig(cfg *NtfyConfig) ServerOption {
	return func(s *Server) {
		s.ntfyConfig = cfg
	}
}

// WithOrigin configures WebAuthn passkey support using the given origin URL
// (e.g. "https://garmin.tailnet.ts.net" or "http://localhost:8080").
func WithOrigin(origin string) ServerOption {
	return func(s *Server) {
		wa := InitWebAuthn(origin)
		if wa != nil {
			s.webAuthn = wa
			s.logger.Info("Passkey (WebAuthn) support enabled", "origin", origin, "rpId", wa.Config.RPID)
		} else if origin != "" {
			s.logger.Warn("Invalid origin for WebAuthn, passkeys disabled", "origin", origin)
		}
	}
}

// WithSessionKey enables encrypted session persistence using the given key.
func WithSessionKey(key string) ServerOption {
	return func(s *Server) {
		if key == "" || s.sessions.fcmDataDir == "" {
			return
		}
		store, err := NewSessionStore(s.sessions.fcmDataDir, key, s.logger)
		if err != nil {
			s.logger.Error("Failed to initialize session store", "error", err)
			return
		}
		s.sessionStore = store
	}
}

// NewServer creates a new web server.
func NewServer(logger *slog.Logger, dataDir string, vapidKeys *VAPIDKeys, opts ...ServerOption) *Server {
	var pushStore *PushSubscriptionStore
	var passkeyStore *PasskeyStore
	var ntfyStore *NtfyStore
	if dataDir != "" {
		pushStore = NewPushSubscriptionStore(dataDir)
		passkeyStore = NewPasskeyStore(dataDir)
		ntfyStore = NewNtfyStore(dataDir)
	}

	s := &Server{
		sessions:     NewSessionManager(logger, dataDir, defaultSessionDays),
		vapidKeys:    vapidKeys,
		pushStore:    pushStore,
		passkeyStore: passkeyStore,
		ntfyStore:    ntfyStore,
		pushAlways:   true,
		logger:       logger,
		mux:          http.NewServeMux(),
	}
	if ntfyStore != nil {
		s.sessions.SetNtfyStore(ntfyStore)
	}
	for _, opt := range opts {
		opt(s)
	}

	// Restore encrypted sessions from disk if SESSION_KEY is configured
	if s.sessionStore != nil {
		n := s.sessions.RestoreSessions(s.sessionStore, logger)
		if n > 0 {
			logger.Info("Restored encrypted sessions", "count", n)
			s.wireRestoredAccounts()
		}
	}

	s.registerRoutes()
	return s
}

// wirePushCallback sets the correct push callback on an account's SSE broker.
// Deduplicates by messageId across SignalR + FCM so each message generates
// at most one push notification (web push + ntfy) within a short window.
func (s *Server) wirePushCallback(acct *UserAccount) {
	pushFn := func(event SSEEvent) {
		if msg, ok := event.Data.(gm.MessageModel); ok {
			if !acct.pushDedup.shouldSend(msg.MessageID.String()) {
				s.logger.Debug("Push dedup: skipping duplicate", "phone", acct.Phone, "messageId", msg.MessageID)
				return
			}
		}
		s.sendWebPush(acct, event)
		s.sendNtfy(acct, event)
	}
	if s.pushAlways {
		acct.SSE.OnEveryPublish(pushFn)
	} else {
		acct.SSE.OnNoSubscribers(pushFn)
	}
}

// wireRestoredAccounts sets up push callbacks and loads push subscriptions
// for accounts restored from encrypted storage.
func (s *Server) wireRestoredAccounts() {
	s.sessions.mu.RLock()
	defer s.sessions.mu.RUnlock()

	for _, acct := range s.sessions.accounts {
		phone := acct.Phone
		if s.pushStore != nil {
			acct.pushMu.Lock()
			if acct.PushSubscriptions == nil {
				acct.PushSubscriptions = s.pushStore.Load(phone)
			}
			acct.pushMu.Unlock()
		}
		s.wirePushCallback(acct)
		s.sessions.EnsureFCM(acct)
		s.logger.Info("Wired push + started FCM for restored account", "phone", phone)
	}
}

// PersistSessions saves current sessions to encrypted storage (if enabled).
func (s *Server) PersistSessions() {
	if s.sessionStore != nil {
		s.sessions.persistSessions(s.sessionStore)
	}
}

func (s *Server) registerRoutes() {
	// Static files (embedded)
	staticFS, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))

	// Auth endpoints (no session required)
	s.mux.HandleFunc("POST /api/auth/request-otp", s.handleRequestOTP)
	s.mux.HandleFunc("POST /api/auth/confirm-otp", s.handleConfirmOTP)
	s.mux.HandleFunc("POST /api/auth/request-reauth-otp", s.handleRequestReauthOTP)
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/logout", s.requireSession(s.handleLogout))
	s.mux.HandleFunc("POST /api/auth/logout-all", s.requireSession(s.handleLogoutAll))

	// API endpoints (session required)
	s.mux.HandleFunc("GET /api/conversations", s.requireSession(s.handleGetConversations))
	s.mux.HandleFunc("GET /api/conversations/{id}", s.requireSession(s.handleGetConversationDetail))
	s.mux.HandleFunc("GET /api/conversations/{id}/members", s.requireSession(s.handleGetConversationMembers))
	s.mux.HandleFunc("POST /api/conversations/{id}/leave", s.requireSession(s.handleLeaveConversation))
	s.mux.HandleFunc("POST /api/messages/send", s.requireSession(s.handleSendMessage))
	s.mux.HandleFunc("POST /api/messages/react", s.requireSession(s.handleSendReaction))
	s.mux.HandleFunc("POST /api/messages/{convId}/{msgId}/read", s.requireSession(s.handleMarkAsRead))
	s.mux.HandleFunc("GET /api/media", s.requireSession(s.handleGetMediaURL))
	s.mux.HandleFunc("GET /api/media/proxy", s.requireSession(s.handleProxyMedia))
	s.mux.HandleFunc("POST /api/media/send", s.requireSession(s.handleSendMedia))
	s.mux.HandleFunc("POST /api/chat/new", s.requireSession(s.handleNewChat))

	// Passkey (WebAuthn) endpoints
	if s.webAuthn != nil && s.passkeyStore != nil {
		s.mux.HandleFunc("POST /api/passkey/register/begin", s.requireSession(s.handlePasskeyRegisterBegin))
		s.mux.HandleFunc("POST /api/passkey/register/finish", s.requireSession(s.handlePasskeyRegisterFinish))
		s.mux.HandleFunc("POST /api/passkey/login/begin", s.handlePasskeyLoginBegin)
		s.mux.HandleFunc("POST /api/passkey/login/finish", s.handlePasskeyLoginFinish)
	}

	// Push notification endpoints
	s.mux.HandleFunc("GET /api/push/vapid-key", s.handleGetVAPIDKey)
	s.mux.HandleFunc("POST /api/push/subscribe", s.requireSession(s.handlePushSubscribe))
	s.mux.HandleFunc("DELETE /api/push/subscribe", s.requireSession(s.handlePushUnsubscribe))

	// ntfy push notification endpoints
	s.mux.HandleFunc("GET /api/ntfy/info", s.requireSession(s.handleGetNtfyInfo))
	s.mux.HandleFunc("POST /api/ntfy/subscribe", s.requireSession(s.handleNtfySubscribe))

	// SSE events (session required)
	s.mux.HandleFunc("GET /api/events", s.requireSession(s.handleSSE))
	s.mux.HandleFunc("POST /api/session/visibility", s.requireSession(s.handleSetVisibility))

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
