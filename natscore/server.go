package natscore

import (
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Start starts an embedded NATS server and waits until it accepts clients.
func Start(opts *server.Options) (*server.Server, error) {
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("create NATS server: %w", err)
	}

	go func() {
		_ = server.Run(srv)
	}()

	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, fmt.Errorf("NATS server did not become ready")
	}
	return srv, nil
}
