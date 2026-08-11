// Package sshd implements an SSH management server shared by the Kyanite
// device runtimes (vein, cairn).
//
// It wraps golang.org/x/crypto/ssh and bridges net.Conn connections to the
// SSH protocol, providing the same interactive shell as the serial console
// (core/console) over an encrypted channel.
//
// # Host Key
//
// On first boot an Ed25519 host key is generated using crypto/rand and stored
// raw (64-byte private seed) in the KV store under "ssh.host_key".  On
// subsequent boots the key is loaded from the store, providing a stable server
// identity.
//
// # Authentication
//
// Two auth methods are supported:
//
//   - Password: stored under "ssh.password" (default: "admin").
//   - Public key: authorized keys stored as newline-separated OpenSSH wire
//     format (base64-encoded marshalled public key bytes) under
//     "ssh.authorized_keys".  Each line is one key.
//
// # Architecture
//
// ListenAndServe accepts TCP connections through Go's net package, then hands
// them off to golang.org/x/crypto/ssh for protocol handling.
//
// Each SSH session spawns the provided ShellFunc.  The ShellFunc receives the
// ssh.Channel as an io.ReadWriter; a fresh core/console.Shell per call is the
// intended callee (the shell carries per-session line/history state and is not
// safe to share across concurrent sessions).
//
// The package is stdlib + golang.org/x/crypto only and host-testable; it is
// used on GOOS=tamago targets where a net.SocketFunc is installed.
package sshd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

// KeyStore is the minimal interface required to persist SSH keys.
// cfgstore.Store satisfies this interface directly.
type KeyStore interface {
	Get(key string) ([]byte, error)
	Set(key string, val []byte) error
}

// ShellFunc is called with the accepted SSH channel to run an interactive
// session.  It should block until the session ends.
type ShellFunc func(rw io.ReadWriter)

// KV keys used by this package.
const (
	keyHostKey      = "ssh.host_key"        // raw 64-byte Ed25519 private key
	keyPassword     = "ssh.password"        // plaintext password (default "admin")
	keyAuthorizedKs = "ssh.authorized_keys" // newline-separated base64 public keys
)

// Server is the SSH management server.
type Server struct {
	cfg     *gossh.ServerConfig
	shellFn ShellFunc
}

// New creates a Server.
//
//   - store is used to persist the host key, password, and authorized keys.
//   - shellFn is called for each accepted interactive session.
//
// If host key generation fails (e.g. crypto/rand not seeded on first boot),
// New returns an error and the caller should retry or fall back to serial
// console only.
func New(store KeyStore, shellFn ShellFunc) (*Server, error) {
	signer, err := loadOrGenHostKey(store)
	if err != nil {
		return nil, fmt.Errorf("sshd: host key: %w", err)
	}

	password := loadPassword(store)
	authorizedKeys := loadAuthorizedKeys(store)

	cfg := &gossh.ServerConfig{
		// Password authentication.
		PasswordCallback: func(_ gossh.ConnMetadata, pass []byte) (*gossh.Permissions, error) {
			if string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("sshd: invalid password")
		},
	}

	// Public-key authentication (optional; only if any keys are stored).
	if len(authorizedKeys) > 0 {
		cfg.PublicKeyCallback = func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			keyBytes := key.Marshal()
			for _, ak := range authorizedKeys {
				if bytes.Equal(ak.Marshal(), keyBytes) {
					return &gossh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("sshd: unauthorized key")
		}
	}

	cfg.AddHostKey(signer)

	return &Server{cfg: cfg, shellFn: shellFn}, nil
}

// ListenAndServe accepts TCP connections on the given port and handles each as
// an SSH session. It runs forever; call from a goroutine.
func (s *Server) ListenAndServe(port uint16) {
	println("[ssh] listening on port", int(port))
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		println("[ssh] listen error:", err.Error())
		return
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			println("[ssh] accept error:", err.Error())
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn runs the SSH protocol handshake and session loop for one TCP
// connection.
func (s *Server) handleConn(c net.Conn) {
	sshConn, chans, reqs, err := gossh.NewServerConn(c, s.cfg)
	if err != nil {
		println("[ssh] handshake error:", err.Error())
		return
	}
	defer sshConn.Close()
	println("[ssh] connection from", sshConn.RemoteAddr().String(), "user:", string(sshConn.User()))

	// Discard global requests (keepalive etc.).
	go gossh.DiscardRequests(reqs)

	// One goroutine per session channel supports concurrent sessions; each
	// ShellFunc call builds its own shell instance (no shared state).
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(gossh.UnknownChannelType, "unsupported channel type")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			println("[ssh] channel accept error:", err.Error())
			return
		}
		go s.handleSession(ch, requests)
	}
}

// handleSession services one SSH "session" channel.  It processes channel
// requests (pty-req, shell, exec) and runs the shell for interactive sessions.
func (s *Server) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	defer ch.Close()

	shellStarted := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// Accept PTY requests (the client asked for a terminal emulator).
			// We don't actually set up a PTY — the shell does its own VT100
			// handling — but we must reply true so the client continues.
			if req.WantReply {
				req.Reply(true, nil)
			}

		case "shell":
			if req.WantReply {
				req.Reply(true, nil)
			}
			if !shellStarted {
				shellStarted = true
				// Run the shell; this blocks until the session ends.
				s.shellFn(ch)
			}
			return

		case "exec":
			// Run a command directly (non-interactive).  For now route exec
			// through the shell too (the user sees output but no prompt).
			if req.WantReply {
				req.Reply(true, nil)
			}
			if !shellStarted {
				shellStarted = true
				s.shellFn(ch)
			}
			return

		case "window-change":
			// Terminal resize — ignore; our shell has no concept of width.
			if req.WantReply {
				req.Reply(false, nil)
			}

		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// --- key management ---------------------------------------------------------

// loadOrGenHostKey loads an Ed25519 host key from store, or generates a new
// one and saves it.
func loadOrGenHostKey(store KeyStore) (gossh.Signer, error) {
	raw, err := store.Get(keyHostKey)
	if err != nil {
		// Generate a new Ed25519 key pair.
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate: %w", err)
		}
		raw = []byte(priv)
		if err := store.Set(keyHostKey, raw); err != nil {
			println("[ssh] WARNING: could not persist host key:", err.Error())
			// Continue with the in-memory key — it will be regenerated next boot.
		}
		println("[ssh] generated new Ed25519 host key")
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("corrupt host key (len %d)", len(raw))
	}
	signer, err := gossh.NewSignerFromKey(ed25519.PrivateKey(raw))
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	println("[ssh] host key fingerprint:", gossh.FingerprintSHA256(signer.PublicKey()))
	return signer, nil
}

// loadPassword returns the stored SSH password or the default "admin".
func loadPassword(store KeyStore) string {
	if v, err := store.Get(keyPassword); err == nil && len(v) > 0 {
		return string(v)
	}
	return "admin"
}

// loadAuthorizedKeys returns the list of authorized public keys from the store.
// Keys are stored one per line, base64-encoded (ssh.PublicKey.Marshal() output).
func loadAuthorizedKeys(store KeyStore) []gossh.PublicKey {
	raw, err := store.Get(keyAuthorizedKs)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var keys []gossh.PublicKey
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Stored as base64(key.Marshal()) — decode and parse.
		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			println("[ssh] skipping malformed authorized key:", err.Error())
			continue
		}
		key, err := gossh.ParsePublicKey(decoded)
		if err != nil {
			println("[ssh] skipping invalid authorized key:", err.Error())
			continue
		}
		keys = append(keys, key)
	}
	return keys
}
