package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/core"
)

// Handler builds the full HTTP handler with routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- config center v1 ----
	mux.HandleFunc("GET /nacos/v1/cs/configs", s.configV1Get)
	mux.HandleFunc("POST /nacos/v1/cs/configs", s.configV1Publish)
	mux.HandleFunc("DELETE /nacos/v1/cs/configs", s.configV1Delete)
	mux.HandleFunc("POST /nacos/v1/cs/configs/listener", s.configV1Listener)
	mux.HandleFunc("GET /nacos/v1/cs/history", s.configV1HistoryList)
	mux.HandleFunc("GET /nacos/v1/cs/history/configId", s.configV1HistoryDetail)
	mux.HandleFunc("GET /nacos/v1/cs/history/configs", s.configV1HistoryPrevious)

	// ---- config center v2 ----
	mux.HandleFunc("GET /nacos/v2/cs/config", s.configV2Get)
	mux.HandleFunc("POST /nacos/v2/cs/config", s.configV2Publish)
	mux.HandleFunc("DELETE /nacos/v2/cs/config", s.configV2Delete)
	mux.HandleFunc("GET /nacos/v2/cs/config/list", s.configV2List)
	mux.HandleFunc("POST /nacos/v2/cs/config/listener", s.configV2Listener)
	mux.HandleFunc("GET /nacos/v2/cs/history/list", s.configV2HistoryList)
	mux.HandleFunc("GET /nacos/v2/cs/history", s.configV2HistoryDetail)

	// ---- naming v1 ----
	mux.HandleFunc("POST /nacos/v1/ns/instance", s.namingV1Register)
	mux.HandleFunc("PUT /nacos/v1/ns/instance", s.namingV1Register)
	mux.HandleFunc("DELETE /nacos/v1/ns/instance", s.namingV1Deregister)
	mux.HandleFunc("GET /nacos/v1/ns/instance", s.namingV1GetInstance)
	mux.HandleFunc("GET /nacos/v1/ns/instance/list", s.namingV1ListInstances)
	mux.HandleFunc("GET /nacos/v1/ns/instance/beat", s.namingV1Beat)
	mux.HandleFunc("PUT /nacos/v1/ns/instance/beat", s.namingV1Beat)
	mux.HandleFunc("GET /nacos/v1/ns/service/list", s.namingV1ListServices)
	mux.HandleFunc("GET /nacos/v1/ns/service", s.namingV1ServiceDetail)
	mux.HandleFunc("PUT /nacos/v1/ns/service", s.namingV1UpdateService)
	mux.HandleFunc("DELETE /nacos/v1/ns/service", s.namingV1DeleteService)
	mux.HandleFunc("GET /nacos/v1/ns/operator/metrics", s.namingV1Metrics)

	// ---- naming v2 ----
	mux.HandleFunc("POST /nacos/v2/ns/instance", s.namingV2Register)
	mux.HandleFunc("PUT /nacos/v2/ns/instance", s.namingV2Register)
	mux.HandleFunc("DELETE /nacos/v2/ns/instance", s.namingV2Deregister)
	mux.HandleFunc("GET /nacos/v2/ns/instance", s.namingV2GetInstance)
	mux.HandleFunc("GET /nacos/v2/ns/instance/list", s.namingV2ListInstances)
	mux.HandleFunc("POST /nacos/v2/ns/instance/beat", s.namingV2Beat)
	mux.HandleFunc("GET /nacos/v2/ns/service/list", s.namingV2ListServices)
	mux.HandleFunc("GET /nacos/v2/ns/service", s.namingV2ServiceDetail)
	mux.HandleFunc("POST /nacos/v2/ns/service", s.namingV2CreateService)
	mux.HandleFunc("PUT /nacos/v2/ns/service", s.namingV2UpdateService)
	mux.HandleFunc("DELETE /nacos/v2/ns/service", s.namingV2DeleteService)

	// ---- auth ----
	mux.HandleFunc("POST /nacos/v1/auth/users/login", s.authLogin)
	mux.HandleFunc("GET /nacos/v1/auth/users/login", s.authLogin)
	mux.HandleFunc("GET /nacos/v1/auth/users", s.authListUsers)
	mux.HandleFunc("POST /nacos/v1/auth/users", s.authCreateUser)
	mux.HandleFunc("PUT /nacos/v1/auth/users", s.authUpdateUser)
	mux.HandleFunc("DELETE /nacos/v1/auth/users", s.authDeleteUser)
	mux.HandleFunc("GET /nacos/v1/auth/roles", s.authListRoles)
	mux.HandleFunc("POST /nacos/v1/auth/roles", s.authAddRole)
	mux.HandleFunc("DELETE /nacos/v1/auth/roles", s.authDeleteRole)
	mux.HandleFunc("POST /nacos/v2/auth/user/login", s.authLoginV2)

	// ---- console / ops ----
	mux.HandleFunc("GET /nacos/v1/console/namespaces", s.namespaceListV1)
	mux.HandleFunc("POST /nacos/v1/console/namespaces", s.namespaceCreateV1)
	mux.HandleFunc("PUT /nacos/v1/console/namespaces", s.namespaceUpdateV1)
	mux.HandleFunc("DELETE /nacos/v1/console/namespaces", s.namespaceDeleteV1)
	mux.HandleFunc("GET /nacos/v2/console/namespace/list", s.namespaceListV2)
	mux.HandleFunc("POST /nacos/v2/console/namespace", s.namespaceCreateV2)
	mux.HandleFunc("PUT /nacos/v2/console/namespace", s.namespaceUpdateV2)
	mux.HandleFunc("DELETE /nacos/v2/console/namespace", s.namespaceDeleteV2)
	mux.HandleFunc("GET /nacos/v1/console/server/state", s.serverState)
	mux.HandleFunc("GET /nacos/v1/console/health/readiness", s.healthReadiness)
	mux.HandleFunc("GET /nacos/v1/console/health/liveness", s.healthLiveness)
	mux.HandleFunc("GET /nacos/actuator/health", s.actuatorHealth)

	// ---- console static (embedded single page) ----
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/nacos/index.html", http.StatusFound)
	})
	mux.HandleFunc("GET /nacos", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/nacos/index.html", http.StatusFound)
	})
	mux.HandleFunc("GET /nacos/", s.consoleStatic)

	return s.middleware(mux)
}

