package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()
	return NewServer(testLogger(), "", nil, opts...)
}

func postJSON(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:1234"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestRequestOTP_RejectsInvalidPhone(t *testing.T) {
	s := newTestServer(t)
	for _, phone := range []string{"", "12345", "+0123456789", "../../x"} {
		rec := postJSON(t, s.handleRequestOTP, "/api/auth/request-otp", `{"phone":"`+phone+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("phone %q: status %d, want 400", phone, rec.Code)
		}
	}
}

func TestRequestOTP_EnforcesWhitelist(t *testing.T) {
	s := newTestServer(t, WithPhoneWhitelist([]string{"+4711111111"}))
	rec := postJSON(t, s.handleRequestOTP, "/api/auth/request-otp", `{"phone":"+4722222222"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
}

func TestRequestOTP_RateLimited(t *testing.T) {
	s := newTestServer(t)
	// Exhaust the per-phone limiter directly, then the handler must 429
	// before it ever talks to Garmin.
	for i := 0; i < 5; i++ {
		s.otpPhoneLimiter.allow("+4711111111")
	}
	rec := postJSON(t, s.handleRequestOTP, "/api/auth/request-otp", `{"phone":"+4711111111"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "too many") {
		t.Fatalf("unexpected error body: %v", resp)
	}
}

func TestRequestOTP_IPRateLimitSharedAcrossPhones(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 30; i++ {
		s.authIPLimiter.allow("192.0.2.10")
	}
	rec := postJSON(t, s.handleRequestOTP, "/api/auth/request-otp", `{"phone":"+4733333333"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 from IP limiter", rec.Code)
	}
}

func TestConfirmOTP_NoPendingIsBadRequest(t *testing.T) {
	s := newTestServer(t)
	rec := postJSON(t, s.handleConfirmOTP, "/api/auth/confirm-otp", `{"phone":"+4711111111","code":"123456"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	rec = postJSON(t, s.handleConfirmOTP, "/api/auth/confirm-otp", `{"phone":"bogus","code":"123456"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid phone: status %d, want 400", rec.Code)
	}
}

func TestSessionCookie_SecureAndLaxBehindProxy(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	SetSessionCookie(rec, req, "abc", 90)
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cs))
	}
	c := cs[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 90*86400 {
		t.Fatalf("cookie attrs wrong: %+v", c)
	}

	// Plain http (localhost) must not set Secure, or the browser drops it.
	rec = httptest.NewRecorder()
	SetSessionCookie(rec, httptest.NewRequest("GET", "/", nil), "abc", 1)
	if rec.Result().Cookies()[0].Secure {
		t.Fatal("Secure must be false on plain http")
	}

	rec = httptest.NewRecorder()
	ClearSessionCookie(rec, req)
	if rec.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("clear cookie should have MaxAge -1")
	}
}

func TestAuthStatus_RenewsCookie(t *testing.T) {
	s := newTestServer(t)
	sess, err := s.sessions.CreateSession("+4711111111", newFakeAuth(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	s.handleAuthStatus(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Value != sess.ID || cs[0].MaxAge <= 0 {
		t.Fatalf("expected renewed session cookie, got %+v", cs)
	}
}
