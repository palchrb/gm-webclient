package web

import (
	"encoding/json"
	"net/http"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

type requestOTPRequest struct {
	Phone      string `json:"phone"`
	DeviceName string `json:"deviceName"`
}

type requestOTPResponse struct {
	Phone             string  `json:"phone"`
	ValidUntil        *string `json:"validUntil,omitempty"`
	AttemptsRemaining *int    `json:"attemptsRemaining,omitempty"`
}

type confirmOTPRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
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

	if req.DeviceName == "" {
		req.DeviceName = "Garmin Messenger Web"
	}

	// Create a new HermesAuth without session dir (no disk persistence)
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

	SetSessionCookie(w, session.ID)

	userID := gm.PhoneToHermesUserID(req.Phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &req.Phone,
		UserID:   &userID,
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	session := s.sessions.GetFromRequest(r)
	if session == nil {
		writeJSON(w, http.StatusOK, authStatusResponse{LoggedIn: false})
		return
	}
	session.Touch()
	userID := gm.PhoneToHermesUserID(session.Phone)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn: true,
		Phone:    &session.Phone,
		UserID:   &userID,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())
	if session != nil {
		s.sessions.RemoveSession(session.ID)
	}
	ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
