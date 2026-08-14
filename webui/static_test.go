package webui

import (
	"net/http"
	"net/http/httptest"
	"testing/fstest"
	"testing"
)

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                      {Data: []byte("<!doctype html>APP")},
		"index.html.br":                   {Data: []byte("BR-INDEX")},
		"index.html.gz":                   {Data: []byte("GZ-INDEX")},
		"favicon.svg":                     {Data: []byte("<svg/>")},
		"_app/immutable/chunks/app.js":    {Data: []byte("APPJS")},
		"_app/immutable/chunks/app.js.br": {Data: []byte("APPJS-BR")},
	}
}

func do(h http.Handler, method, target, acceptEnc string) *http.Response {
	req := httptest.NewRequest(method, target, nil)
	if acceptEnc != "" {
		req.Header.Set("Accept-Encoding", acceptEnc)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestSPAServesAsset(t *testing.T) {
	resp := do(SPAHandler(testAssets()), "GET", "/favicon.svg", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestSPAFallbackToIndex(t *testing.T) {
	// A deep client-side route with no matching file must return index.html.
	resp := do(SPAHandler(testAssets()), "GET", "/nodes/abc", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestSPAPrefersBrotli(t *testing.T) {
	resp := do(SPAHandler(testAssets()), "GET", "/index.html", "gzip, br")
	if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
		t.Fatalf("content-encoding = %q, want br", enc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q (should be the logical type, not the .br type)", ct)
	}
}

func TestSPAFallsBackToGzip(t *testing.T) {
	// app.js has only a .br sibling; a gzip-only client gets the identity file.
	resp := do(SPAHandler(testAssets()), "GET", "/_app/immutable/chunks/app.js", "gzip")
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("content-encoding = %q, want identity", enc)
	}
	// index.html has a .gz sibling; a gzip-only client gets gzip.
	resp = do(SPAHandler(testAssets()), "GET", "/", "gzip")
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", enc)
	}
}

func TestSPAImmutableCaching(t *testing.T) {
	resp := do(SPAHandler(testAssets()), "GET", "/_app/immutable/chunks/app.js", "")
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("immutable cache-control = %q", cc)
	}
	resp = do(SPAHandler(testAssets()), "GET", "/index.html", "")
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control = %q", cc)
	}
}

func TestSPARejectsNonGet(t *testing.T) {
	resp := do(SPAHandler(testAssets()), "POST", "/", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
