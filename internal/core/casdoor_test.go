package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
)

func TestCasdoorAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		machine, inactive, badAud, forbidden, deny, outage bool
		currentField                                       string
		missingApplication                                 bool
	}{
		{name: "human"}, {name: "machine", machine: true},
		{name: "deleted application", machine: true, missingApplication: true},
		{name: "application organization changed", machine: true, currentField: "organization"},
		{name: "application client ID changed", machine: true, currentField: "clientId"},
		{name: "application owner changed", machine: true, currentField: "owner"},
		{name: "application name changed", machine: true, currentField: "name"},
		{name: "user ID mismatch", currentField: "id"},
		{name: "user name mismatch", currentField: "name"},
		{name: "user owner mismatch", currentField: "owner"}, {name: "inactive", inactive: true}, {name: "wrong audience", badAud: true}, {name: "forbidden", forbidden: true}, {name: "policy deny", deny: true}, {name: "outage", outage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, name, sub, aud, typ := "org", "alice", "user-uuid", "registry", "user"
			if tc.machine {
				owner, name, sub, aud, typ = "admin", "worker", "admin/worker", "worker-client", "application"
			}
			if tc.badAud {
				aud = "other"
			}
			payload, _ := json.Marshal(map[string]string{"owner": owner, "name": name, "sub": sub, "type": typ})
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				id, secret, ok := r.BasicAuth()
				if !ok || id != "registry" || secret != "secret" {
					t.Error("missing service Basic auth")
				}
				if tc.outage {
					w.WriteHeader(500)
					return
				}
				var result any
				switch r.URL.Path {
				case "/api/login/oauth/introspect":
					r.ParseForm()
					if r.Form.Get("token") != token || r.Form.Get("token_type_hint") != "access_token" {
						t.Error("incorrect introspection")
					}
					result = map[string]any{"active": !tc.inactive, "iss": "issuer", "aud": aud, "sub": sub, "username": name, "token_type": "access-token", "exp": time.Now().Unix() + 600}
				case "/api/get-users":
					current := map[string]any{"id": sub, "owner": owner, "name": name, "isForbidden": tc.forbidden, "isDeleted": false}
					if tc.currentField != "" {
						current[tc.currentField] = "other"
					}
					result = map[string]any{"status": "ok", "data": []any{current}}
				case "/api/get-application":
					current := map[string]string{"owner": "admin", "name": "worker", "organization": "org", "clientId": "worker-client"}
					if tc.currentField != "" {
						current[tc.currentField] = "other"
					}
					result = map[string]any{"status": "ok", "data": current}
					if tc.missingApplication {
						result = map[string]any{"status": "ok", "data": nil}
					}
				case "/api/enforce":
					if tc.currentField != "" || tc.missingApplication {
						t.Error("invalid principal reached policy enforcement")
					}
					var args []string
					json.NewDecoder(r.Body).Decode(&args)
					if len(args) != 3 || args[0] != owner+"/"+name || args[1] != "goacos/config/t/g/d" || args[2] != "read" || r.URL.Query().Get("permissionId") != "org/registry" {
						t.Error("incorrect permission tuple")
					}
					result = map[string]any{"status": "ok", "data": []bool{!tc.deny}}
				default:
					t.Error("unexpected endpoint")
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer srv.Close()
			a, err := NewCasdoorAuthorizer(config.CasdoorConfig{Endpoint: srv.URL, AllowHTTP: true, Issuer: "issuer", ClientID: "registry", ClientSecret: "secret", Organization: "org", Permission: "org/registry", Applications: map[string]string{"worker": "worker-client"}})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := a.Authorize(context.Background(), token, "goacos/config/t/g/d", "read")
			if tc.outage {
				if !errors.Is(err, ErrCasdoorUnavailable) {
					t.Fatal(err)
				}
				return
			}
			if tc.inactive || tc.badAud || tc.forbidden || tc.deny || tc.currentField != "" || tc.missingApplication {
				if claims != nil || !errors.Is(err, ErrCasdoorDenied) {
					t.Fatalf("expected denied: %v", err)
				}
				return
			}
			if err != nil || claims == nil || claims.Admin || calls != 3 {
				t.Fatalf("invalid success: %+v %v calls=%d", claims, err, calls)
			}
		})
	}
}
func TestCasdoorLocalTokenGuard(t *testing.T) {
	a := NewAuthService(&config.Config{AuthProvider: "casdoor"}, nil)
	if _, _, _, err := a.Login(context.Background(), "alice", "password"); !errors.Is(err, ErrCasdoorDenied) {
		t.Fatal(err)
	}
	if _, err := a.Parse("token"); !errors.Is(err, ErrCasdoorDenied) {
		t.Fatal(err)
	}
}

