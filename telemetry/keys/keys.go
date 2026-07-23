// SPDX-License-Identifier: BSD-3-Clause

// Package keys defines the single, shared attribute-key vocabulary used across
// the Kyanite telemetry surface — structured logs (log/slog) and, later, trace
// spans and span events. Divergent key names between logs and spans silently
// break cross-correlation queries, so both must draw from this one package.
//
// Keys follow OpenTelemetry semantic conventions where an established name
// exists, and a project-local hw.* namespace for hardware-access attributes
// that have no upstream convention. Keys are plain strings (type [Key], an
// alias for string) so they can be used directly with slog.String(key, …) and
// with attribute.String(key, …)/attribute.Key(key).Int(…) without this
// foundational package depending on the OpenTelemetry module — keeping it
// stdlib-only and TamaGo-clean.
package keys

// Key is an attribute key. It is an alias for string so a Key value is usable
// wherever either a slog attribute key or an OpenTelemetry attribute key is
// expected.
type Key = string

// Trace-correlation keys stamped onto log records from the active span context.
const (
	// TraceID is the W3C trace id of the record's span context.
	TraceID Key = "trace_id"
	// SpanID is the span id of the record's span context.
	SpanID Key = "span_id"
	// LogSeverity is the slog level recorded on a span event escalated from a
	// log record.
	LogSeverity Key = "log.severity"
)

// Hardware-access keys (project-local hw.* namespace). Values must be bounded,
// low-cardinality identities (bus number, device address, register offset,
// chip/block name) — never payload bytes or formatted strings.
const (
	// HWBus is the bus index (e.g. I2C/SPI controller number).
	HWBus Key = "hw.bus"
	// HWAddr is the device address on a bus.
	HWAddr Key = "hw.addr"
	// HWReg is a register offset within a device or block.
	HWReg Key = "hw.reg"
	// HWChip identifies a chip or on-die block.
	HWChip Key = "hw.chip"
	// HWOp is the operation name (e.g. "read", "write", "erase").
	HWOp Key = "hw.op"
)

// Service/identity keys (set once on the telemetry Resource, not per record).
const (
	// ServiceName is the emitting service/runtime name (e.g. "vein", "cairn").
	ServiceName Key = "service.name"
)
