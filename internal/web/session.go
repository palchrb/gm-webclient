package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
	"github.com/yourusername/matrix-garmin-messenger/internal/hermes/fcm"
)

const (
	sessionCookieName = "garmin_session"
	sessionMaxIdle    = 30 * time.Minute
	reaperInterval    = 5 * time.Minute

	// FCM reconnect backoff parameters
	fcmInitialBackoff = 5 * time.Second
	fcmMaxBackoff     = 5 * time.Minute
)

type contextKey string

const sessionContextKey contextKey = "session"

// UserSession represents a logged-in user's session.
type UserSession struct {
	ID                string
	Phone             string
	Auth              *gm.HermesAuth
	API               *gm.HermesAPI
	SignalR           *gm.HermesSignalR
	FCM               *fcm.Client
	SSE               *SSEBroker
	PushSubscriptions map[string]*webpush.Subscription
	LastActivity      time.Time
	mu                sync.Mutex
	pushMu            sync.RWMutex
	cancel            context.CancelFunc
	signalRStarted    bool
	fcmStarted        bool
}

// Touch updates the last activity time.
func (s *UserSession) Touch() {
	s.mu.Lock()
	s.LastActivity = time.Now()
	s.mu.Unlock()
}

// PendingOTP represents a pending OTP request before a session is created.
type PendingOTP struct {
	OtpReq    *gm.OtpRequest
	Auth      *gm.HermesAuth
	CreatedAt time.Time
}

// SessionManager manages user sessions.
type SessionManager struct {
	sessions   map[string]*UserSession
	pendingOTP map[string]*PendingOTP // keyed by phone number
	fcmDataDir string                 // base dir for FCM credential persistence
	mu         sync.RWMutex
	logger     *slog.Logger
}

// NewSessionManager creates a new session manager and starts the reaper.
func NewSessionManager(logger *slog.Logger, fcmDataDir string) *SessionManager {
	sm := &SessionManager{
		sessions:   make(map[string]*UserSession),
		pendingOTP: make(map[string]*PendingOTP),
		fcmDataDir: fcmDataDir,
		logger:     logger,
	}
	go sm.reaper()
	return sm
}

// CreateSession creates a new user session after successful OTP confirmation.
func (sm *SessionManager) CreateSession(phone string, auth *gm.HermesAuth, logger *slog.Logger) (*UserSession, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	sessionID := hex.EncodeToString(tokenBytes)

	hermesLogger := logger.With("component", "hermes", "phone", phone)
	api := gm.NewHermesAPI(auth, gm.WithAPILogger(hermesLogger))
	sr := gm.NewHermesSignalR(auth, gm.WithSignalRLogger(hermesLogger))

	// Create FCM client with persistent storage per phone number.
	// FCM credentials (androidId, securityToken) persist across restarts
	// to avoid Google's registration rate limits. These are device-level
	// credentials for Google's push system, not Garmin auth tokens.
	var fcmClient *fcm.Client
	if sm.fcmDataDir != "" {
		fcmSessionDir := filepath.Join(sm.fcmDataDir, "fcm", phone)
		fcmClient = fcm.NewClient(fcmSessionDir,
			fcm.WithLogger(logger.With("component", "fcm", "phone", phone)),
		)
	}

	session := &UserSession{
		ID:           sessionID,
		Phone:        phone,
		Auth:         auth,
		API:          api,
		SignalR:      sr,
		FCM:          fcmClient,
		SSE:          NewSSEBroker(),
		LastActivity: time.Now(),
	}

	// Wire SignalR events to SSE broker
	sr.OnMessage(func(msg gm.MessageModel) {
		session.SSE.Publish(SSEEvent{Type: "message", Data: msg})
	})
	sr.OnStatusUpdate(func(update gm.MessageStatusUpdate) {
		session.SSE.Publish(SSEEvent{Type: "status", Data: update})
	})
	sr.OnMuteUpdate(func(update gm.ConversationMuteStatusUpdate) {
		session.SSE.Publish(SSEEvent{Type: "mute", Data: update})
	})
	sr.OnBlockUpdate(func(update gm.UserBlockStatusUpdate) {
		session.SSE.Publish(SSEEvent{Type: "block", Data: update})
	})
	sr.OnNotification(func(notif gm.ServerNotification) {
		session.SSE.Publish(SSEEvent{Type: "notification", Data: notif})
	})
	sr.OnOpen(func() {
		session.SSE.Publish(SSEEvent{Type: "connected", Data: nil})
	})
	sr.OnClose(func() {
		session.SSE.Publish(SSEEvent{Type: "disconnected", Data: nil})
	})

	// Wire FCM events to SSE broker (same events, additional delivery path)
	if fcmClient != nil {
		fcmClient.OnMessage(func(msg fcm.NewMessage) {
			session.SSE.Publish(SSEEvent{Type: "message", Data: msg.MessageModel})
		})
		fcmClient.OnConnected(func() {
			sm.logger.Debug("FCM connected", "phone", phone)
		})
		fcmClient.OnDisconnected(func() {
			sm.logger.Debug("FCM disconnected", "phone", phone)
		})
		fcmClient.OnError(func(err error) {
			sm.logger.Error("FCM error", "phone", phone, "error", err)
		})
	}

	sm.mu.Lock()
	sm.sessions[sessionID] = session
	sm.mu.Unlock()

	sm.logger.Info("Session created", "phone", phone, "sessionId", sessionID[:8]+"...")
	return session, nil
}

