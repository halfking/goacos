package api

import (
	"embed"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// consoleStatic serves the embedded single-page console under /nacos/.
func (s *Server) consoleStatic(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/nacos/", "/nacos/index.html":
		b, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	case "/nacos/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
