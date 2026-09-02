package garminmessenger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHermesAuth_Defaults(t *testing.T) {
	a := NewHermesAuth()
	assert.Equal(t, DefaultHermesBase, a.HermesBase)
	assert.Empty(t, a.AccessToken)
}

func TestNewHermesAuth_Options(t *testing.T) {
	a := NewHermesAuth(WithHermesBase("https://example.com/"))
	assert.Equal(t, "https://example.com", a.HermesBase) // trailing slash stripped
}

func TestWithPnsHandle_Default(t *testing.T) {
	a := NewHermesAuth()
	assert.Equal(t, DefaultPnsHandle, a.PnsHandle())
}

func TestWithPnsHandle_Custom(t *testing.T) {
	a := NewHermesAuth(WithPnsHandle("custom-token"))
	assert.Equal(t, "custom-token", a.PnsHandle())
}

func TestTokenExpired_NoToken(t *testing.T) {
	a := NewHermesAuth()
	assert.True(t, a.TokenExpired())
}

func TestTokenExpired_ExpiredToken(t *testing.T) {
	a := NewHermesAuth()
	a.AccessToken = "test"
	a.ExpiresAt = float64(time.Now().Unix()) - 100
	assert.True(t, a.TokenExpired())
}

func TestTokenExpired_WithinBuffer(t *testing.T) {
	a := NewHermesAuth()
	a.AccessToken = "test"
	// Expires in 30s — within the 60s buffer
	a.ExpiresAt = float64(time.Now().Unix()) + 30
	assert.True(t, a.TokenExpired())
}

func TestTokenExpired_NotExpired(t *testing.T) {
	a := NewHermesAuth()
	a.AccessToken = "test"
	a.ExpiresAt = float64(time.Now().Unix()) + 3600
	assert.False(t, a.TokenExpired())
}

func TestRequestOTP_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/Registration/App", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, RegistrationAPIKey, r.Header.Get("RegistrationApiKey"))
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body NewAppRegistrationBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "+15551234567", body.SmsNumber)
		assert.Equal(t, "android", body.Platform)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(NewAppRegistrationResponse{
			RequestID:         "req-abc-123",
			ValidUntil:        ptr("2025-06-01T12:00:00Z"),
			AttemptsRemaining: ptr(3),
		})
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	otpReq, err := a.RequestOTP(context.Background(), "+15551234567", "garmin-messenger")
	require.NoError(t, err)

	assert.Equal(t, "req-abc-123", otpReq.RequestID)
	assert.Equal(t, "+15551234567", otpReq.PhoneNumber)
	assert.Equal(t, "garmin-messenger", otpReq.DeviceName)
	assert.Equal(t, ptr("2025-06-01T12:00:00Z"), otpReq.ValidUntil)
	assert.Equal(t, ptr(3), otpReq.AttemptsRemaining)
}

func TestRequestOTP_409Retry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(NewAppRegistrationResponse{
			RequestID: "req-after-retry",
		})
	}))
	defer server.Close()

	// Use a context with short timeout to speed up test
	// The 409 handler sleeps 30s, so we use a context that doesn't expire
	// but we set a very short sleep by canceling quickly
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	a := NewHermesAuth(WithHermesBase(server.URL))
	otpReq, err := a.RequestOTP(ctx, "+15551234567", "test")
	require.NoError(t, err)
	assert.Equal(t, "req-after-retry", otpReq.RequestID)
	assert.Equal(t, 2, attempts)
}

func TestConfirmOTP_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/Registration/App/Confirm", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, RegistrationAPIKey, r.Header.Get("RegistrationApiKey"))

		var body ConfirmAppRegistrationBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "req-123", body.RequestID)
		assert.Equal(t, "+15551234567", body.SmsNumber)
		assert.Equal(t, "123456", body.VerificationCode)
		assert.Equal(t, "android", body.Platform)
		assert.Equal(t, DefaultPnsHandle, body.PnsHandle)
		assert.Equal(t, PnsEnvironment, body.PnsEnvironment)
		assert.Equal(t, "garmin-messenger", body.AppDescription)
		assert.True(t, body.OptInForSms)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AppRegistrationResponse{
			InstanceID: testInstanceID,
			AccessAndRefreshToken: AccessAndRefreshToken{
				AccessToken:  "new-access-token",
				RefreshToken: "new-refresh-token",
				ExpiresIn:    3600,
			},
		})
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	updated := make(chan struct{}, 1)
	a.OnCredentialsUpdated = func() { updated <- struct{}{} }
	otpReq := &OtpRequest{
		RequestID:   "req-123",
		PhoneNumber: "+15551234567",
		DeviceName:  "garmin-messenger",
	}

	err := a.ConfirmOTP(context.Background(), otpReq, "123456")
	require.NoError(t, err)

	assert.Equal(t, "new-access-token", a.AccessToken)
	assert.Equal(t, "new-refresh-token", a.RefreshToken)
	assert.Equal(t, testInstanceID, a.InstanceID)
	assert.False(t, a.TokenExpired())

	// The owner is notified so it can persist the new tokens itself.
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("OnCredentialsUpdated was not called")
	}
}

