// Package natscore configures the embedded NATS management core shared by the
// Kyanite device runtimes (vein, cairn): a bounded in-process NATS server with
// JetStream backed by Scree.
package natscore

import (
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

const (
	defaultClientPort    = 4222
	defaultWebSocketPort = 8080
	defaultMemoryLimit   = 64 * 1024 * 1024
	defaultStoreLimit    = 64 * 1024 * 1024
	defaultMaxPayload    = 256 * 1024
)

// Options returns bounded NATS server options for the management plane.
func Options(storeDir string) *server.Options {
	return &server.Options{
		Host:                   "0.0.0.0",
		Port:                   defaultClientPort,
		JetStream:              true,
		DisableJetStreamBanner: true,
		StoreDir:               storeDir,
		NoSigs:                 true,
		JetStreamMaxMemory:     defaultMemoryLimit,
		JetStreamMaxStore:      defaultStoreLimit,
		MaxConn:                16,
		MaxControlLine:         512,
		MaxPayload:             defaultMaxPayload,
		MaxPending:             defaultMaxPayload,
		WriteDeadline:          10 * time.Second,
		Websocket: server.WebsocketOpts{
			Host:             "0.0.0.0",
			Port:             defaultWebSocketPort,
			NoTLS:            true,
			HandshakeTimeout: 5 * time.Second,
		},
	}
}
