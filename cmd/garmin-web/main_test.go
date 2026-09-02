package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+4712345678":  "+47…5678",
		"+12125550123": "+12…0123",
		"+1234567":     "+1234567", // too short to mask meaningfully
		"":             "",
	}
	for in, want := range cases {
		if got := maskPhone(in); got != want {
			t.Errorf("maskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskPhoneAttr_AppliesToWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: maskPhoneAttr}))
	logger.With("phone", "+4712345678").Info("hello", "phone", "+4798765432", "other", "+4711111111")
	out := buf.String()
	if strings.Contains(out, "+4712345678") || strings.Contains(out, "+4798765432") {
		t.Fatalf("phone leaked in log line: %s", out)
	}
	if !strings.Contains(out, "+47…5678") || !strings.Contains(out, "+47…5432") {
		t.Fatalf("expected masked phones: %s", out)
	}
	if !strings.Contains(out, "+4711111111") {
		t.Fatalf("non-phone attrs must be untouched: %s", out)
	}
}
