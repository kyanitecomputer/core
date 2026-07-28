// SPDX-License-Identifier: BSD-3-Clause

package otelbridge_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"src.kyanite.computer/core/fsm"
	"src.kyanite.computer/core/fsm/otelbridge"
)

type st uint8

const (
	a st = iota
	z
)

type trig uint8

const fire trig = 0

type ev struct{}

func labeler() fsm.Labeler[st, trig] {
	return fsm.Labeler[st, trig]{
		State: func(s st) string {
			if s == a {
				return "a"
			}
			return "z"
		},
		Trigger: func(trig) string { return "fire" },
	}
}

func machine(t *testing.T) *fsm.Config[st, trig, ev] {
	t.Helper()
	b := fsm.New[st, trig, ev](a).Observe(otelbridge.Observer[st, trig, ev]("m", labeler()))
	b.State(a).Permit(fire, z)
	b.State(z)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cfg
}

func TestObserverRecordsSpanEvent(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ctx, span := tp.Tracer("test").Start(context.Background(), "root")

	if err := machine(t).Instance().Fire(ctx, fire, ev{}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(ended))
	}
	events := ended[0].Events()
	if len(events) != 1 || events[0].Name != "fsm.transition" {
		t.Fatalf("span events = %+v, want one fsm.transition", events)
	}
	attrs := map[string]string{}
	for _, kv := range events[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	for k, want := range map[string]string{
		"fsm.name":    "m",
		"fsm.from":    "a",
		"fsm.to":      "z",
		"fsm.trigger": "fire",
		"fsm.kind":    "external",
	} {
		if attrs[k] != want {
			t.Errorf("event attr %s = %q, want %q", k, attrs[k], want)
		}
	}
}

func TestObserverSkipsNonRecordingSpan(t *testing.T) {
	// No tracer provider set: SpanFromContext returns a non-recording span, so
	// the observer must be a safe no-op.
	if err := machine(t).Instance().Fire(context.Background(), fire, ev{}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
}
