package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// webauthnUser implements the webauthn.User interface using the passkey store.
type webauthnUser struct {
	phone string
	store *PasskeyStore
}

func (u *webauthnUser) WebAuthnID() []byte          { return []byte(u.phone) }
func (u *webauthnUser) WebAuthnName() string         { return u.phone }
func (u *webauthnUser) WebAuthnDisplayName() string   { return u.phone }

func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	creds := u.store.Load(u.phone)
	if creds == nil {
		return []webauthn.Credential{}
	}
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

// --- Registration: called after OTP login to set up mandatory passkey ---

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	user := &webauthnUser{phone: session.Account.Phone, store: s.passkeyStore}
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

	phone := session.Account.Phone
	sessionData := popCeremony(phone)
	if sessionData == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending registration"})
		return
	}

	user := &webauthnUser{phone: phone, store: s.passkeyStore}
	cred, err := s.webAuthn.FinishRegistration(user, *sessionData, r)
	if err != nil {
		s.logger.Error("Passkey registration finish failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passkey registration failed: " + err.Error()})
		return
	}

	// Add to existing or replace all credentials
	if r.URL.Query().Get("mode") == "add" {
		existing := s.passkeyStore.Load(phone)
		s.passkeyStore.Save(phone, append(existing, *cred))
	} else {
		s.passkeyStore.Save(phone, []webauthn.Credential{*cred})
	}
	s.logger.Info("Passkey registered", "phone", phone)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Login: passkey authentication (no session required) ---

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

	if !s.passkeyStore.HasCredentials(req.Phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no passkeys registered"})
		return
	}

	user := &webauthnUser{phone: req.Phone, store: s.passkeyStore}
	assertion, sessionData, err := s.webAuthn.BeginLogin(user)
	if err != nil {
		s.logger.Error("Passkey login begin failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start passkey login"})
		return
	}

	storeCeremony(req.Phone, sessionData)
	writeJSON(w, http.StatusOK, assertion)
}

type passkeyLoginResponse struct {
	LoggedIn   bool    `json:"loggedIn"`
	Phone      *string `json:"phone,omitempty"`
	UserID     *string `json:"userId,omitempty"`
	NeedReauth bool    `json:"needReauth,omitempty"` // Garmin tokens expired, need OTP to reconnect
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
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

	user := &webauthnUser{phone: phone, store: s.passkeyStore}
	cred, err := s.webAuthn.FinishLogin(user, *sessionData, r)
	if err != nil {
		s.logger.Warn("Passkey login failed", "phone", phone, "error", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passkey verification failed"})
		return
	}

	// Update credential counter
	creds := s.passkeyStore.Load(phone)
	for i, c := range creds {
		if string(c.ID) == string(cred.ID) {
			creds[i] = *cred
			break
		}
	}
	s.passkeyStore.Save(phone, creds)

	// Check if we have a valid Garmin account (tokens not expired)
	acct := s.sessions.GetAccount(phone)
	if acct != nil {
		// Account exists — check if tokens are still valid
		if !acct.Auth.TokenExpired() {
			// Tokens valid — create session directly
			session, err := s.sessions.CreateSession(phone, acct.Auth, s.logger)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
				return
			}

			if s.pushStore != nil {
				session.Account.pushMu.Lock()
				if session.Account.PushSubscriptions == nil {
					session.Account.PushSubscriptions = s.pushStore.Load(phone)
				}
				session.Account.pushMu.Unlock()
			}
			s.wirePushCallback(session.Account)

			SetSessionCookie(w, session.ID, s.sessions.sessionDays)
			s.PersistSessions()

			s.logger.Info("Passkey login successful", "phone", phone)
			userID := gm.PhoneToHermesUserID(phone)
			writeJSON(w, http.StatusOK, passkeyLoginResponse{
				LoggedIn: true,
				Phone:    &phone,
				UserID:   &userID,
			})
			return
		}

		// Try to refresh
		if err := acct.Auth.RefreshHermesToken(r.Context()); err == nil {
			session, err := s.sessions.CreateSession(phone, acct.Auth, s.logger)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
				return
			}

			if s.pushStore != nil {
				session.Account.pushMu.Lock()
				if session.Account.PushSubscriptions == nil {
					session.Account.PushSubscriptions = s.pushStore.Load(phone)
				}
				session.Account.pushMu.Unlock()
			}
			s.wirePushCallback(session.Account)

			SetSessionCookie(w, session.ID, s.sessions.sessionDays)
			s.PersistSessions()

			s.logger.Info("Passkey login successful (token refreshed)", "phone", phone)
			userID := gm.PhoneToHermesUserID(phone)
			writeJSON(w, http.StatusOK, passkeyLoginResponse{
				LoggedIn: true,
				Phone:    &phone,
				UserID:   &userID,
			})
			return
		}
	}

	// No account or tokens completely expired — need OTP reauth
	s.sessions.MarkPasskeyVerified(phone)
	s.logger.Info("Passkey verified but Garmin tokens expired, need reauth", "phone", phone)

	// Try token refresh one more time with a fresh context
	if acct != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := acct.Auth.RefreshHermesToken(ctx); err == nil {
			// Refresh worked after all
			session, err := s.sessions.CreateSession(phone, acct.Auth, s.logger)
			if err == nil {
				if s.pushStore != nil {
					session.Account.pushMu.Lock()
					if session.Account.PushSubscriptions == nil {
						session.Account.PushSubscriptions = s.pushStore.Load(phone)
					}
					session.Account.pushMu.Unlock()
				}
				s.wirePushCallback(session.Account)
				SetSessionCookie(w, session.ID, s.sessions.sessionDays)
				s.PersistSessions()

				s.logger.Info("Passkey login successful (late refresh)", "phone", phone)
				userID := gm.PhoneToHermesUserID(phone)
				writeJSON(w, http.StatusOK, passkeyLoginResponse{
					LoggedIn: true,
					Phone:    &phone,
					UserID:   &userID,
				})
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, passkeyLoginResponse{
		NeedReauth: true,
		Phone:      &phone,
	})
}
