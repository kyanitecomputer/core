// Package webui serves the management HTTP surface shared by the Kyanite device
// runtimes (vein, cairn): the ConnectRPC services (auth, users, …) and, in time,
// the embedded facet single-page app.
//
// Transport note: serving currently uses the standard library net/http server
// on top of TamaGo's net.SocketFunc, which routes through the lneto TCP/IP
// stack on bare metal. ConnectRPC handlers are stdlib http.Handlers and the
// auth/user RPCs are unary, so HTTP/1.1 is sufficient.
//
// TODO(lneto): migrate the HTTP serving layer from net/http to lneto-native
// (httpraw) once the ConnectRPC-over-lneto path is hardened, so the whole
// networking stack is lneto end to end.
package webui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"src.kyanite.computer/schema/gen/go/schema/v1/schemav1connect"
)

// connectBase is the same-origin base path the facet SPA calls ConnectRPC on
// (facet-backend-contract §3.1: /api/connect/schema.v1.<Service>/<Method>).
const connectBase = "/api/connect"

// Mux builds the management HTTP handler. It mounts the provided ConnectRPC
// service handlers; the embedded SPA and additional services are added as the
// core grows.
func Mux(auth schemav1connect.AuthServiceHandler) *http.ServeMux {
	mux := http.NewServeMux()
	authPath, authHandler := schemav1connect.NewAuthServiceHandler(auth)
	mux.Handle(authPath, authHandler)
	return mux
}

// Handler builds the full same-origin management surface: the ConnectRPC
// AuthService under /api/connect and the facet SPA (when assets is non-nil) for
// everything else. Either argument may be nil (no auth mounted / no SPA, in
// which case unmatched paths 404). This is the handler cairn and vein serve
// over TLS on :443.
func Handler(auth schemav1connect.AuthServiceHandler, assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	if auth != nil {
		p, h := schemav1connect.NewAuthServiceHandler(auth)
		mux.Handle(connectBase+p, http.StripPrefix(connectBase, h))
	}
	if assets != nil {
		mux.Handle("/", SPAHandler(assets))
	}
	return mux
}

// ListenAndServe serves h on the given TCP port until ctx is cancelled. On bare
// metal the listener is provided by the lneto stack via net.SocketFunc; on a
// host it is an ordinary kernel socket.
func ListenAndServe(ctx context.Context, port uint16, h http.Handler) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("webui: listen :%d: %w", port, err)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("webui: serve: %w", err)
	}
	return nil
}
