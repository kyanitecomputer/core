package webui

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

// memStore is an in-memory CertStore for tests.
type memStore map[string][]byte

func (m memStore) Get(k string) ([]byte, error) {
	v, ok := m[k]
	if !ok {
		return nil, http.ErrNoCookie // any non-nil error signals "absent"
	}
	return v, nil
}
func (m memStore) Set(k string, v []byte) error { m[k] = v; return nil }

func TestCertificateGeneratesAndPersists(t *testing.T) {
	store := memStore{}
	cert, err := Certificate(store, []string{"ast2700-dcscm", "192.168.0.2"})
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	if len(store[keyTLSCert]) == 0 || len(store[keyTLSKey]) == 0 {
		t.Fatal("cert/key not persisted")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "ast2700-dcscm" {
		t.Fatalf("DNS SANs = %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.168.0.2" {
		t.Fatalf("IP SANs = %v", leaf.IPAddresses)
	}
}

func TestCertificateReusesPersisted(t *testing.T) {
	store := memStore{}
	c1, _ := Certificate(store, []string{"host"})
	c2, err := Certificate(store, []string{"host"})
	if err != nil {
		t.Fatalf("second Certificate: %v", err)
	}
	// Same persisted material ⇒ identical leaf DER (no regeneration).
	if string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Fatal("certificate regenerated instead of reused")
	}
}

func TestSetCertificateValidates(t *testing.T) {
	store := memStore{}
	if err := SetCertificate(store, []byte("not a cert"), []byte("not a key")); err == nil {
		t.Fatal("expected validation error for garbage PEM")
	}
	// A generated pair round-trips through SetCertificate and is then reused.
	certPEM, keyPEM, err := generateSelfSigned([]string{"host"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := SetCertificate(store, certPEM, keyPEM); err != nil {
		t.Fatalf("SetCertificate: %v", err)
	}
	if string(store[keyTLSCert]) != string(certPEM) {
		t.Fatal("uploaded cert not stored")
	}
}

func TestRedirectHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "http://bmc.local:80/nodes?x=1", nil)
	rec := httptest.NewRecorder()
	RedirectHandler().ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://bmc.local/nodes?x=1" {
		t.Fatalf("Location = %q", loc)
	}
}
