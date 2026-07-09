package telemetry

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestConsoleHandlerCRLFAndFormat(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newConsoleHandler(&buf, slog.LevelInfo))

	log.Info("link up", slog.Int("port", 3), slog.String("speed", "1000"))

	out := buf.String()
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("output not CR-LF terminated: %q", out)
	}
	if strings.Count(out, "\n") != 1 || strings.Count(out, "\r") != 1 {
		t.Fatalf("expected exactly one CR-LF: %q", out)
	}
	for _, want := range []string{"INF", "link up", "port=3", "speed=1000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q missing %q", out, want)
		}
	}
}

func TestConsoleHandlerLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newConsoleHandler(&buf, slog.LevelInfo))

	log.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug record should be filtered, got %q", buf.String())
	}

	log.Warn("careful")
	if !strings.Contains(buf.String(), "WRN careful") {
		t.Fatalf("warn record missing: %q", buf.String())
	}
}

func TestConsoleHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(newConsoleHandler(&buf, slog.LevelInfo))
	log := base.With(slog.String("svc", "net"))

	log.Info("started")

	out := buf.String()
	if !strings.Contains(out, "svc=net") || !strings.Contains(out, "started") {
		t.Fatalf("WithAttrs output wrong: %q", out)
	}
}
