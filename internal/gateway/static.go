package gateway

// Static delivery for frontend sites.
//
// # Two ways a frontend reaches the gateway
//
// A built single-page application is usually published as files somewhere — an
// image layer, a mounted volume, an object-storage sync — and does not run as
// a registered instance at all. That is protocol=static. The alternative, and
// the one the naming registry already handles, is a real web server registered
// as an instance; that is protocol=http with kind=web. Both are supported, and
// the difference matters only in where the bytes come from.
//
// # Why static wins when both are available
//
// Serving the files directly avoids a network hop and a second point of
// failure for the shell of every page load. But it is only correct if the
// directory actually exists, so the check is a filesystem stat rather than a
// declaration — a static component whose directory is missing falls back to
// proxying instead of 404ing, which is what lets a team switch the two without
// redeclaring the route.
//
// # Why index.html fallback
//
// A single-page application owns its own client-side routes. Without a
// fallback, a user pasting /settings/profile into the address bar gets a 404
// from the file server while the exact same URL works when the SPA is entered
// through the home page. Falling back to index.html for extension-less paths
// reproduces what the browser would have done anyway. It is deliberately NOT
// applied to paths that look like assets (/static/app.js), because serving
// index.html for a missing script produces a confusing MIME-type error instead
// of an honest 404.

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/halfking/goacos/internal/storage"
)

// staticRoot resolves the on-disk directory for one static component.
//
// Directories are named from the component identity so several frontends can
// share one root without colliding, and so removing a component's directory is
// a single, predictable path.
func (g *Gateway) staticRoot(t *Target) string {
	if g.cfg.GatewayStaticRoot == "" || t == nil {
		return ""
	}
	if t.Protocol != storage.ProtocolStatic {
		return ""
	}
	name := sanitizeFilename(t.AppName + "-" + t.ComponentName)
	return filepath.Join(g.cfg.GatewayStaticRoot, name)
}

// servesStatic reports whether this target should be served from disk.
//
// The component's own metadata can force it either way, which is how an
// operator overrides the protocol field without rewriting the route.
func (g *Gateway) servesStatic(t *Target) bool {
	root := g.staticRoot(t)
	if root == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(t.Metadata["delivery"])) {
	case "proxy":
		return false
	case "static":
		return true
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

// ServeStatic delivers a frontend site's files.
//
// It returns false when the request should fall through to proxying instead.
func (g *Gateway) ServeStatic(w http.ResponseWriter, r *http.Request, t *Target) bool {
	root := g.staticRoot(t)
	if root == "" {
		return false
	}
	// The root is recomputed per request rather than captured, so a component
	// redeclared as proxied takes effect on the next request instead of
	// after a restart.
	if !g.servesStatic(t) {
		return false
	}

	upath := path.Clean("/" + r.URL.Path)
	index := filepath.Join(root, "index.html")

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		filePath := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(upath, "/")))
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
			serveStaticFile(w, r, filePath, index)
			return true
		}
		// A directory request looks for its own index; a missing one still
		// deserves the SPA shell rather than a bare 403 from the file server.
		if info, err := os.Stat(filePath); err == nil && info.IsDir() {
			if entry := filepath.Join(filePath, "index.html"); fileExists(entry) {
				serveStaticFile(w, r, entry, index)
				return true
			}
		}
		// The single-page fallback. Only for paths that name a client-side
		// route: serving index.html for a missing script produces a confusing
		// MIME-type error instead of an honest 404.
		if g.cfg.GatewayStaticFallback && !looksLikeAsset(upath) && fileExists(index) {
			serveStaticFile(w, r, index, index)
			return true
		}
	}

	// Static delivery was chosen and there is nothing at this path. Reporting
	// 404 here rather than falling through matters: falling through would send
	// the request to an instance that does not serve the site either, turning
	// a clear "file missing" into a confusing upstream error.
	http.Error(w, "not found", http.StatusNotFound)
	return true
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// serveStaticFile writes one file with cache headers that match its kind.
//
// Fingerprinted build assets are immutable, so they are cached for a year;
// index.html must never be cached, or a deploy is invisible until the browser
// gives up. This is the single most common way a "we deployed it" report turns
// into a support ticket.
func serveStaticFile(w http.ResponseWriter, r *http.Request, filePath, indexPath string) {
	immutable := filePath != indexPath && isFingerprinted(filePath)
	switch {
	case immutable:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case filePath == indexPath:
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("X-Goacos-Static", "true")

	f, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "static file unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeContent(w, r, filepath.Base(filePath), info.ModTime(), f)
}

// looksLikeAsset reports whether a path names a concrete file rather than a
// client-side route.
func looksLikeAsset(p string) bool {
	return path.Ext(p) != ""
}

// isFingerprinted recognises content-hashed bundle names produced by the
// mainstream build tools: app.4f3a2b1c.js (hex, webpack) and
// main-C1x9zPq2.css (base64-ish, Vite/Rollup).
//
// Both shapes end in a separator followed by a run of hash characters. The run
// must contain at least one digit, which is what keeps ordinary names like
// "bootstrap.min" or "vendor-bundle" out: a real hash is not all letters, but
// an English word is. Caching a non-fingerprinted file for a year is the exact
// failure this guards — a deploy that appears to do nothing.
func isFingerprinted(p string) bool {
	name := filepath.Base(p)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if len(stem) < 9 {
		return false
	}
	tail := stem[len(stem)-8:]
	hasDigit := false
	for _, c := range tail {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return hasDigit
}
