package connector

import (
	"context"
	"log/slog"

	"github.com/rs/zerolog"
)

// zerologSlogHandler is a slog.Handler that routes records to a zerolog.Logger.
// This lets the hermes library (which uses slog) emit logs through the same
// zerolog pipeline as the rest of the bridge, including debug-level raw payloads.
type zerologSlogHandler struct {
	l zerolog.Logger
}

func newZerologSlogHandler(l zerolog.Logger) slog.Handler {
	return &zerologSlogHandler{l: l}
}

func (h *zerologSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	zl := h.l.GetLevel()
	switch level {
	case slog.LevelDebug:
		return zl <= zerolog.DebugLevel
	case slog.LevelWarn:
		return zl <= zerolog.WarnLevel
	case slog.LevelError:
		return zl <= zerolog.ErrorLevel
	default: // Info
		return zl <= zerolog.InfoLevel
	}
}

func (h *zerologSlogHandler) Handle(_ context.Context, r slog.Record) error {
	var ev *zerolog.Event
	switch r.Level {
	case slog.LevelDebug:
		ev = h.l.Debug()
	case slog.LevelWarn:
		ev = h.l.Warn()
	case slog.LevelError:
		ev = h.l.Error()
	default:
		ev = h.l.Info()
	}
	r.Attrs(func(a slog.Attr) bool {
		ev = ev.Any(a.Key, a.Value.Any())
		return true
	})
	ev.Msg(r.Message)
	return nil
}

func (h *zerologSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	l := h.l
	for _, a := range attrs {
		l = l.With().Any(a.Key, a.Value.Any()).Logger()
	}
	return &zerologSlogHandler{l: l}
}

func (h *zerologSlogHandler) WithGroup(name string) slog.Handler {
	return h // groups not needed for our use case
}
