// SPDX-License-Identifier: BSD-3-Clause

package slogbridge_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"src.kyanite.computer/core/fsm"
	"src.kyanite.computer/core/fsm/slogbridge"
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

func machine(t *testing.T, o fsm.Observer[st, trig, ev]) *fsm.Config[st, trig, ev] {
	t.Helper()
	b := fsm.New[st, trig, ev](a).Observe(o)
	b.State(a).Permit(fire, z)
	b.State(z)
	cfg, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cfg
}

func TestObserverLogsTransition(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := machine(t, slogbridge.Observer[st, trig, ev](logger, "m", labeler()))

	if err := cfg.Instance().Fire(context.Background(), fire, ev{}); err != nil {
		t.Fatalf("Fire: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		`"fsm.name":"m"`,
		`"fsm.from":"a"`,
		`"fsm.to":"z"`,
		`"fsm.trigger":"fire"`,
		`"fsm.kind":"external"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output %q missing %q", out, want)
		}
	}
}

func TestObserverRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	// Info level: Debug transition logs are suppressed.
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := machine(t, slogbridge.Observer[st, trig, ev](logger, "m", labeler()))

	_ = cfg.Instance().Fire(context.Background(), fire, ev{})
	if buf.Len() != 0 {
		t.Fatalf("expected no output at Info level, got %q", buf.String())
	}
}
