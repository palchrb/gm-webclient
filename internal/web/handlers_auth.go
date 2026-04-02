package web

import (
	"encoding/json"
	"net/http"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

type requestOTPRequest struct {
	Phone      string `json:"phone"`
	DeviceName string `json:"deviceName"`
	ForceOTP   bool   `json:"forceOTP"` // skip passkey check (lost passkey flow)
}

type requestOTPResponse struct {
	Phone             string  `json:"phone"`
	ValidUntil        *string `json:"validUntil,omitempty"`
	AttemptsRemaining *int    `json:"attemptsRemaining,omitempty"`
	NeedPasskey       bool    `json:"needPasskey,omitempty"`
}

type confirmOTPRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

type confirmOTPResponse struct {
	LoggedIn         bool    `json:"loggedIn"`
	Phone            *string `json:"phone,omitempty"`
	UserID           *string `json:"userId,omitempty"`
	NeedPasskeySetup bool    `json:"needPasskeySetup,omitempty"` // must register passkey before using app
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

	// If passkey exists and not forcing OTP, redirect to passkey login
	if !req.ForceOTP && s.webAuthn != nil && s.passkeyStore != nil && s.passkeyStore.HasCredentials(req.Phone) {
		s.logger.Info("Passkey exists, requesting passkey login", "phone", req.Phone)
		writeJSON(w, http.StatusOK, requestOTPResponse{
			Phone:       req.Phone,
			NeedPasskey: true,
		})
		return
	}

	if req.DeviceName == "" {
		req.DeviceName = "Garmin Messenger"
	}

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

	s.sessions.StorePendingOTP(req.Phone, otpReq, auth, false)

	writeJSON(w, http.StatusOK, requestOTPResponse{
		Phone:             req.Phone,
		ValidUntil:        otpReq.ValidUntil,
		AttemptsRemaining: otpReq.AttemptsRemaining,
	})
}

// handleRequestReauthOTP sends an OTP specifically for reconnecting Garmin
// after passkey login when tokens are expired. Uses the existing device name.
func (s *Server) handleRequestReauthOTP(w http.ResponseWriter, r *http.Request) {
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

	auth := gm.NewHermesAuth(gm.WithLogger(s.logger))

	otpReq, err := auth.RequestOTP(r.Context(), req.Phone, "Garmin Messenger")
	if err != nil {
		s.logger.Error("Reauth OTP request failed", "phone", req.Phone, "error", err)
		if apiErr, ok := err.(*gm.APIError); ok {
			writeJSON(w, apiErr.StatusCode, map[string]string{"error": "OTP request failed: " + apiErr.Body})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTP request failed"})
		return
	}

	s.sessions.StorePendingOTP(req.Phone, otpReq, auth, true)

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
		s.sessions.StorePendingOTP(req.Phone, pending.OtpReq, pending.Auth, pending.IsReauth)
		if apiErr, ok := err.(*gm.APIError); ok {
			writeJSON(w, apiErr.StatusCode, map[string]string{"error": "OTP confirmation failed: " + apiErr.Body})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "OTP confirmation failed"})
		return
	}

	// Create session (hard prunes any existing session for this phone)
	session, err := s.sessions.CreateSession(req.Phone, pending.Auth, s.logger)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}

	// Load persisted push subscriptions and wire push callback
	if s.pushStore != nil {
		session.Account.pushMu.Lock()
		if session.Account.PushSubscriptions == nil {
			session.Account.PushSubscriptions = s.pushStore.Load(req.Phone)
		}
		session.Account.pushMu.Unlock()
	}
	s.wirePushCallback(session.Account)

	SetSessionCookie(w, session.ID, s.sessions.sessionDays)
	s.PersistSessions()

	userID := gm.PhoneToHermesUserID(req.Phone)

	// Determine if passkey setup is needed:
	// - Reauth (passkey already verified, tokens refreshed): no setup needed
	// - Passkey already verified via PopPasskeyVerified: no setup needed (lost passkey OTP, will re-register)
	// - WebAuthn available and no passkey yet: need setup
	// - ForceOTP (lost passkey): clear old passkeys, need setup
	needPasskeySetup := false
	if s.webAuthn == nil || s.passkeyStore == nil {
		s.logger.Debug("Passkey setup skipped (ORIGIN not configured)", "phone", req.Phone,
			"webAuthn", s.webAuthn != nil, "passkeyStore", s.passkeyStore != nil)
	}
	if s.webAuthn != nil && s.passkeyStore != nil {
		passkeyVerified := s.sessions.PopPasskeyVerified(req.Phone)
		if pending.IsReauth || passkeyVerified {
			// Reauth or lost-passkey OTP — passkey will be re-registered by frontend
			needPasskeySetup = !s.passkeyStore.HasCredentials(req.Phone) || passkeyVerified
		} else {
			// Fresh first login — always need passkey
			needPasskeySetup = true
			// Clear any old passkeys (fresh OTP = fresh start)
			s.passkeyStore.Save(req.Phone, nil)
		}
	}

	writeJSON(w, http.StatusOK, confirmOTPResponse{
		LoggedIn:         true,
		Phone:            &req.Phone,
		UserID:           &userID,
		NeedPasskeySetup: needPasskeySetup,
	})
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

// handleLogoutAll removes ALL sessions for this phone, stops the Garmin account,
// deregisters with Garmin, and optionally clears passkeys.
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		ClearPasskeys bool `json:"clearPasskeys"`
	}
	json.NewDecoder(r.Body).Decode(&req) // optional body

	phone := session.Account.Phone
	s.sessions.RemoveAllForPhone(phone)

	if req.ClearPasskeys && s.passkeyStore != nil {
		s.passkeyStore.Save(phone, nil)
		s.logger.Info("Passkeys cleared", "phone", phone)
	}

	s.PersistSessions()
	ClearSessionCookie(w)
	s.logger.Info("Full logout: all sessions + Garmin deregistered", "phone", phone, "passkeysCleared", req.ClearPasskeys)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out everywhere"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
