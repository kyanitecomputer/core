// SPDX-License-Identifier: BSD-3-Clause

package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/micro"
)

// Re-exported micro types so callers get nats-micro ergonomics without importing
// the micro package directly.
type (
	// Request is a service request presented to a [Handler]. Respond, RespondJSON,
	// and Error send the reply; Data and Headers read the request.
	Request = micro.Request
	// Info is the discovery document returned on $SRV.INFO.
	Info = micro.Info
	// Stats is the statistics document returned on $SRV.STATS.
	Stats = micro.Stats
)

// Errors reported by the service surface.
var (
	// ErrNoServiceName is returned when a service is added without a name.
	ErrNoServiceName = errors.New("bus: service name is required")
	// ErrNoEndpointName is returned when an endpoint is added without a name.
	ErrNoEndpointName = errors.New("bus: endpoint name is required")
	// ErrNoHandler is returned when an endpoint is added without a handler.
	ErrNoHandler = errors.New("bus: endpoint handler is required")
)

// Handler processes one request. It receives a context so it can honor
// cancellation and carry trace correlation; the context is per-request and must
// not be retained. The handler replies via req.Respond / req.RespondJSON /
// req.Error.
type Handler func(ctx context.Context, req Request)

// ServiceConfig configures a microservice. Zero value needs at least Name; call
// Normalize (AddService does) to fill defaults.
type ServiceConfig struct {
	// Name is the service name reported by discovery. Required.
	Name string
	// Version is a SemVer string. Defaults to "0.0.0".
	Version string
	// Description annotates the service in discovery.
	Description string
	// Metadata annotates the service with arbitrary key/values.
	Metadata map[string]string
	// QueueGroup overrides the default queue group ("q") shared by endpoints,
	// which load-balances requests across instances of the same service.
	QueueGroup string
}

// Normalize fills unset fields with defaults. It is idempotent.
func (c *ServiceConfig) Normalize() {
	if c.Version == "" {
		c.Version = "0.0.0"
	}
}

// EndpointConfig configures one endpoint of a service.
type EndpointConfig struct {
	// Name identifies the endpoint in discovery and stats. Required.
	Name string
	// Subject is the subject the endpoint listens on. Defaults to Name (or, in a
	// group, the group prefix + Name).
	Subject string
	// QueueGroup overrides the service/group queue group for this endpoint.
	QueueGroup string
	// Metadata annotates the endpoint.
	Metadata map[string]string
	// Handler processes requests. Required.
	Handler Handler
}

// Service is a registered microservice. It answers discovery/stats on $SRV.>
// automatically and dispatches requests to its endpoints. Stop (or Drain on the
// connection) to tear it down.
type Service struct {
	svc micro.Service
}

// AddService registers a microservice on the connection.
func (c *Conn) AddService(cfg ServiceConfig) (*Service, error) {
	if cfg.Name == "" {
		return nil, ErrNoServiceName
	}
	cfg.Normalize()
	svc, err := micro.AddService(c.nc, micro.Config{
		Name:        cfg.Name,
		Version:     cfg.Version,
		Description: cfg.Description,
		Metadata:    cfg.Metadata,
		QueueGroup:  cfg.QueueGroup,
	})
	if err != nil {
		return nil, fmt.Errorf("bus: add service %q: %w", cfg.Name, err)
	}
	return &Service{svc: svc}, nil
}

// AddEndpoint registers an endpoint on the service.
func (s *Service) AddEndpoint(cfg EndpointConfig) error {
	return addEndpoint(func(name string, h micro.Handler, opts ...micro.EndpointOpt) error {
		return s.svc.AddEndpoint(name, h, opts...)
	}, cfg)
}

// Group returns a group whose endpoints are prefixed by prefix, allowing
// hierarchical subject topologies (e.g. "sensors" → "sensors.temperature").
func (s *Service) Group(prefix string) *Group {
	return &Group{g: s.svc.AddGroup(prefix)}
}

// Info returns the service discovery document.
func (s *Service) Info() Info { return s.svc.Info() }

// Stats returns per-endpoint statistics.
func (s *Service) Stats() Stats { return s.svc.Stats() }

// Stop drains the service subscriptions and marks it stopped.
func (s *Service) Stop() error { return s.svc.Stop() }

// Stopped reports whether Stop has been called.
func (s *Service) Stopped() bool { return s.svc.Stopped() }

// Group is a subject-prefixed grouping of endpoints on a service.
type Group struct {
	g micro.Group
}

// AddEndpoint registers an endpoint whose subject is prefixed by the group.
func (g *Group) AddEndpoint(cfg EndpointConfig) error {
	return addEndpoint(func(name string, h micro.Handler, opts ...micro.EndpointOpt) error {
		return g.g.AddEndpoint(name, h, opts...)
	}, cfg)
}

// Group returns a nested group prefixed by this group.
func (g *Group) Group(prefix string) *Group {
	return &Group{g: g.g.AddGroup(prefix)}
}

// addEndpoint validates cfg and registers it via add (service or group).
func addEndpoint(add func(string, micro.Handler, ...micro.EndpointOpt) error, cfg EndpointConfig) error {
	if cfg.Name == "" {
		return ErrNoEndpointName
	}
	if cfg.Handler == nil {
		return ErrNoHandler
	}
	opts := make([]micro.EndpointOpt, 0, 3)
	if cfg.Subject != "" {
		opts = append(opts, micro.WithEndpointSubject(cfg.Subject))
	}
	if cfg.QueueGroup != "" {
		opts = append(opts, micro.WithEndpointQueueGroup(cfg.QueueGroup))
	}
	if cfg.Metadata != nil {
		opts = append(opts, micro.WithEndpointMetadata(cfg.Metadata))
	}
	if err := add(cfg.Name, wrapHandler(cfg.Handler), opts...); err != nil {
		return fmt.Errorf("bus: add endpoint %q: %w", cfg.Name, err)
	}
	return nil
}

// wrapHandler adapts a context-aware bus.Handler to micro's context-free
// Handler. Each request gets a fresh context; trace-context extraction from
// request headers is layered in a later slice.
func wrapHandler(h Handler) micro.Handler {
	return micro.HandlerFunc(func(req micro.Request) {
		h(context.Background(), req)
	})
}
