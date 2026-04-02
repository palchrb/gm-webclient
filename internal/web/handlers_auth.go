package web

import (
	"encoding/json"
	"net/http"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
	"golang.org/x/crypto/bcrypt"
)

type requestOTPRequest struct {
	Phone      string `json:"phone"`
	DeviceName string `json:"deviceName"`
}

type requestOTPResponse struct {
	Phone             string  `json:"phone"`
	ValidUntil        *string `json:"validUntil,omitempty"`
	AttemptsRemaining *int    `json:"attemptsRemaining,omitempty"`
	NeedPIN           bool    `json:"needPin,omitempty"`
}

type confirmOTPRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
	PIN   string `json:"pin"`
}

type authStatusResponse struct {
	LoggedIn bool    `json:"loggedIn"`
	Phone    *string `json:"phone,omitempty"`
	UserID   *string `json:"userId,omitempty"`
}

func (s *Server) handleRequestOTP(w http.ResponseWriter, r *http.Request) {
	var req requestOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Phone == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone is required"})
		return
	}

	// Check phone whitelist
	if s.phoneWhitelist != nil && !s.phoneWhitelist[req.Phone] {
		s.logger.Warn("Login attempt from non-whitelisted phone", "phone", req.Phone)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this phone number is not allowed"})
		return
	}

	// If account already exists with a PIN, user can log in with PIN instead of OTP.
	if acct := s.sessions.GetAccount(req.Phone); acct != nil && len(acct.PINHash) > 0 {
		s.logger.Info("Account active with PIN, requesting PIN login", "phone", req.Phone)
		writeJSON(w, http.StatusOK, requestOTPResponse{
			Phone:   req.Phone,
			NeedPIN: true,
		})
		return
	}

	// Account exists but no PIN, or no account at all — proceed with normal OTP.
	// The confirm-otp endpoint accepts an optional PIN to set it.

	if req.DeviceName == "" {
		req.DeviceName = "Garmin Messenger"
	}

	// Auth tokens are kept in server memory only — not persisted to disk.
	// A Docker restart requires re-login, but no user data is stored on the server.
	auth := gm.NewHermesAuth(gm.WithLogger(s.logger))

	otpReq, err := auth.RequestOTP(r.Context(), req.Phone, req.DeviceName)
	if err != nil {
		s.logger.Error("OTP request failed", "phone", req.Phone, "error", err)
		if apiErr, ok := err.(*gm.APIError); ok {
			writeJSON(w, apiErr.StatusCode, map[string]string{"error": "OTP request failed: " + apiErr.Body})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTP request failed"})
		return
	}

	// Store pending OTP for confirmation
	s.sessions.StorePendingOTP(req.Phone, otpReq, auth)

	writeJSON(w, http.StatusOK, requestOTPResponse{
		Phone:             req.Phone,
		ValidUntil:        otpReq.ValidUntil,
		AttemptsRemaining: otpReq.AttemptsRemaining,
	})
}

func (s *Server) handleConfirmOTP(w http.ResponseWriter, r *http.Request) {
	var req confirmOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Phone == "" || req.Code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone and code are required"})
		return
	}

	pending := s.sessions.GetPendingOTP(req.Phone)
	if pending == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no pending OTP for this phone number, request OTP first"})
		return
	}

	if err := pending.Auth.ConfirmOTP(r.Context(), pending.OtpReq, req.Code); err != nil {
		s.logger.Error("OTP confirmation failed", "phone", req.Phone, "error", err)
		// Put it back so user can retry
		s.sessions.StorePendingOTP(req.Phone, pending.OtpReq, pending.Auth)
		if apiErr, ok := err.(*gm.APIError); ok {
			writeJSON(w, apiErr.StatusCode, map[string]string{"error": "OTP confirmation failed: " + apiErr.Body})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTP confirmation failed"})
		return
	}

	// Create session
	session, err := s.sessions.CreateSession(req.Phone, pending.Auth, s.logger)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}

	// If a PIN was provided with the OTP confirmation, set it on the account
	if req.PIN != "" {
		if hash, err := bcrypt.GenerateFromPassword([]byte(req.PIN), bcrypt.DefaultCost); err == nil {
			session.Account.PINHash = hash
		}
	}

	// Load persisted push subscriptions and wire push callback
	if s.pushStore != nil {
		session.Account.pushMu.Lock()
		if session.Account.PushSubscriptions == nil {
			session.Account.PushSubscriptions = s.pushStore.Load(req.Phone)
		}
		session.Account.pushMu.Unlock()
	}
	session.Account.SSE.OnNoSubscribers(func(event SSEEvent) {
		s.sendWebPush(session.Account, event)
	})

	SetSessionCookie(w, session.ID, s.sessions.sessionDays)
	s.PersistSessions()

	userID := gm.PhoneToHermesUserID(req.Phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &req.Phone,
		UserID:   &userID,
	})
}

// handlePINLogin creates a new browser session using a PIN for an already-active account.
func (s *Server) handlePINLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		PIN   string `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Phone == "" || req.PIN == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone and pin are required"})
		return
	}

	// Check phone whitelist
	if s.phoneWhitelist != nil && !s.phoneWhitelist[req.Phone] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this phone number is not allowed"})
		return
	}

	acct := s.sessions.GetAccount(req.Phone)
	if acct == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no active account for this phone"})
		return
	}

	if len(acct.PINHash) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no PIN set for this account"})
		return
	}

	if err := bcrypt.CompareHashAndPassword(acct.PINHash, []byte(req.PIN)); err != nil {
		s.logger.Warn("PIN login failed", "phone", req.Phone)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect PIN"})
		return
	}

	// Create a new session reusing the existing account
	session, err := s.sessions.CreateSession(req.Phone, acct.Auth, s.logger)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}

	SetSessionCookie(w, session.ID, s.sessions.sessionDays)
	s.PersistSessions()

	s.logger.Info("PIN login successful", "phone", req.Phone)
	userID := gm.PhoneToHermesUserID(req.Phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &req.Phone,
		UserID:   &userID,
	})
}

// handleSetPIN sets a PIN on an already-active account that doesn't have one yet.
// Requires an existing valid session cookie.
func (s *Server) handleSetPIN(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		PIN string `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if len(req.PIN) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "PIN must be at least 4 characters"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.PIN), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to hash PIN"})
		return
	}

	session.Account.PINHash = hash
	s.PersistSessions()
	s.logger.Info("PIN set for account", "phone", session.Account.Phone)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	session := s.sessions.GetFromRequest(r)
	if session == nil {
		writeJSON(w, http.StatusOK, authStatusResponse{LoggedIn: false})
		return
	}
	session.Touch()
	phone := session.Phone()
	userID := gm.PhoneToHermesUserID(phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &phone,
		UserID:   &userID,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session != nil {
		s.sessions.RemoveSession(session.ID)
		s.PersistSessions()
	}
	ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
