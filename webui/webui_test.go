package webui_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "src.kyanite.computer/schema/gen/go/schema/v1"
	"src.kyanite.computer/schema/gen/go/schema/v1/schemav1connect"

	"src.kyanite.computer/core/webui"
)

// stubAuth is a placeholder AuthService used to prove the ConnectRPC transport
// end to end. The real implementation (NATS JWT/NKey auth callout) replaces it
// in core/auth.
type stubAuth struct {
	schemav1connect.UnimplementedAuthServiceHandler
}

func (stubAuth) Login(_ context.Context, req *connect.Request[v1.LoginRequest]) (*connect.Response[v1.LoginResponse], error) {
	if req.Msg.GetUsername() != "admin" || req.Msg.GetPassword() != "admin" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	}
	return connect.NewResponse(&v1.LoginResponse{
		AccessToken: "spike-token",
		ExpiresAt:   timestamppb.New(time.Now().Add(time.Hour)),
		Username:    "admin",
		Role:        v1.UserRole_USER_ROLE_ADMIN,
	}), nil
}

// TestAuthLoginOverHTTP proves a real ConnectRPC AuthService round-trip through
// the webui mux served over net/http (the same path that runs over lneto on
// bare metal).
func TestAuthLoginOverHTTP(t *testing.T) {
	srv := httptest.NewServer(webui.Mux(stubAuth{}))
	t.Cleanup(srv.Close)

	client := schemav1connect.NewAuthServiceClient(srv.Client(), srv.URL)

	resp, err := client.Login(context.Background(), connect.NewRequest(&v1.LoginRequest{
		Username: "admin",
		Password: "admin",
	}))
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if got := resp.Msg.GetUsername(); got != "admin" {
		t.Fatalf("Login() username = %q, want admin", got)
	}
	if resp.Msg.GetAccessToken() == "" {
		t.Fatal("Login() returned empty access token")
	}
	if got := resp.Msg.GetRole(); got != v1.UserRole_USER_ROLE_ADMIN {
		t.Fatalf("Login() role = %v, want ADMIN", got)
	}

	_, err = client.Login(context.Background(), connect.NewRequest(&v1.LoginRequest{
		Username: "admin",
		Password: "wrong",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Login() with bad password: code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}
