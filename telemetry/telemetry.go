// Package telemetry wires Go's slog logging to two sinks shared by the Kyanite
// device runtimes (vein, cairn):
//
//   - a readable, CR-LF console handler (os.Stdout, which maps to the serial
//     UART on bare-metal TamaGo targets), and
//   - an in-process ring buffer of the last [RingSize] records, exposed via
//     [RecentLogs] for the management REST API.
//
// Usage:
//
//	telemetry.Init("vein")
//	slog.Info("link up", slog.String("port", "eth0"))   // console + ring
//	records := telemetry.RecentLogs(100)                // REST /api/telemetry/logs
//
// The pipeline is intentionally self-contained: it does not run the OpenTelemetry
// SDK on the device (its background batch processor is heavy for bare metal and
// unnecessary before an exporter exists). An OTLP/HTTP push exporter can be added
// later, fed from the ring buffer, once the lneto HTTP client path is wired to
// net.SocketFunc.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"src.kyanite.computer/core/telemetry/keys"
)

// RingSize is the maximum number of log records kept in the in-process ring
// buffer. At ~200 bytes/record this is ~200 KB of RAM.
const RingSize = 1024

// Level constants matching slog severity names.
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Record is a JSON-serialisable log entry stored in the ring buffer.
type Record struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// TimeString returns the record timestamp as an RFC 3339 string.
func (r Record) TimeString() string { return r.Time.Format(time.RFC3339) }

// LevelString returns the severity level string.
func (r Record) LevelString() string { return r.Level }

// MessageString returns the log message body.
func (r Record) MessageString() string { return r.Message }

// ring is the circular log buffer.
var ring struct {
	mu   sync.Mutex
	buf  [RingSize]Record
	head int // next write position
	used int // number of valid entries (≤ RingSize)
}

// Init installs the default slog handler. It must be called once at startup
// before any logging. Records are stamped with trace correlation (trace_id/
// span_id from the active span context, WARN+ escalated to span events) by a
// [traceHandler] middleware, then fanned out to the CR-LF console (os.Stdout)
// and the in-process ring buffer. serviceName is attached as service.name so it
// appears on every record.
func Init(serviceName string) {
	console := newConsoleHandler(os.Stdout, slog.LevelInfo)
	ring := &ringHandler{level: slog.LevelInfo}
	base := multiHandler{handlers: []slog.Handler{console, ring}}
	h := slog.Handler(traceHandler{inner: base})
	if serviceName != "" {
		h = h.WithAttrs([]slog.Attr{slog.String(keys.ServiceName, serviceName)})
	}
	slog.SetDefault(slog.New(h))
}

// Shutdown is a no-op retained for API compatibility and future exporters.
func Shutdown(_ context.Context) error { return nil }

// RecentLogs returns the last n log records from the ring buffer (oldest first).
// If n ≤ 0 or n > RingSize, all available records are returned.
func RecentLogs(n int) []Record {
	ring.mu.Lock()
	defer ring.mu.Unlock()

	if ring.used == 0 {
		return nil
	}
	count := ring.used
	if n > 0 && n < count {
		count = n
	}

	out := make([]Record, count)
	// The oldest entry in a full ring is ring.head (next write = oldest overwrite).
	start := (ring.head - ring.used + RingSize*2) % RingSize
	for i := 0; i < count; i++ {
		out[i] = ring.buf[(start+i)%RingSize]
	}
	return out
}

// ringHandler is a slog.Handler that appends records to the in-process ring.
type ringHandler struct {
	level slog.Level
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	rec := Record{Time: r.Time, Level: r.Level.String(), Message: r.Message}
	add := func(a slog.Attr) {
		if rec.Attrs == nil {
			rec.Attrs = make(map[string]any)
		}
		rec.Attrs[a.Key] = a.Value.Resolve().Any()
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(a)
		return true
	})

	ring.mu.Lock()
	ring.buf[ring.head] = rec
	ring.head = (ring.head + 1) % RingSize
	if ring.used < RingSize {
		ring.used++
	}
	ring.mu.Unlock()
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	na := make([]slog.Attr, 0, len(h.attrs)+len(as))
	na = append(na, h.attrs...)
	na = append(na, as...)
	return &ringHandler{level: h.level, attrs: na}
}

func (h *ringHandler) WithGroup(string) slog.Handler { return h }
