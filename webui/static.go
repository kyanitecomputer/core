package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAHandler serves a SvelteKit adapter-static single-page application from a
// read-only asset filesystem (typically an embed.FS subtree holding facet's
// `build/` output). It provides the behaviour that build expects:
//
//   - Client-side routing fallback: any GET that is not a real asset returns
//     index.html, so deep links like /nodes/x resolve in the browser.
//   - Transparent precompressed negotiation: when the client accepts br/gzip and
//     a sibling <name>.br / <name>.gz exists (adapter-static `precompress: true`),
//     it is served with the matching Content-Encoding and the original type.
//   - Correct Content-Type by extension and long-lived immutable caching for the
//     content-hashed assets under _app/immutable/ (everything else no-cache so a
//     new deploy is picked up immediately).
//
// The handler is stdlib-only and host-testable with fstest.MapFS.
func SPAHandler(assets fs.FS) http.Handler { return &spaHandler{assets: assets} }

type spaHandler struct{ assets fs.FS }

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	data, ctype, enc, ok := h.load(r, name)
	if !ok {
		// SPA fallback: unknown client-side routes render index.html.
		name = "index.html"
		if data, ctype, enc, ok = h.load(r, name); !ok {
			http.NotFound(w, r)
			return
		}
	}

	hdr := w.Header()
	hdr.Set("Content-Type", ctype)
	if strings.HasPrefix(name, "_app/immutable/") {
		hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hdr.Set("Cache-Control", "no-cache")
	}
	if enc != "" {
		hdr.Set("Content-Encoding", enc)
		hdr.Add("Vary", "Accept-Encoding")
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

// load reads name, preferring a precompressed sibling the client accepts. It
// returns the bytes to send, the Content-Type of the logical resource, and the
// Content-Encoding (empty when serving the identity representation).
func (h *spaHandler) load(r *http.Request, name string) (data []byte, ctype, enc string, ok bool) {
	ctype = contentTypeFor(name)
	ae := r.Header.Get("Accept-Encoding")
	if strings.Contains(ae, "br") {
		if b, err := fs.ReadFile(h.assets, name+".br"); err == nil {
			return b, ctype, "br", true
		}
	}
	if strings.Contains(ae, "gzip") {
		if b, err := fs.ReadFile(h.assets, name+".gz"); err == nil {
			return b, ctype, "gzip", true
		}
	}
	if b, err := fs.ReadFile(h.assets, name); err == nil {
		return b, ctype, "", true
	}
	return nil, "", "", false
}

// contentTypeFor returns a MIME type for a static asset filename.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".wasm"):
		return "application/wasm"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
