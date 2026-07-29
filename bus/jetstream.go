// SPDX-License-Identifier: BSD-3-Clause

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// StorageKind selects the JetStream backing store for a stream. Scree is the
// device default: HMAC-Merkle authenticated flash storage (tamper-evident, which
// the audit stream relies on). Memory and File are useful in tests and on hosts.
type StorageKind string

const (
	// StorageScree is the flash-backed, authenticated store (device default).
	StorageScree StorageKind = "scree"
	// StorageMemory is in-RAM storage (lost on restart).
	StorageMemory StorageKind = "memory"
	// StorageFile is on-disk storage.
	StorageFile StorageKind = "file"
)

// RetentionKind selects a stream's retention policy.
type RetentionKind string

const (
	// RetentionLimits keeps messages until a size/age/count limit is hit.
	RetentionLimits RetentionKind = "limits"
	// RetentionInterest keeps messages only while a consumer is interested.
	RetentionInterest RetentionKind = "interest"
	// RetentionWorkQueue removes a message once consumed.
	RetentionWorkQueue RetentionKind = "workqueue"
)

// ErrNoStreamName is returned when a stream is declared without a name.
var ErrNoStreamName = errors.New("bus: stream name is required")

// StreamConfig declares a JetStream stream. Zero value needs Name and Subjects;
// Normalize (EnsureStream does) fills the rest.
type StreamConfig struct {
	// Name identifies the stream. Required.
	Name string
	// Subjects captured by the stream. Required for a usable stream.
	Subjects []string
	// Storage backing the stream. Defaults to Scree.
	Storage StorageKind
	// Retention policy. Defaults to Limits.
	Retention RetentionKind
	// MaxBytes bounds total stored bytes (0 = unbounded).
	MaxBytes int64
	// MaxMsgs bounds total stored messages (0 = unbounded).
	MaxMsgs int64
	// MaxAge bounds message age (0 = unbounded).
	MaxAge time.Duration
	// Replicas sets the replication factor. Defaults to 1.
	Replicas int
}

// Normalize fills unset fields with defaults. It is idempotent.
func (c *StreamConfig) Normalize() {
	if c.Storage == "" {
		c.Storage = StorageScree
	}
	if c.Retention == "" {
		c.Retention = RetentionLimits
	}
	if c.Replicas < 1 {
		c.Replicas = 1
	}
}

// Streams manages JetStream streams and durable publishing on a connection.
type Streams struct {
	nc *nats.Conn
	js nats.JetStreamContext
}

// Streams returns a JetStream helper for the connection.
func (c *Conn) Streams() (*Streams, error) {
	js, err := c.nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("bus: jetstream context: %w", err)
	}
	return &Streams{nc: c.nc, js: js}, nil
}

// EnsureStream creates the stream if it does not already exist. It is
// idempotent: an existing stream is left as-is (reconfiguration is a deliberate
// operation, not a side effect of ensuring). Scree-backed streams are created
// via the raw JetStream API because the custom storage type is not expressible
// through the typed client config.
func (s *Streams) EnsureStream(ctx context.Context, cfg StreamConfig) error {
	if cfg.Name == "" {
		return ErrNoStreamName
	}
	cfg.Normalize()

	exists, err := s.streamExists(ctx, cfg.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.createStream(ctx, cfg)
}

// Publish durably appends data on subject (which must be captured by a stream),
// waiting for the server ack, and returns the assigned stream sequence.
func (s *Streams) Publish(ctx context.Context, subject string, data []byte) (uint64, error) {
	ack, err := s.js.Publish(subject, data, nats.Context(ctx))
	if err != nil {
		return 0, fmt.Errorf("bus: jetstream publish to %q: %w", subject, err)
	}
	return ack.Sequence, nil
}

// jsAPIError mirrors the error object in a JetStream API response.
type jsAPIError struct {
	Code        int    `json:"code"`
	ErrCode     int    `json:"err_code"`
	Description string `json:"description"`
}

type jsAPIResponse struct {
	Type  string      `json:"type"`
	Error *jsAPIError `json:"error"`
}

// streamNotFoundErrCode is the JetStream API err_code for a missing stream.
const streamNotFoundErrCode = 10059

func (s *Streams) streamExists(ctx context.Context, name string) (bool, error) {
	apiErr, err := s.apiRequest(ctx, "$JS.API.STREAM.INFO."+name, nil)
	if err != nil {
		return false, err
	}
	if apiErr == nil {
		return true, nil
	}
	if apiErr.ErrCode == streamNotFoundErrCode || apiErr.Code == 404 {
		return false, nil
	}
	return false, fmt.Errorf("bus: stream info %q: %s (code %d)", name, apiErr.Description, apiErr.Code)
}

func (s *Streams) createStream(ctx context.Context, cfg StreamConfig) error {
	req := map[string]any{
		"name":         cfg.Name,
		"subjects":     cfg.Subjects,
		"storage":      string(cfg.Storage),
		"retention":    string(cfg.Retention),
		"num_replicas": cfg.Replicas,
	}
	if cfg.MaxBytes > 0 {
		req["max_bytes"] = cfg.MaxBytes
	}
	if cfg.MaxMsgs > 0 {
		req["max_msgs"] = cfg.MaxMsgs
	}
	if cfg.MaxAge > 0 {
		req["max_age"] = int64(cfg.MaxAge)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("bus: marshal stream config: %w", err)
	}
	apiErr, err := s.apiRequest(ctx, "$JS.API.STREAM.CREATE."+cfg.Name, payload)
	if err != nil {
		return err
	}
	if apiErr != nil {
		return fmt.Errorf("bus: create stream %q: %s (code %d)", cfg.Name, apiErr.Description, apiErr.Code)
	}
	return nil
}

// apiRequest sends a JetStream API request and decodes the standard response
// envelope, returning its error object (if any) separately from transport
// errors.
func (s *Streams) apiRequest(ctx context.Context, subject string, payload []byte) (*jsAPIError, error) {
	msg, err := s.nc.RequestWithContext(ctx, subject, payload)
	if err != nil {
		return nil, fmt.Errorf("bus: jetstream api %q: %w", subject, err)
	}
	var resp jsAPIResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("bus: decode jetstream api response: %w", err)
	}
	return resp.Error, nil
}