func TestCasdoorFailClosedResponses(t *testing.T) {
	cases := []struct {
		name, stage string
		response    any
		status      int
	}{
		{name: "expired", stage: "introspect", response: map[string]any{"exp": 1}},
		{name: "future nbf", stage: "introspect", response: map[string]any{"nbf": time.Now().Unix() + 3600}},
		{name: "future iat", stage: "introspect", response: map[string]any{"iat": time.Now().Unix() + 3600}},
		{name: "issuer", stage: "introspect", response: map[string]any{"iss": "other"}},
		{name: "refresh", stage: "introspect", response: map[string]any{"token_type": "refresh-token"}},
		{name: "subject", stage: "introspect", response: map[string]any{"sub": "other"}},
		{name: "username", stage: "introspect", response: map[string]any{"username": "other"}},
		{name: "aud malformed", stage: "introspect", response: map[string]any{"aud": []any{"registry", 7}}},
		{name: "missing flags", stage: "users", response: map[string]any{"status": "ok", "data": []any{map[string]any{"id": "uuid", "owner": "org", "name": "alice"}}}},
		{name: "missing principal", stage: "users", response: map[string]any{"status": "ok", "data": []any{}}},
		{name: "extra decisions", stage: "enforce", response: map[string]any{"status": "ok", "data": []bool{true, true}}},
		{name: "missing decision", stage: "enforce", response: map[string]any{"status": "ok"}},
		{name: "redirect", stage: "introspect", status: 302},
		{name: "oversize", stage: "introspect", response: strings.Repeat("x", 65537)},
		{name: "malformed", stage: "introspect", response: "{"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"owner":"org","name":"alice","sub":"uuid","type":"user"}`)) + ".c2ln"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stage := "introspect"
				var result any = map[string]any{"active": true, "iss": "issuer", "aud": "registry", "sub": "uuid", "username": "alice", "token_type": "access-token", "exp": time.Now().Unix() + 600}
				if r.URL.Path == "/api/get-users" {
					stage = "users"
					result = map[string]any{"status": "ok", "data": []any{map[string]any{"id": "uuid", "owner": "org", "name": "alice", "isForbidden": false, "isDeleted": false}}}
				}
				if r.URL.Path == "/api/enforce" {
					stage = "enforce"
					result = map[string]any{"status": "ok", "data": []bool{true}}
				}
				if stage == tc.stage {
					if tc.status != 0 {
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(tc.status)
						return
					}
					if raw, ok := tc.response.(string); ok {
						w.Write([]byte(raw))
						return
					}
					if stage == "introspect" {
						for k, v := range tc.response.(map[string]any) {
							result.(map[string]any)[k] = v
						}
					} else {
						result = tc.response
					}
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer srv.Close()
			a, err := NewCasdoorAuthorizer(config.CasdoorConfig{Endpoint: srv.URL, AllowHTTP: true, Issuer: "issuer", ClientID: "registry", ClientSecret: "secret", Organization: "org", Permission: "org/p"})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := a.Authorize(context.Background(), token, "goacos/config/t/g/d", "read")
			if err == nil || claims != nil {
				t.Fatalf("accepted invalid response: %+v", claims)
			}
			if err != ErrCasdoorDenied && err != ErrCasdoorUnavailable {
				t.Fatalf("unsanitized error: %v", err)
			}
		})
	}
}

func TestCasdoorModelAuthorization(t *testing.T) {
	for _, tc := range []struct {
		decisions  []bool
		groups     []string
		validGroup bool
		configured string
	}{
		{[]bool{true}, []string{"registry-model/"}, true, ""},
		{[]bool{false}, []string{"registry-model/"}, true, ""},
		{[]bool{true, true}, []string{"registry-model/"}, true, ""},
		{[]bool{true, false}, []string{"registry-model/"}, true, ""},
		{[]bool{}, []string{"registry-model/"}, true, ""},
		{[]bool{true}, nil, false, ""},
		{[]bool{true}, []string{"other/"}, false, ""},
		{[]bool{true}, []string{"registry-model/external"}, false, ""},
		{[]bool{true}, []string{"registry-model/", "registry-model/"}, false, ""},
		{[]bool{true}, []string{"org/registry-model/"}, false, ""},
		{[]bool{true}, []string{"org/registry-model/"}, true, "org/registry-model/"},
		{[]bool{false}, []string{"org/registry-model/"}, true, "org/registry-model/"},
		{[]bool{true}, []string{"registry-model/"}, false, "org/registry-model/"},
		{[]bool{true}, []string{"other/registry-model/"}, false, "org/registry-model/"},
		{[]bool{true}, []string{"org/other/"}, false, "org/registry-model/"},
		{[]bool{true}, []string{"org/registry-model/external"}, false, "org/registry-model/"},
		{[]bool{true}, []string{"registry-model/"}, true, "registry-model/"},
		{[]bool{true}, []string{"org/registry-model/"}, false, "registry-model/"},
	} {
		decisions := tc.decisions
		t.Run(fmt.Sprint(decisions, tc.groups, tc.configured), func(t *testing.T) {
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"owner":"org","name":"alice","sub":"uuid","type":"user"}`)) + ".c2ln"
			enforced := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var result any
				switch r.URL.Path {
				case "/api/login/oauth/introspect":
					result = map[string]any{"active": true, "iss": "issuer", "aud": "registry", "sub": "uuid", "username": "alice", "token_type": "access-token", "exp": time.Now().Unix() + 600}
				case "/api/get-users":
					result = map[string]any{"status": "ok", "data": []any{map[string]any{"id": "uuid", "owner": "org", "name": "alice", "isForbidden": false, "isDeleted": false}}}
				case "/api/enforce":
					enforced = true
					query := r.URL.Query()
					if len(query) != 1 || len(query["modelId"]) != 1 || query.Get("modelId") != "org/registry-model" {
						t.Errorf("unexpected enforce selector: %v", query)
					}
					var tuple []string
					if json.NewDecoder(r.Body).Decode(&tuple) != nil || !reflect.DeepEqual(tuple, []string{"org/alice", "goacos/config/t/g/d", "write"}) {
						t.Errorf("unexpected enforcement tuple: %v", tuple)
					}
					result = map[string]any{"status": "ok", "data": decisions, "data2": tc.groups}
					if tc.groups == nil {
						delete(result.(map[string]any), "data2")
					}
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer srv.Close()
			a, err := NewCasdoorAuthorizer(config.CasdoorConfig{Endpoint: srv.URL, AllowHTTP: true, Issuer: "issuer", ClientID: "registry", ClientSecret: "secret", Organization: "org", Model: "org/registry-model", ModelResponseGroup: tc.configured})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := a.Authorize(context.Background(), token, "goacos/config/t/g/d", "write")
			if !enforced {
				t.Fatal("enforcement was not reached")
			}
			switch {
			case len(decisions) != 1 || !tc.validGroup:
				if claims != nil || err != ErrCasdoorUnavailable {
					t.Fatalf("accepted invalid decision cardinality: %v %v", claims, err)
				}
			case !decisions[0]:
				if claims != nil || err != ErrCasdoorDenied {
					t.Fatalf("accepted denied decision: %v %v", claims, err)
				}
			default:
				if claims == nil || claims.Admin || err != nil {
					t.Fatalf("unexpected rejection: %v %v", claims, err)
				}
			}
		})
	}
}
