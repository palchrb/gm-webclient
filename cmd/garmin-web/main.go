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
	fcmDataDir := flag.String("fcm-data-dir", "", "Directory for FCM credential persistence (enables FCM push; empty to disable)")
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

	if *fcmDataDir != "" {
		log.Printf("FCM push enabled, data dir: %s", *fcmDataDir)
	}

	srv := web.NewServer(logger, *fcmDataDir)
	log.Printf("Garmin Messenger Web listening on %s", *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
}
