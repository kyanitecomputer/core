// SPDX-License-Identifier: BSD-3-Clause

package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"src.kyanite.computer/core/telemetry/keys"
)

// captureHandler records the attributes of the last handled record.
type captureHandler struct{ attrs map[string]slog.Value }

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.attrs = map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		c.attrs[a.Key] = a.Value
		return true
	})
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

func spanCtx(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	tid, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), sc
}

func TestTraceHandlerStampsIDs(t *testing.T) {
	cap := &captureHandler{}
	h := traceHandler{inner: cap}
	ctx, sc := spanCtx(t)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if v, ok := cap.attrs[keys.TraceID]; !ok || v.String() != sc.TraceID().String() {
		t.Fatalf("trace_id = %v (present=%v), want %s", v, ok, sc.TraceID())
	}
	if v, ok := cap.attrs[keys.SpanID]; !ok || v.String() != sc.SpanID().String() {
		t.Fatalf("span_id = %v (present=%v), want %s", v, ok, sc.SpanID())
	}
}

func TestTraceHandlerNoContextIsPassthrough(t *testing.T) {
	cap := &captureHandler{}
	h := traceHandler{inner: cap}

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := cap.attrs[keys.TraceID]; ok {
		t.Fatal("trace_id stamped despite no span context")
	}
}
