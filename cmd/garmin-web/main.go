package main

import (
	"flag"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/yourusername/matrix-garmin-messenger/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Directory for persistent data (FCM credentials, VAPID keys, push subscriptions)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	phoneWhitelist := flag.String("phone-whitelist", "", "Comma-separated list of phone numbers allowed to log in (e.g. \"+4712345678,+4787654321\"). Empty allows all.")
	flag.Parse()

	var level slog.Level
	switch *logLevel {
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
		log.Printf("Web Push enabled (VAPID public key: %s...)", vapidKeys.PublicKey[:20])
		log.Printf("FCM push enabled, data dir: %s", *dataDir)
	}

	// Parse phone whitelist from flag or PHONE_WHITELIST env var
	var opts []web.ServerOption
	whitelist := *phoneWhitelist
	if whitelist == "" {
		whitelist = os.Getenv("PHONE_WHITELIST")
	}
	if whitelist != "" {
		phones := parsePhoneList(whitelist)
		if len(phones) > 0 {
			opts = append(opts, web.WithPhoneWhitelist(phones))
			log.Printf("Phone whitelist enabled: %v", phones)
		}
	}

	srv := web.NewServer(logger, *dataDir, vapidKeys, opts...)
	log.Printf("Garmin Messenger Web listening on %s", *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
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
