package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "src.kyanite.computer/schema/gen/go/schema/v1"
	"src.kyanite.computer/schema/gen/go/schema/v1/schemav1connect"
)

// sessionCookie is the HttpOnly refresh-cookie name. The __Host- prefix binds it
// to the exact origin over HTTPS with Path=/ and no Domain (per the facet
// contract §3.3).
const sessionCookie = "__Host-facet_session"

// accessTokenTTL is how long an issued access token is valid.
const accessTokenTTL = time.Hour

// CredentialChecker validates a username/password and returns the user's role.
// It is supplied by the target (e.g. from the config store); the auth handler
// owns only session/cookie mechanics, not the credential source.
type CredentialChecker func(username, password string) (role v1.UserRole, ok bool)

// AuthHandler is a minimal same-origin implementation of schema.v1.AuthService:
// password Login returns a short-lived access token in the body and sets an
// HttpOnly refresh cookie; GetCurrentUser re-mints the access token from the
// cookie (boot/reload survival); Logout clears it. Unimplemented RPCs
// (WebAuthn, OIDC, ChangePassword, …) return CodeUnimplemented via the embedded
// base.
//
// This is a pre-callout placeholder: the access token is an opaque random
// string that is not yet validated by the NATS auth-callout. Binding the token
// to the callout-minted NATS user (facet-auth-plan §3) is layered on later; the
// handler exists so the SPA login/session flow works same-origin today.
type AuthHandler struct {
	schemav1connect.UnimplementedAuthServiceHandler

	check CredentialChecker

	mu       sync.Mutex
	sessions map[string]session // cookie id → session
}

type session struct {
	username string
	role     v1.UserRole
}

// NewAuthHandler builds an AuthHandler validating credentials with check.
func NewAuthHandler(check CredentialChecker) *AuthHandler {
	return &AuthHandler{check: check, sessions: make(map[string]session)}
}

func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Login validates credentials, creates a server-side session, sets the HttpOnly
// refresh cookie, and returns a fresh access token.
func (a *AuthHandler) Login(_ context.Context, req *connect.Request[v1.LoginRequest]) (*connect.Response[v1.LoginResponse], error) {
	role, ok := a.check(req.Msg.GetUsername(), req.Msg.GetPassword())
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	}
	id, err := randomID()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	a.mu.Lock()
	a.sessions[id] = session{username: req.Msg.GetUsername(), role: role}
	a.mu.Unlock()

	tok, err := randomID()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := connect.NewResponse(&v1.LoginResponse{
		AccessToken: tok,
		ExpiresAt:   timestamppb.New(time.Now().Add(accessTokenTTL)),
		Username:    req.Msg.GetUsername(),
		Role:        role,
	})
	setCookie(resp.Header(), (&http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}).String())
	return resp, nil
}

// GetCurrentUser re-mints an access token for the session named by the refresh
// cookie. Any failure maps to Unauthenticated so the SPA cleanly lands on login.
func (a *AuthHandler) GetCurrentUser(_ context.Context, req *connect.Request[v1.GetCurrentUserRequest]) (*connect.Response[v1.GetCurrentUserResponse], error) {
	s, ok := a.lookup(req.Header())
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no session"))
	}
	tok, err := randomID()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.GetCurrentUserResponse{
		Username:    s.username,
		Role:        s.role,
		AccessToken: tok,
		ExpiresAt:   timestamppb.New(time.Now().Add(accessTokenTTL)),
	}), nil
}

// Logout revokes the session server-side and expires the cookie.
func (a *AuthHandler) Logout(_ context.Context, req *connect.Request[v1.LogoutRequest]) (*connect.Response[v1.LogoutResponse], error) {
	if id, ok := cookieID(req.Header()); ok {
		a.mu.Lock()
		delete(a.sessions, id)
		a.mu.Unlock()
	}
	resp := connect.NewResponse(&v1.LogoutResponse{})
	setCookie(resp.Header(), (&http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}).String())
	return resp, nil
}

func (a *AuthHandler) lookup(h http.Header) (session, bool) {
	id, ok := cookieID(h)
	if !ok {
		return session{}, false
	}
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	return s, ok
}

// cookieID extracts the refresh-cookie value from request headers.
func cookieID(h http.Header) (string, bool) {
	r := &http.Request{Header: h}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func setCookie(h http.Header, v string) { h.Add("Set-Cookie", v) }
