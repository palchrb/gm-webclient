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
	signalRIdleTimeout = 2 * time.Minute // pause SignalR when no browser is listening
	reaperInterval     = 30 * time.Second
	defaultSessionDays = 30

	fcmInitialBackoff = 5 * time.Second
	fcmMaxBackoff     = 5 * time.Minute
)

type contextKey string

const sessionContextKey contextKey = "session"

// ---------------------------------------------------------------------------
// UserAccount — one per phone number, shared across all browser sessions
// ---------------------------------------------------------------------------

// UserAccount holds Garmin connectivity shared across all browser sessions
// for the same phone number. One SignalR + one FCM connection per account.
type UserAccount struct {
	Phone   string
	Auth    *gm.HermesAuth
	API     *gm.HermesAPI
	SignalR *gm.HermesSignalR
	FCM     *fcm.Client
	SSE     *SSEBroker // shared: all sessions for this phone receive events

	PushSubscriptions map[string]*webpush.Subscription
	pushMu            sync.RWMutex

	PINHash []byte // bcrypt hash, empty if no PIN set yet

	mu             sync.Mutex
	signalRCancel  context.CancelFunc
	fcmCancel      context.CancelFunc
	signalRStarted bool
	fcmStarted     bool
}

// ---------------------------------------------------------------------------
// UserSession — one per browser cookie, lightweight
// ---------------------------------------------------------------------------

// UserSession is a browser login. Multiple sessions can share one UserAccount.
type UserSession struct {
	ID           string
	Account      *UserAccount
	LastActivity time.Time
	mu           sync.Mutex
}

// Touch updates the last activity time.
func (s *UserSession) Touch() {
	s.mu.Lock()
	s.LastActivity = time.Now()
	s.mu.Unlock()
}

// Convenience accessors so the rest of the codebase doesn't change much.
func (s *UserSession) Phone() string            { return s.Account.Phone }
func (s *UserSession) AuthObj() *gm.HermesAuth  { return s.Account.Auth }
func (s *UserSession) APIObj() *gm.HermesAPI    { return s.Account.API }
func (s *UserSession) SSEBroker() *SSEBroker    { return s.Account.SSE }

