package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// webauthnUser wraps UserAccount to implement the webauthn.User interface.
type webauthnUser struct {
	acct *UserAccount
}

func (u *webauthnUser) WebAuthnID() []byte {
	return []byte(u.acct.Phone)
}

func (u *webauthnUser) WebAuthnName() string {
	return u.acct.Phone
}

func (u *webauthnUser) WebAuthnDisplayName() string {
	return u.acct.Phone
}

func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	u.acct.credMu.RLock()
	defer u.acct.credMu.RUnlock()
	creds := make([]webauthn.Credential, len(u.acct.WebAuthnCreds))
	copy(creds, u.acct.WebAuthnCreds)
	return creds
}

// pendingWebAuthn stores in-flight registration/login ceremonies.
var (
	pendingCeremonies   = map[string]*webauthn.SessionData{} // keyed by phone
	pendingCeremoniesMu sync.Mutex
)

func storeCeremony(phone string, data *webauthn.SessionData) {
	pendingCeremoniesMu.Lock()
	pendingCeremonies[phone] = data
	pendingCeremoniesMu.Unlock()
}

func popCeremony(phone string) *webauthn.SessionData {
	pendingCeremoniesMu.Lock()
	data := pendingCeremonies[phone]
	delete(pendingCeremonies, phone)
	pendingCeremoniesMu.Unlock()
	return data
}

// InitWebAuthn creates a WebAuthn instance from the given origin URL.
// Returns nil if origin is empty or invalid.
func InitWebAuthn(origin string) *webauthn.WebAuthn {
	if origin == "" {
		return nil
	}

	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return nil
	}

	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Garmin Messenger",
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
		AttestationPreference: protocol.PreferNoAttestation,
	})
	if err != nil {
		return nil
	}
	return wa
}

// --- Registration: called after successful OTP login (session required) ---

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	user := &webauthnUser{acct: session.Account}
	creation, sessionData, err := s.webAuthn.BeginRegistration(user)
	if err != nil {
		s.logger.Error("Passkey registration begin failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start passkey registration"})
		return
	}

	storeCeremony(session.Account.Phone, sessionData)
	writeJSON(w, http.StatusOK, creation)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	sessionData := popCeremony(session.Account.Phone)
	if sessionData == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending registration"})
		return
	}

	user := &webauthnUser{acct: session.Account}
	cred, err := s.webAuthn.FinishRegistration(user, *sessionData, r)
	if err != nil {
		s.logger.Error("Passkey registration finish failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passkey registration failed: " + err.Error()})
		return
	}

	session.Account.credMu.Lock()
	session.Account.WebAuthnCreds = append(session.Account.WebAuthnCreds, *cred)
	session.Account.credMu.Unlock()

	s.PersistSessions()
	s.logger.Info("Passkey registered", "phone", session.Account.Phone)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Login: called when account has passkey credentials (no session required) ---

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Phone == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone is required"})
		return
	}

	if s.phoneWhitelist != nil && !s.phoneWhitelist[req.Phone] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this phone number is not allowed"})
		return
	}

	acct := s.sessions.GetAccount(req.Phone)
	if acct == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no active account"})
		return
	}

	acct.credMu.RLock()
	hasCreds := len(acct.WebAuthnCreds) > 0
	acct.credMu.RUnlock()

	if !hasCreds {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no passkeys registered"})
		return
	}

	user := &webauthnUser{acct: acct}
	assertion, sessionData, err := s.webAuthn.BeginLogin(user)
	if err != nil {
		s.logger.Error("Passkey login begin failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start passkey login"})
		return
	}

	storeCeremony(req.Phone, sessionData)
	writeJSON(w, http.StatusOK, assertion)
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	// The phone is encoded in the ceremony session, but we need it to look up the account.
	// We pass it as a query param since the body is the authenticator response.
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone is required"})
		return
	}

	if s.phoneWhitelist != nil && !s.phoneWhitelist[phone] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this phone number is not allowed"})
		return
	}

	sessionData := popCeremony(phone)
	if sessionData == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending login ceremony"})
		return
	}

	acct := s.sessions.GetAccount(phone)
	if acct == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no active account"})
		return
	}

	user := &webauthnUser{acct: acct}
	cred, err := s.webAuthn.FinishLogin(user, *sessionData, r)
	if err != nil {
		s.logger.Warn("Passkey login failed", "phone", phone, "error", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passkey verification failed"})
		return
	}

	// Update credential (counter etc.)
	acct.credMu.Lock()
	for i, c := range acct.WebAuthnCreds {
		if string(c.ID) == string(cred.ID) {
			acct.WebAuthnCreds[i] = *cred
			break
		}
	}
	acct.credMu.Unlock()

	// Create a new browser session
	session, err := s.sessions.CreateSession(phone, acct.Auth, s.logger)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}

	SetSessionCookie(w, session.ID, s.sessions.sessionDays)
	s.PersistSessions()

	s.logger.Info("Passkey login successful", "phone", phone)
	userID := gm.PhoneToHermesUserID(phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &phone,
		UserID:   &userID,
	})
}