func TestConfirmOTP_CustomPnsHandle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/Registration/App/Confirm", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, RegistrationAPIKey, r.Header.Get("RegistrationApiKey"))

		var body ConfirmAppRegistrationBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "req-123", body.RequestID)
		assert.Equal(t, "+15551234567", body.SmsNumber)
		assert.Equal(t, "123456", body.VerificationCode)
		assert.Equal(t, "android", body.Platform)
		assert.Equal(t, "custom-fcm-token", body.PnsHandle)
		assert.Equal(t, PnsEnvironment, body.PnsEnvironment)
		assert.Equal(t, "garmin-messenger", body.AppDescription)
		assert.True(t, body.OptInForSms)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AppRegistrationResponse{
			InstanceID: testInstanceID,
			AccessAndRefreshToken: AccessAndRefreshToken{
				AccessToken:  "new-access-token",
				RefreshToken: "new-refresh-token",
				ExpiresIn:    3600,
			},
		})
	}))
	defer server.Close()

	a := NewHermesAuth(
		WithHermesBase(server.URL),
		WithPnsHandle("custom-fcm-token"),
	)
	otpReq := &OtpRequest{
		RequestID:   "req-123",
		PhoneNumber: "+15551234567",
		DeviceName:  "garmin-messenger",
	}

	err := a.ConfirmOTP(context.Background(), otpReq, "123456")
	require.NoError(t, err)
}

func TestUpdatePnsHandle_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/Registration/App", r.URL.Path)
		assert.Equal(t, "PATCH", r.Method)
		assert.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body UpdateAppPnsHandleBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "new-fcm-token", body.PnsHandle)
		assert.Equal(t, PnsEnvironment, body.PnsEnvironment)
		assert.Equal(t, "garmin-messenger", body.AppDescription)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.AccessToken = "access-token"
	a.ExpiresAt = float64(time.Now().Unix()) + 3600

	err := a.UpdatePnsHandle(context.Background(), "new-fcm-token")
	require.NoError(t, err)
}

func TestUpdatePnsHandle_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.AccessToken = "access-token"
	a.ExpiresAt = float64(time.Now().Unix()) + 3600

	err := a.UpdatePnsHandle(context.Background(), "new-fcm-token")
	require.Error(t, err)
}

func TestHeaders_BearerFormat(t *testing.T) {
	a := NewHermesAuth()
	a.AccessToken = "test-token"
	a.ExpiresAt = float64(time.Now().Unix()) + 3600

	h, err := a.Headers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", h.Get("Authorization"))
	assert.Equal(t, "2.0", h.Get("Api-Version"))
}

func TestHeaders_TriggersRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AppRegistrationResponse{
			InstanceID: testInstanceID,
			AccessAndRefreshToken: AccessAndRefreshToken{
				AccessToken:  "refreshed-token",
				RefreshToken: "new-refresh",
				ExpiresIn:    3600,
			},
		})
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.AccessToken = "expired-token"
	a.RefreshToken = "valid-refresh"
	a.InstanceID = testInstanceID
	a.ExpiresAt = float64(time.Now().Unix()) - 100

	h, err := a.Headers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer refreshed-token", h.Get("Authorization"))
}

func TestRefreshHermesToken_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/Registration/App/Refresh", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))

		var body RefreshAuthBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "old-refresh", body.RefreshToken)
		assert.Equal(t, testInstanceID, body.InstanceID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AppRegistrationResponse{
			InstanceID: testInstanceID,
			AccessAndRefreshToken: AccessAndRefreshToken{
				AccessToken:  "new-token",
				RefreshToken: "new-refresh",
				ExpiresIn:    3600,
			},
		})
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.RefreshToken = "old-refresh"
	a.InstanceID = testInstanceID
	updated := make(chan struct{}, 1)
	a.OnCredentialsUpdated = func() { updated <- struct{}{} }

	err := a.RefreshHermesToken(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "new-token", a.AccessToken)
	assert.Equal(t, "new-refresh", a.RefreshToken)
	assert.False(t, a.TokenExpired())

	// A refresh must notify the owner, otherwise a restart would restore
	// the old (now invalid) refresh token.
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("OnCredentialsUpdated was not called after refresh")
	}
}

func TestRefreshHermesToken_NoCredentials(t *testing.T) {
	a := NewHermesAuth()
	err := a.RefreshHermesToken(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no refresh token")
}

func TestAccessTokenFactory_RefreshesExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AppRegistrationResponse{
			InstanceID: testInstanceID,
			AccessAndRefreshToken: AccessAndRefreshToken{
				AccessToken:  "factory-refreshed",
				RefreshToken: "new-refresh",
				ExpiresIn:    3600,
			},
		})
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.AccessToken = "expired"
	a.RefreshToken = "valid"
	a.InstanceID = testInstanceID
	a.ExpiresAt = float64(time.Now().Unix()) - 100

	token, err := a.AccessTokenFactory(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "factory-refreshed", token)
}

func TestDeleteAppRegistration_PathEscape(t *testing.T) {
	var requestedRawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedRawPath = r.URL.RawPath
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	a := NewHermesAuth(WithHermesBase(server.URL))
	a.AccessToken = "access-token"
	a.ExpiresAt = float64(time.Now().Unix()) + 3600

	// A malicious instanceID with path traversal should be escaped
	err := a.DeleteAppRegistration(context.Background(), "../User")
	require.NoError(t, err)

	// RawPath preserves %-encoding; the slash in "../User" must be escaped
	assert.Contains(t, requestedRawPath, "%2F")
	assert.NotContains(t, requestedRawPath, "/Registration/User")
}

func TestAPIError_Error(t *testing.T) {
	err := &APIError{
		StatusCode: 401,
		Status:     "401 Unauthorized",
		Body:       "invalid token",
		URL:        "https://example.com/api",
		Method:     "GET",
	}
	assert.Contains(t, err.Error(), "GET")
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "invalid token")
}