// StartFCM registers with Google FCM and starts the MCS listener for a session.
// It runs in a goroutine and reconnects with exponential backoff on failure.
func (sm *SessionManager) StartFCM(session *UserSession, ctx context.Context) {
	session.mu.Lock()
	if session.fcmStarted || session.FCM == nil {
		session.mu.Unlock()
		return
	}
	session.fcmStarted = true
	session.mu.Unlock()

	go func() {
		phone := session.Phone

		// Step 1: Register with Google FCM
		fcmToken, err := session.FCM.Register(ctx)
		if err != nil {
			sm.logger.Error("FCM registration failed", "phone", phone, "error", err)
			session.mu.Lock()
			session.fcmStarted = false
			session.mu.Unlock()
			return
		}

		// Step 2: Register FCM token with Garmin's backend
		if err := session.Auth.UpdatePnsHandle(ctx, fcmToken); err != nil {
			sm.logger.Warn("Failed to register FCM token with Garmin", "phone", phone, "error", err)
			// Continue anyway — SignalR still works as primary delivery
		} else {
			sm.logger.Info("FCM token registered with Garmin", "phone", phone)
		}

		// Step 3: Listen with exponential backoff reconnect
		backoff := fcmInitialBackoff
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			sm.logger.Info("Starting FCM listener", "phone", phone)
			err := session.FCM.Listen(ctx)
			if ctx.Err() != nil {
				return // Context cancelled, clean shutdown
			}

			sm.logger.Warn("FCM listener disconnected, will reconnect",
				"phone", phone, "error", err, "backoff", backoff)

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > fcmMaxBackoff {
				backoff = fcmMaxBackoff
			}
		}
	}()
}

// GetSession returns a session by ID.
func (sm *SessionManager) GetSession(sessionID string) *UserSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[sessionID]
}

// GetFromRequest extracts the session from the request cookie.
func (sm *SessionManager) GetFromRequest(r *http.Request) *UserSession {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	return sm.GetSession(cookie.Value)
}

// RemoveSession removes and cleans up a session.
func (sm *SessionManager) RemoveSession(sessionID string) {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	if ok {
		delete(sm.sessions, sessionID)
	}
	sm.mu.Unlock()

	if ok {
		session.SignalR.Stop()
		if session.cancel != nil {
			session.cancel()
		}
		sm.logger.Info("Session removed", "phone", session.Phone)
	}
}

// StorePendingOTP stores a pending OTP request.
func (sm *SessionManager) StorePendingOTP(phone string, otpReq *gm.OtpRequest, auth *gm.HermesAuth) {
	sm.mu.Lock()
	sm.pendingOTP[phone] = &PendingOTP{
		OtpReq:    otpReq,
		Auth:      auth,
		CreatedAt: time.Now(),
	}
	sm.mu.Unlock()
}

// GetPendingOTP returns and removes a pending OTP request.
func (sm *SessionManager) GetPendingOTP(phone string) *PendingOTP {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	pending := sm.pendingOTP[phone]
	if pending != nil {
		delete(sm.pendingOTP, phone)
	}
	return pending
}

// SetSessionCookie sets the session cookie on the response.
func SetSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 30, // 30 days
	})
}

// ClearSessionCookie clears the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// withSession adds the session to the request context.
func withSession(ctx context.Context, session *UserSession) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

// getSession retrieves the session from the request context.
func getSession(ctx context.Context) *UserSession {
	session, _ := ctx.Value(sessionContextKey).(*UserSession)
	return session
}

func generateSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// reaper periodically cleans up idle sessions.
func (sm *SessionManager) reaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		sm.mu.Lock()
		now := time.Now()
		for id, session := range sm.sessions {
			session.mu.Lock()
			idle := now.Sub(session.LastActivity)
			session.mu.Unlock()

			if idle > sessionMaxIdle && session.SSE.SubscriberCount() == 0 {
				sm.logger.Info("Reaping idle session", "phone", session.Phone, "idle", idle)
				session.SignalR.Stop()
				if session.cancel != nil {
					session.cancel()
				}
				delete(sm.sessions, id)
			}
		}

		// Clean up old pending OTPs (older than 5 minutes)
		for phone, pending := range sm.pendingOTP {
			if now.Sub(pending.CreatedAt) > 5*time.Minute {
				delete(sm.pendingOTP, phone)
			}
		}
		sm.mu.Unlock()
	}
}

// ensureFCMDataDir creates the FCM data directory if it doesn't exist.
func ensureFCMDataDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
