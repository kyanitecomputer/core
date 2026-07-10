package natscore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestStartAcceptsClient(t *testing.T) {
	opts := Options(t.TempDir())
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.Websocket.Port = -1
	opts.NoLog = true

	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	nc.Close()
}

func TestScreeJetStreamStore(t *testing.T) {
	if _, err := RegisterRAMStore(8 * 1024 * 1024); err != nil {
		t.Fatalf("RegisterRAMStore() error = %v", err)
	}

	opts := Options(t.TempDir())
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.Websocket.Port = -1
	opts.NoLog = true

	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect() error = %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream() error = %v", err)
	}
	resp, err := nc.Request("$JS.API.STREAM.CREATE.TEST", []byte(`{"name":"TEST","subjects":["test"],"storage":"scree"}`), time.Second)
	if err != nil {
		t.Fatalf("create stream request error = %v", err)
	}
	var createResp struct {
		Error *struct {
			Code        int    `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Data, &createResp); err != nil {
		t.Fatalf("decode create stream response: %v", err)
	}
	if createResp.Error != nil {
		t.Fatalf("create stream error = %d %s", createResp.Error.Code, createResp.Error.Description)
	}
	if _, err := js.Publish("test", []byte("hello")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	msg, err := js.GetMsg("TEST", 1)
	if err != nil {
		t.Fatalf("GetMsg() error = %v", err)
	}
	if string(msg.Data) != "hello" {
		t.Fatalf("GetMsg() data = %q, want %q", string(msg.Data), "hello")
	}
}
