package main

import (
	"flag"
	"log"
	"log/slog"
	"os"

	"github.com/yourusername/matrix-garmin-messenger/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Directory for persistent data (FCM credentials, VAPID keys, push subscriptions)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
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

	srv := web.NewServer(logger, *dataDir, vapidKeys)
	log.Printf("Garmin Messenger Web listening on %s", *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
}
