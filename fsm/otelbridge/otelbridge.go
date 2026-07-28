// SPDX-License-Identifier: BSD-3-Clause

// Package otelbridge adapts an fsm machine to OpenTelemetry tracing. It is the
// only fsm package that imports OpenTelemetry, keeping fsm core fit for TamaGo
// and dependency-free.
//
// Following the embedded-OTel architecture, the bridge creates no spans: a
// transition is a fact, recorded as a span event on the caller's active span
// and only when that span is recording, so unsampled traces cost nothing beyond
// the guard.
package otelbridge

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"src.kyanite.computer/core/fsm"
	"src.kyanite.computer/core/telemetry/keys"
)

// Observer returns an [fsm.Observer] that records each completed transition as
// a "fsm.transition" span event on the active span in the transition's context,
// when that span is recording. name identifies the machine; l provides
// low-cardinality state and trigger names. Attach it with
// Builder.Observe(otelbridge.Observer[S, T, E](...)).
func Observer[S ~uint8, T ~uint8, E any](name string, l fsm.Labeler[S, T]) fsm.Observer[S, T, E] {
	return func(ctx context.Context, tr fsm.Transition[S, T], _ E) {
		span := trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return
		}
		span.AddEvent("fsm.transition", trace.WithAttributes(
			attribute.String(keys.FSMName, name),
			attribute.String(keys.FSMFrom, l.StateName(tr.Source)),
			attribute.String(keys.FSMTo, l.StateName(tr.Destination)),
			attribute.String(keys.FSMTrigger, l.TriggerName(tr.Trigger)),
			attribute.String(keys.FSMKind, tr.Kind.String()),
		))
	}
}
