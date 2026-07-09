package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

// consoleHandler is a slog.Handler that writes compact, CR-LF terminated lines
// to a serial console (or any io.Writer). Serial terminals advance the cursor
// only on a carriage return, so a standard text handler (which emits only "\n")
// renders as a descending staircase; this handler emits "\r\n".
//
// Output format is "LVL message key=value …" — terse, allocation-light, and
// readable on a 80-column console. Groups are flattened (their attributes are
// emitted without a prefix), which is sufficient for device logging.
type consoleHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	attrs string // preformatted " key=value" pairs accumulated via WithAttrs
}

func newConsoleHandler(w io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{mu: new(sync.Mutex), w: w, level: level}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b []byte
	b = append(b, levelTag(r.Level)...)
	b = append(b, ' ')
	b = append(b, r.Message...)
	b = append(b, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		b = appendAttr(b, a)
		return true
	})
	b = append(b, '\r', '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(b)
	return err
}

func (h *consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	b := []byte(h.attrs)
	for _, a := range as {
		b = appendAttr(b, a)
	}
	return &consoleHandler{mu: h.mu, w: h.w, level: h.level, attrs: string(b)}
}

// WithGroup flattens groups: attributes are emitted without a group prefix.
func (h *consoleHandler) WithGroup(string) slog.Handler { return h }

func levelTag(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DBG"
	case l < slog.LevelWarn:
		return "INF"
	case l < slog.LevelError:
		return "WRN"
	default:
		return "ERR"
	}
}

func appendAttr(b []byte, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return b
	}
	b = append(b, ' ')
	b = append(b, a.Key...)
	b = append(b, '=')
	switch a.Value.Kind() {
	case slog.KindString:
		b = append(b, a.Value.String()...)
	case slog.KindInt64:
		b = strconv.AppendInt(b, a.Value.Int64(), 10)
	case slog.KindUint64:
		b = strconv.AppendUint(b, a.Value.Uint64(), 10)
	case slog.KindBool:
		b = strconv.AppendBool(b, a.Value.Bool())
	case slog.KindFloat64:
		b = strconv.AppendFloat(b, a.Value.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		b = append(b, a.Value.Duration().String()...)
	default:
		b = append(b, a.Value.String()...)
	}
	return b
}

// multiHandler fans a record out to several handlers (for example the console
// and the OTel ring buffer), so a single slog call reaches every sink.
type multiHandler struct {
	handlers []slog.Handler
}

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m multiHandler) WithAttrs(as []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(as)
	}
	return multiHandler{handlers: hs}
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return multiHandler{handlers: hs}
}
