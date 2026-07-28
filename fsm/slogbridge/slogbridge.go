// SPDX-License-Identifier: BSD-3-Clause

// Package slogbridge adapts an fsm machine to structured logging with log/slog.
// It logs one record per completed transition at Debug. Trace and span IDs are
// stamped by the telemetry logging handler from the context, so they are not
// added here.
package slogbridge

import (
	"context"
	"log/slog"

	"src.kyanite.computer/core/fsm"
	"src.kyanite.computer/core/telemetry/keys"
)

// Observer returns an [fsm.Observer] that logs each completed transition at
// Debug via logger, using preformatted attributes and the shared key
// vocabulary. name identifies the machine; l provides low-cardinality state and
// trigger names. Attach it with
// Builder.Observe(slogbridge.Observer[S, T, E](...)).
func Observer[S ~uint8, T ~uint8, E any](logger *slog.Logger, name string, l fsm.Labeler[S, T]) fsm.Observer[S, T, E] {
	return func(ctx context.Context, tr fsm.Transition[S, T], _ E) {
		if !logger.Enabled(ctx, slog.LevelDebug) {
			return
		}
		logger.LogAttrs(ctx, slog.LevelDebug, "fsm.transition",
			slog.String(keys.FSMName, name),
			slog.String(keys.FSMFrom, l.StateName(tr.Source)),
			slog.String(keys.FSMTo, l.StateName(tr.Destination)),
			slog.String(keys.FSMTrigger, l.TriggerName(tr.Trigger)),
			slog.String(keys.FSMKind, tr.Kind.String()),
		)
	}
}