// openEndpoints lists path prefixes/methods that bypass auth.
func isOpenPath(method, path string) bool {
	if path == "/" || path == "/nacos" || path == "/nacos/index.html" || path == "/nacos/favicon.ico" {
		return true
	}
	if strings.HasPrefix(path, "/nacos/actuator/") {
		return true
	}
	if strings.HasPrefix(path, "/nacos/v1/console/health/") {
		return true
	}
	if path == "/nacos/v1/console/server/state" {
		return true
	}
	if path == "/nacos/v1/auth/users/login" || path == "/nacos/v2/auth/user/login" {
		return method == http.MethodPost || method == http.MethodGet
	}
	return false
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[goacos] panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSON(w, http.StatusInternalServerError, V1Result{Code: 500, Message: "internal error"})
			}
		}()
		if s.Cfg.AuthEnabled && !isOpenPath(r.Method, r.URL.Path) {
			claims, err := s.tokenFromRequest(r)
			if err != nil {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"timestamp": time.Now().Format(time.RFC3339),
					"status":    403,
					"error":     "Unauthorized",
					"message":   "invalid token",
					"path":      r.URL.Path,
				})
				return
			}
			// admin-only management endpoints
			if isAdminOnly(r.URL.Path, r.Method) && !claims.Admin {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"timestamp": time.Now().Format(time.RFC3339),
					"status":    403,
					"error":     "Forbidden",
					"message":   "admin permission required",
					"path":      r.URL.Path,
				})
				return
			}
			r = r.WithContext(withClaims(r.Context(), claims))
		}
		next.ServeHTTP(w, r)
		log.Printf("[goacos] %s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func isAdminOnly(path, method string) bool {
	if strings.HasPrefix(path, "/nacos/v1/auth/users") && method != http.MethodGet {
		return true
	}
	if strings.HasPrefix(path, "/nacos/v1/auth/roles") {
		return true
	}
	return false
}

type claimsKeyType struct{}

func withClaims(ctx context.Context, c *core.Claims) context.Context {
	return context.WithValue(ctx, claimsKeyType{}, c)
}

// ClaimsFrom returns the authenticated claims (nil when auth disabled).
func ClaimsFrom(ctx context.Context) *core.Claims {
	c, _ := ctx.Value(claimsKeyType{}).(*core.Claims)
	return c
}

// tokenFromRequest extracts and validates the access token from query param,
// cookie or Authorization header (Nacos-compatible order).
func (s *Server) tokenFromRequest(r *http.Request) (*core.Claims, error) {
	tok := r.URL.Query().Get("accessToken")
	if tok == "" {
		if ck, err := r.Cookie("accessToken"); err == nil {
			tok = ck.Value
		}
	}
	if tok == "" {
		h := r.Header.Get("Authorization")
		h = strings.TrimPrefix(h, "Bearer ")
		if h != "" {
			tok = strings.TrimSpace(h)
		}
	}
	if tok == "" {
		return nil, errUnauthorized
	}
	return s.Auth.Parse(tok)
}

var errUnauthorized = errors.New("unauthorized")