// PendingOTP represents a pending OTP request before a session is created.
type PendingOTP struct {
	OtpReq    *gm.OtpRequest
	Auth      *gm.HermesAuth
	CreatedAt time.Time
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

// SessionManager manages accounts (per phone) and sessions (per browser).
type SessionManager struct {
	accounts    map[string]*UserAccount  // keyed by phone number
	sessions    map[string]*UserSession  // keyed by session ID (cookie)
	pendingOTP  map[string]*PendingOTP   // keyed by phone number
	fcmDataDir  string
	sessionDays int
	mu          sync.RWMutex
	logger      *slog.Logger
}

func NewSessionManager(logger *slog.Logger, fcmDataDir string, sessionDays int) *SessionManager {
	if sessionDays <= 0 {
		sessionDays = defaultSessionDays
	}
	sm := &SessionManager{
		accounts:    make(map[string]*UserAccount),
		sessions:    make(map[string]*UserSession),
		pendingOTP:  make(map[string]*PendingOTP),
		fcmDataDir:  fcmDataDir,
		sessionDays: sessionDays,
		logger:      logger,
	}
	go sm.reaper()
	return sm
}

// getOrCreateAccount returns the existing account for a phone, or creates one.
// A fresh OTP login ALWAYS replaces the existing account's credentials and
// restarts connections. This ensures we never get stuck on stale tokens
// (e.g. after external device deletion via iOS app).
func (sm *SessionManager) getOrCreateAccount(phone string, auth *gm.HermesAuth, logger *slog.Logger) *UserAccount {
	if acct, ok := sm.accounts[phone]; ok {
		// If auth is the same object (e.g. additional session for same account
		// during restore), just reuse — no need to restart connections.
		if acct.Auth == auth {
			return acct
		}

		// Fresh OTP login — update credentials and restart connections.
		sm.logger.Info("Updating account with fresh credentials from new login", "phone", phone,
			"oldInstance", acct.Auth.InstanceID, "newInstance", auth.InstanceID)

		// Stop old connections before swapping
		acct.SignalR.Stop()
		if acct.signalRCancel != nil {
			acct.signalRCancel()
		}
		if acct.fcmCancel != nil {
			acct.fcmCancel()
		}

		hermesLogger := logger.With("component", "hermes", "phone", phone)
		acct.mu.Lock()
		acct.Auth = auth
		acct.API = gm.NewHermesAPI(auth, gm.WithAPILogger(hermesLogger))
		acct.SignalR = gm.NewHermesSignalR(auth, gm.WithSignalRLogger(hermesLogger))
		acct.signalRStarted = false
		acct.fcmStarted = false
		acct.mu.Unlock()

		sm.wireAccountEvents(acct, logger)
		return acct
	}

	hermesLogger := logger.With("component", "hermes", "phone", phone)
	api := gm.NewHermesAPI(auth, gm.WithAPILogger(hermesLogger))
	sr := gm.NewHermesSignalR(auth, gm.WithSignalRLogger(hermesLogger))

	var fcmClient *fcm.Client
	if sm.fcmDataDir != "" {
		fcmSessionDir := filepath.Join(sm.fcmDataDir, "fcm", phone)
		fcmClient = fcm.NewClient(fcmSessionDir,
			fcm.WithLogger(logger.With("component", "fcm", "phone", phone)),
		)
	}

	acct := &UserAccount{
		Phone:   phone,
		Auth:    auth,
		API:     api,
		SignalR: sr,
		FCM:     fcmClient,
		SSE:     NewSSEBroker(),
	}

	sm.wireAccountEvents(acct, logger)
	sm.accounts[phone] = acct
	sm.logger.Info("Account created", "phone", phone)
	return acct
}

// wireAccountEvents sets up SignalR and FCM event callbacks on the account's SSE broker.
func (sm *SessionManager) wireAccountEvents(acct *UserAccount, logger *slog.Logger) {
	phone := acct.Phone

	acct.SignalR.OnMessage(func(msg gm.MessageModel) {
		acct.SSE.Publish(SSEEvent{Type: "message", Data: msg})
	})
	acct.SignalR.OnStatusUpdate(func(update gm.MessageStatusUpdate) {
		acct.SSE.Publish(SSEEvent{Type: "status", Data: update})
	})
	acct.SignalR.OnMuteUpdate(func(update gm.ConversationMuteStatusUpdate) {
		acct.SSE.Publish(SSEEvent{Type: "mute", Data: update})
	})
	acct.SignalR.OnBlockUpdate(func(update gm.UserBlockStatusUpdate) {
		acct.SSE.Publish(SSEEvent{Type: "block", Data: update})
	})
	acct.SignalR.OnNotification(func(notif gm.ServerNotification) {
		acct.SSE.Publish(SSEEvent{Type: "notification", Data: notif})
	})
	acct.SignalR.OnOpen(func() {
		acct.SSE.Publish(SSEEvent{Type: "connected", Data: nil})
	})
	acct.SignalR.OnClose(func() {
		acct.SSE.Publish(SSEEvent{Type: "disconnected", Data: nil})
	})

	if acct.FCM != nil {
		// Deduplicate FCM messages — Garmin sends the same message via
		// multiple FCM channels, producing 5+ duplicates per message.
		lastFCMMessageID := ""
		acct.FCM.OnMessage(func(msg fcm.NewMessage) {
			msgID := msg.MessageID.String()
			if msgID == lastFCMMessageID {
				sm.logger.Debug("FCM→SSE: skipping duplicate", "messageId", msgID)
				return
			}
			lastFCMMessageID = msgID
			sm.logger.Info("FCM→SSE: publishing new message",
				"phone", phone,
				"messageId", msgID,
				"sseSubscribers", acct.SSE.SubscriberCount(),
			)
			acct.SSE.Publish(SSEEvent{Type: "message", Data: msg.MessageModel})
		})
		acct.FCM.OnConnected(func() {
			sm.logger.Debug("FCM connected", "phone", phone)
		})
		acct.FCM.OnDisconnected(func() {
			sm.logger.Debug("FCM disconnected", "phone", phone)
		})
		acct.FCM.OnError(func(err error) {
			sm.logger.Error("FCM error", "phone", phone, "error", err)
		})
	}
}

// CreateSession creates a new browser session, reusing or creating an account.
func (sm *SessionManager) CreateSession(phone string, auth *gm.HermesAuth, logger *slog.Logger) (*UserSession, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	sessionID := hex.EncodeToString(tokenBytes)

	sm.mu.Lock()
	acct := sm.getOrCreateAccount(phone, auth, logger)
	session := &UserSession{
		ID:           sessionID,
		Account:      acct,
		LastActivity: time.Now(),
	}
	sm.sessions[sessionID] = session
	sm.mu.Unlock()

	sm.logger.Info("Session created", "phone", phone, "sessionId", sessionID[:8]+"...")
	return session, nil
}

// EnsureSignalR starts SignalR for the account if not already started.
func (sm *SessionManager) EnsureSignalR(acct *UserAccount) {
	acct.mu.Lock()
	defer acct.mu.Unlock()

	if acct.signalRStarted {
		return
	}

	srCtx, cancel := context.WithCancel(context.Background())
	acct.signalRCancel = cancel

	go func() {
		sm.logger.Info("Starting SignalR", "phone", acct.Phone)
		if err := acct.SignalR.Start(srCtx); err != nil {
			sm.logger.Error("SignalR start failed", "phone", acct.Phone, "error", err)
			acct.SSE.Publish(SSEEvent{Type: "error", Data: map[string]string{"message": "SignalR connection failed: " + err.Error()}})
			acct.mu.Lock()
			acct.signalRStarted = false
			acct.mu.Unlock()
			return
		}
		sm.logger.Info("SignalR connected", "phone", acct.Phone)
	}()

	acct.signalRStarted = true
}

// EnsureFCM starts FCM for the account if not already started.
func (sm *SessionManager) EnsureFCM(acct *UserAccount) {
	acct.mu.Lock()
	if acct.fcmStarted || acct.FCM == nil {
		acct.mu.Unlock()
		return
	}
	acct.fcmStarted = true
	fcmCtx, cancel := context.WithCancel(context.Background())
	acct.fcmCancel = cancel
	acct.mu.Unlock()

	go func() {
		phone := acct.Phone

		fcmToken, err := acct.FCM.Register(fcmCtx)
		if err != nil {
			sm.logger.Error("FCM registration failed", "phone", phone, "error", err)
			acct.mu.Lock()
			acct.fcmStarted = false
			acct.mu.Unlock()
			return
		}

		if err := acct.Auth.UpdatePnsHandle(fcmCtx, fcmToken); err != nil {
			sm.logger.Warn("Failed to register FCM token with Garmin", "phone", phone, "error", err)
		} else {
			sm.logger.Info("FCM token registered with Garmin", "phone", phone)
		}

		backoff := fcmInitialBackoff
		for {
			select {
			case <-fcmCtx.Done():
				return
			default:
			}

			sm.logger.Info("Starting FCM listener", "phone", phone)
			err := acct.FCM.Listen(fcmCtx)
			if fcmCtx.Err() != nil {
				return
			}

			sm.logger.Warn("FCM listener disconnected, will reconnect",
				"phone", phone, "error", err, "backoff", backoff)

			select {
			case <-fcmCtx.Done():
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

// GetAccount returns the active account for a phone, or nil.
func (sm *SessionManager) GetAccount(phone string) *UserAccount {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.accounts[phone]
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

// RemoveSession removes a browser session. If it's the last session for the
// account, the account's SignalR and FCM are stopped too.
func (sm *SessionManager) RemoveSession(sessionID string) {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	if !ok {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, sessionID)
	phone := session.Account.Phone

	// Check if any other sessions still reference this account
	accountInUse := false
	for _, s := range sm.sessions {
		if s.Account.Phone == phone {
			accountInUse = true
			break
		}
	}

	if !accountInUse {
		sm.stopAccount(session.Account)
		delete(sm.accounts, phone)
	}
	sm.mu.Unlock()

	sm.logger.Info("Session removed", "phone", phone, "accountRemoved", !accountInUse)
}

// stopAccount shuts down all connections for an account and deregisters with Garmin.
func (sm *SessionManager) stopAccount(acct *UserAccount) {
	acct.SignalR.Stop()
	if acct.signalRCancel != nil {
		acct.signalRCancel()
	}
	if acct.fcmCancel != nil {
		acct.fcmCancel()
	}

	// Deregister with Garmin so the app instance doesn't pile up
	// in the user's device list on the iOS/Android Garmin Messenger app.
	if acct.Auth.InstanceID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := acct.Auth.DeleteAppRegistration(ctx, acct.Auth.InstanceID); err != nil {
			sm.logger.Warn("Failed to deregister with Garmin on logout", "phone", acct.Phone, "error", err)
		} else {
			sm.logger.Info("Deregistered with Garmin", "phone", acct.Phone)
		}
	}

	sm.logger.Info("Account stopped", "phone", acct.Phone)
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
func SetSessionCookie(w http.ResponseWriter, sessionID string, maxAgeDays int) {
	if maxAgeDays <= 0 {
		maxAgeDays = defaultSessionDays
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * maxAgeDays,
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

func withSession(ctx context.Context, session *UserSession) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

func getSession(ctx context.Context) *UserSession {
	session, _ := ctx.Value(sessionContextKey).(*UserSession)
	return session
}

// ---------------------------------------------------------------------------
// Reaper
// ---------------------------------------------------------------------------

func (sm *SessionManager) reaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		sm.mu.Lock()
		now := time.Now()
		sessionTTL := time.Duration(sm.sessionDays) * 24 * time.Hour

		// Expire old sessions
		for id, session := range sm.sessions {
			session.mu.Lock()
			idle := now.Sub(session.LastActivity)
			session.mu.Unlock()

			if idle > sessionTTL {
				sm.logger.Info("Session expired", "phone", session.Account.Phone, "idle", idle)
				delete(sm.sessions, id)
			}
		}

		// For each account, check if it still has active sessions
		for phone, acct := range sm.accounts {
			hasSession := false
			for _, s := range sm.sessions {
				if s.Account.Phone == phone {
					hasSession = true
					break
				}
			}

			if !hasSession {
				// No sessions left — stop everything
				sm.stopAccount(acct)
				delete(sm.accounts, phone)
				continue
			}

			// If no SSE subscribers (all browser tabs closed), pause SignalR
			// but keep FCM for Web Push
			if acct.SSE.SubscriberCount() == 0 && acct.signalRStarted {
				sm.logger.Info("Pausing SignalR (no browser tabs, FCM stays for push)", "phone", phone, "sseSubscribers", 0)
				acct.SignalR.Stop()
				if acct.signalRCancel != nil {
					acct.signalRCancel()
				}
				acct.mu.Lock()
				acct.signalRStarted = false
				acct.mu.Unlock()
			}
		}

		// Clean up old pending OTPs
		for phone, pending := range sm.pendingOTP {
			if now.Sub(pending.CreatedAt) > 5*time.Minute {
				delete(sm.pendingOTP, phone)
			}
		}
		sm.mu.Unlock()
	}
}

func ensureFCMDataDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
