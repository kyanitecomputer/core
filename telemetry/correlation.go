// SPDX-License-Identifier: BSD-3-Clause

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"src.kyanite.computer/core/telemetry/keys"
)

// traceHandler is a slog.Handler middleware that correlates log records with
// the active trace, per the embedded-OTel architecture:
//
//   - It stamps trace_id/span_id onto every record whose context carries a valid
//     span context. This works even for unsampled traces, and is the primary
//     debugging aid when spans were sampled away, so it is unconditional.
//   - It escalates significant records into the active span as a bounded signal:
//     WARN and above become a span event (only when the span is recording), and
//     ERROR additionally sets the span status. Lower levels are never bridged,
//     to keep span-event volume bounded on constrained targets.
//
// The handler holds no context.Context; ctx is received per Handle call and is
// never stored — trace correlation flows through the call, not through struct
// fields.
type traceHandler struct {
	inner slog.Handler
}

func (h traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String(keys.TraceID, sc.TraceID().String()),
			slog.String(keys.SpanID, sc.SpanID().String()),
		)
		if r.Level >= slog.LevelWarn {
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.AddEvent(r.Message, trace.WithAttributes(
					attribute.String(keys.LogSeverity, r.Level.String()),
				))
				if r.Level >= slog.LevelError {
					span.SetStatus(codes.Error, "")
				}
			}
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return traceHandler{inner: h.inner.WithAttrs(as)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{inner: h.inner.WithGroup(name)}
}
