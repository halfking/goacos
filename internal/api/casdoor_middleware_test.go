package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
)

type admissionAuthorizer struct {
	calls                   int
	token, resource, action string
	err                     error
}

func (a *admissionAuthorizer) Authorize(_ context.Context, token, resource, action string) (*core.Claims, error) {
	a.calls++
	a.token, a.resource, a.action = token, resource, action
	if a.err != nil {
		return nil, a.err
	}
	return &core.Claims{Username: "org/reader"}, nil
}
func TestCasdoorMiddlewareAdmission(t *testing.T) {
	const path = "/nacos/v1/ns/instance/list?namespaceId=lab&groupName=GROUP&serviceName=ask"
	cases := []struct {
		name, path, header string
		authErr            error
		want, calls        int
	}{
		{"allow", path, "Bearer opaque", nil, 204, 1},
		{"missing", path, "", nil, 403, 0},
		{"query-only", path + "&accessToken=opaque", "", nil, 403, 0},
		{"denied", path, "Bearer opaque", core.ErrCasdoorDenied, 403, 1},
		{"unavailable", path, "Bearer opaque", core.ErrCasdoorUnavailable, 503, 1},
		{"unexpected-error", path, "Bearer opaque", errors.New("PRIVATE upstream error"), 503, 1},
		{"local-login", "/nacos/v1/auth/users/login", "Bearer opaque", nil, 403, 0},
		{"management", "/nacos/v1/auth/users", "Bearer opaque", nil, 403, 0},
		{"broad-list", "/nacos/v1/ns/service/list?namespaceId=lab", "Bearer opaque", nil, 403, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &admissionAuthorizer{err: tc.authErr}
			s := &Server{Cfg: &config.Config{AuthEnabled: true, AuthProvider: "casdoor"}, Casdoor: a}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c := ClaimsFrom(r.Context())
				if c == nil || c.Username != "org/reader" || c.Admin {
					t.Error("scoped principal lost or global admin granted")
				}
				w.WriteHeader(204)
			})
			r := httptest.NewRequest("GET", tc.path, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			s.middleware(next).ServeHTTP(w, r)
			if w.Code != tc.want || a.calls != tc.calls {
				t.Fatalf("status/calls %d/%d want %d/%d", w.Code, a.calls, tc.want, tc.calls)
			}
			if tc.want == 204 && (a.resource != "goacos/service/lab/GROUP/ask" || a.action != "read" || a.token != "opaque") {
				t.Fatal("wrong authorization tuple")
			}
		})
	}
}
func TestCasdoorMiddlewareNoDisabledOrMissingAuthorizerFallback(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		s := &Server{Cfg: &config.Config{AuthEnabled: enabled, AuthProvider: "casdoor"}}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("bypassed missing Casdoor") })
		r := httptest.NewRequest("GET", "/nacos/v1/ns/instance/list?namespaceId=lab&groupName=G&serviceName=a", nil)
		r.Header.Set("Authorization", "Bearer opaque")
		w := httptest.NewRecorder()
		s.middleware(next).ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatalf("got %d", w.Code)
		}
	}
}
func TestCasdoorMiddlewareHealthAndDuplicateAuthorization(t *testing.T) {
	a := &admissionAuthorizer{}
	s := &Server{Cfg: &config.Config{AuthEnabled: true, AuthProvider: "casdoor"}, Casdoor: a}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, path := range []string{"/nacos/actuator/health", "/nacos/v1/console/health/readiness", "/nacos/v1/console/health/liveness"} {
		w := httptest.NewRecorder()
		s.middleware(next).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/nacos/v1/ns/instance/list?namespaceId=lab&groupName=G&serviceName=a", nil)
	r.Header.Add("Authorization", "Bearer first")
	r.Header.Add("Authorization", "Bearer second")
	w := httptest.NewRecorder()
	s.middleware(next).ServeHTTP(w, r)
	if w.Code != 403 || a.calls != 0 {
		t.Fatal("ambiguous credentials admitted")
	}
}
