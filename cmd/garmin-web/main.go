package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yourusername/matrix-garmin-messenger/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Directory for persistent data (FCM credentials, VAPID keys, push subscriptions, sessions)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	phoneWhitelist := flag.String("phone-whitelist", "", "Comma-separated list of phone numbers allowed to log in (e.g. \"+4712345678,+4787654321\"). Empty allows all.")
	sessionDays := flag.Int("session-days", 7, "Number of days a login session/cookie is valid")
	origin := flag.String("origin", "", "Origin URL for passkey/WebAuthn support (e.g. \"https://garmin.tailnet.ts.net\")")
	flag.Parse()

	// Log level: flag takes precedence, then env var, then default "info"
	logLevelStr := *logLevel
	if logLevelStr == "info" {
		if envLevel := os.Getenv("LOG_LEVEL"); envLevel != "" {
			logLevelStr = envLevel
		}
	}

	var level slog.Level
	switch strings.ToLower(logLevelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Load or generate VAPID keys for Web Push notifications
	var vapidKeys *web.VAPIDKeys
	if *dataDir != "" {
		var err error
		vapidKeys, err = web.LoadOrGenerateVAPIDKeys(*dataDir)
		if err != nil {
			log.Fatalf("Failed to load/generate VAPID keys: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Web Push enabled (VAPID public key: %s...)\n", vapidKeys.PublicKey[:20])
		fmt.Fprintf(os.Stderr, "FCM push enabled, data dir: %s\n", *dataDir)
	}

	var opts []web.ServerOption

	// Phone whitelist from flag or env var
	whitelist := *phoneWhitelist
	if whitelist == "" {
		whitelist = os.Getenv("PHONE_WHITELIST")
	}
	if whitelist != "" {
		phones := parsePhoneList(whitelist)
		if len(phones) > 0 {
			opts = append(opts, web.WithPhoneWhitelist(phones))
			fmt.Fprintf(os.Stderr, "Phone whitelist enabled: %v\n", phones)
		}
	}

	// Session TTL from flag or env var
	days := *sessionDays
	if envDays := os.Getenv("SESSION_DAYS"); envDays != "" {
		if d, err := strconv.Atoi(envDays); err == nil && d > 0 {
			days = d
		}
	}
	if days > 0 {
		opts = append(opts, web.WithSessionDays(days))
		fmt.Fprintf(os.Stderr, "Session TTL: %d days\n", days)
	}

	// Passkey (WebAuthn) support from flag or env var
	originStr := *origin
	if originStr == "" {
		originStr = os.Getenv("ORIGIN")
	}
	if originStr != "" {
		opts = append(opts, web.WithOrigin(originStr))
	}

	// ntfy.sh push notification forwarding
	if ntfyURL := os.Getenv("NTFY_URL"); ntfyURL != "" && *dataDir != "" {
		hmacKey, err := web.LoadOrGenerateNtfyHMACKey(*dataDir)
		if err != nil {
			log.Fatalf("Failed to load/generate ntfy HMAC key: %v", err)
		}
		ntfyFull := os.Getenv("NTFY_FULL_MESSAGE") == "true" || os.Getenv("NTFY_FULL_MESSAGE") == "1"
		opts = append(opts, web.WithNtfyConfig(&web.NtfyConfig{
			BaseURL:     ntfyURL,
			HMACKey:     hmacKey,
			ClickURL:    originStr,
			FullMessage: ntfyFull,
		}))
		fmt.Fprintf(os.Stderr, "ntfy push enabled (server: %s)\n", ntfyURL)
	}

	// Push always: send web push even when browser tabs are open (default true)
	pushAlways := true
	if envPush := os.Getenv("PUSH_ALWAYS"); envPush != "" {
		pushAlways = envPush == "true" || envPush == "1"
	}
	opts = append(opts, web.WithPushAlways(pushAlways))
	fmt.Fprintf(os.Stderr, "Web Push always-on: %v\n", pushAlways)

	// Encrypted session persistence.
	// If SESSION_KEY is set, use it. Otherwise auto-generate and persist one
	// in the data dir so sessions survive restarts without manual config.
	if *dataDir != "" {
		sessionKey := os.Getenv("SESSION_KEY")
		if sessionKey == "" {
			sessionKey = loadOrGenerateSessionKey(*dataDir)
		}
		if sessionKey != "" {
			opts = append(opts, web.WithSessionKey(sessionKey))
			fmt.Fprintln(os.Stderr, "Encrypted session persistence enabled")
		}
	}

	srv := web.NewServer(logger, *dataDir, vapidKeys, opts...)
	fmt.Fprintf(os.Stderr, "Garmin Messenger Web listening on %s\n", *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
}

// loadOrGenerateSessionKey reads or creates a random session encryption key
// in the data directory. This means sessions survive restarts automatically
// without requiring the user to set SESSION_KEY manually.
func loadOrGenerateSessionKey(dataDir string) string {
	keyPath := filepath.Join(dataDir, "session_key")
	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) > 0 {
		return strings.TrimSpace(string(data))
	}

	// Generate a random 32-byte key
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not generate session key: %v\n", err)
		return ""
	}
	key := hex.EncodeToString(keyBytes)

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create data dir for session key: %v\n", err)
		return ""
	}
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save session key: %v\n", err)
		return ""
	}

	fmt.Fprintf(os.Stderr, "Generated new session encryption key in %s\n", keyPath)
	return key
}

func parsePhoneList(s string) []string {
	var phones []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			phones = append(phones, p)
		}
	}
	return phones
}
