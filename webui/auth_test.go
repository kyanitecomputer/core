package webui_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	v1 "src.kyanite.computer/schema/gen/go/schema/v1"
	"src.kyanite.computer/schema/gen/go/schema/v1/schemav1connect"

	"src.kyanite.computer/core/webui"
)

func adminChecker(u, p string) (v1.UserRole, bool) {
	if u == "admin" && p == "admin" {
		return v1.UserRole_USER_ROLE_ADMIN, true
	}
	return v1.UserRole_USER_ROLE_UNSPECIFIED, false
}

func TestAuthHandlerLoginSetsCookieAndSessionRestores(t *testing.T) {
	h := webui.NewAuthHandler(adminChecker)
	srv := httptest.NewServer(webui.Handler(h, nil))
	t.Cleanup(srv.Close)

	// The connect client shares the test server's cookie-less http.Client; drive
	// the cookie manually to prove Set-Cookie / Cookie round-trips.
	base := srv.URL + "/api/connect"
	client := schemav1connect.NewAuthServiceClient(srv.Client(), base)

	resp, err := client.Login(context.Background(), connect.NewRequest(&v1.LoginRequest{
		Username: "admin", Password: "admin",
	}))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if resp.Msg.GetAccessToken() == "" {
		t.Fatal("empty access token")
	}
	cookie := resp.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("Login did not set a session cookie")
	}

	// GetCurrentUser with the cookie restores the session.
	req := connect.NewRequest(&v1.GetCurrentUserRequest{})
	req.Header().Set("Cookie", cookie)
	cur, err := client.GetCurrentUser(context.Background(), req)
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	if cur.Msg.GetUsername() != "admin" || cur.Msg.GetRole() != v1.UserRole_USER_ROLE_ADMIN {
		t.Fatalf("restored user = %q/%v", cur.Msg.GetUsername(), cur.Msg.GetRole())
	}
	if cur.Msg.GetAccessToken() == "" {
		t.Fatal("GetCurrentUser did not re-mint an access token")
	}
}

func TestAuthHandlerRejectsBadCreds(t *testing.T) {
	h := webui.NewAuthHandler(adminChecker)
	srv := httptest.NewServer(webui.Handler(h, nil))
	t.Cleanup(srv.Close)
	client := schemav1connect.NewAuthServiceClient(srv.Client(), srv.URL+"/api/connect")

	_, err := client.Login(context.Background(), connect.NewRequest(&v1.LoginRequest{
		Username: "admin", Password: "nope",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad-cred code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestGetCurrentUserWithoutCookieUnauthenticated(t *testing.T) {
	h := webui.NewAuthHandler(adminChecker)
	srv := httptest.NewServer(webui.Handler(h, nil))
	t.Cleanup(srv.Close)
	client := schemav1connect.NewAuthServiceClient(srv.Client(), srv.URL+"/api/connect")

	_, err := client.GetCurrentUser(context.Background(), connect.NewRequest(&v1.GetCurrentUserRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no-cookie code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}
