package webui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// CertStore is the minimal persistence needed for the TLS material.
// cfgstore.Store satisfies it directly (as does sshd.KeyStore).
type CertStore interface {
	Get(key string) ([]byte, error)
	Set(key string, val []byte) error
}

// KV keys holding the PEM-encoded TLS material. The same slots hold either a
// generated self-signed pair or an operator-uploaded certificate, so uploading
// simply overwrites them and survives reboot.
const (
	keyTLSCert = "web.tls_cert" // PEM CERTIFICATE (leaf, optional chain)
	keyTLSKey  = "web.tls_key"  // PEM PRIVATE KEY (PKCS#8)
)

// selfSignedValidity is the lifetime of a generated self-signed certificate.
const selfSignedValidity = 10 * 365 * 24 * time.Hour

// Certificate returns the TLS certificate to serve with. If an uploaded or
// previously generated pair is present in the store it is used; otherwise a
// self-signed certificate is generated for hosts (DNS names and/or IPs),
// persisted, and returned. Browsers warn on the self-signed default until an
// operator uploads a trusted certificate via [SetCertificate].
func Certificate(store CertStore, hosts []string) (tls.Certificate, error) {
	certPEM, cerr := store.Get(keyTLSCert)
	keyPEM, kerr := store.Get(keyTLSKey)
	if cerr == nil && kerr == nil && len(certPEM) > 0 && len(keyPEM) > 0 {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err == nil {
			return cert, nil
		}
		// Stored material is corrupt; fall through and regenerate.
	}

	certPEM, keyPEM, err := generateSelfSigned(hosts)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("webui: self-signed cert: %w", err)
	}
	if err := store.Set(keyTLSCert, certPEM); err != nil {
		println("[web] WARNING: could not persist TLS cert:", err.Error())
	}
	if err := store.Set(keyTLSKey, keyPEM); err != nil {
		println("[web] WARNING: could not persist TLS key:", err.Error())
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// SetCertificate validates a PEM certificate/key pair and stores it, replacing
// the current (possibly self-signed) material. The change takes effect on the
// next server start. It is the entry point for operator certificate upload.
func SetCertificate(store CertStore, certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("webui: invalid certificate/key pair: %w", err)
	}
	if err := store.Set(keyTLSCert, certPEM); err != nil {
		return fmt.Errorf("webui: store cert: %w", err)
	}
	if err := store.Set(keyTLSKey, keyPEM); err != nil {
		return fmt.Errorf("webui: store key: %w", err)
	}
	return nil
}

// generateSelfSigned creates an ECDSA P-256 self-signed certificate valid for
// the given hosts (parsed as IPs when possible, else treated as DNS names).
func generateSelfSigned(hosts []string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	cn := "kyanite"
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Kyanite"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if addr, perr := netip.ParseAddr(h); perr == nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.IP(addr.AsSlice()))
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// ListenAndServeTLS serves h over HTTPS on the given TCP port until ctx is
// cancelled. Go's net/http negotiates HTTP/2 automatically via TLS ALPN. On
// bare metal the listener comes from the lneto stack via net.SocketFunc.
func ListenAndServeTLS(ctx context.Context, port uint16, h http.Handler, cert tls.Certificate) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("webui: listen :%d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("webui: serve tls: %w", err)
	}
	return nil
}

// RedirectHandler returns a handler that permanently redirects every request to
// the same host/path over https. Serve it on :80 with [ListenAndServe] while TLS
// terminates on :443.
func RedirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}
