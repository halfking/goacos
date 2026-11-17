package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/halfking/goacos/internal/core"
)

// admitCasdoor is exclusive: no local login, cookie, query-token or admin fallback.
// Keep admission separate from dispatch so the existing storage ownership gates
// still protect configuration requests after their resource policy succeeds.
func (s *Server) admitCasdoor(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if r.Method == http.MethodGet && (r.URL.Path == "/nacos/actuator/health" || r.URL.Path == "/nacos/v1/console/health/readiness" || r.URL.Path == "/nacos/v1/console/health/liveness") {
		return r, true
	}
	deny := func(code int) (*http.Request, bool) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, code, map[string]any{"code": code, "message": http.StatusText(code)})
		return r, false
	}
	if !s.Cfg.AuthEnabled || s.Casdoor == nil {
		return deny(http.StatusServiceUnavailable)
	}
	if len(r.Header.Values("Authorization")) != 1 {
		return deny(http.StatusForbidden)
	}
	h := strings.Fields(r.Header.Get("Authorization"))
	if len(h) != 2 || !strings.EqualFold(h[0], "Bearer") || len(h[1]) > 16384 {
		return deny(http.StatusForbidden)
	}
	resource, action, err := casdoorScope(r)
	if err != nil {
		return deny(http.StatusForbidden)
	}
	claims, err := s.Casdoor.Authorize(r.Context(), h[1], resource, action)
	if errors.Is(err, core.ErrCasdoorDenied) {
		return deny(http.StatusForbidden)
	}
	if err != nil || claims == nil || claims.Username == "" || claims.Admin {
		return deny(http.StatusServiceUnavailable)
	}
	return r.WithContext(withClaims(r.Context(), claims)), true
}
