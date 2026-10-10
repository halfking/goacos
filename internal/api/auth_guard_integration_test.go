//go:build integration

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

// Reuses the explicit loopback-only fixture. Account/password/token values are
// synthetic, held in memory, and never printed in failure output.
func authGuardFixture(t *testing.T) (*Server, string, string, func(string, string, string, string) int) {
	t.Helper()
	s, ns, _ := freshnessServer(t)
	hub, err := core.NewConfigHub(context.Background(), s.ST)
	if err != nil {
		t.Fatal(err)
	}
	s.Hub = hub
	username := "w038-user-" + storage.NewID()
	hash, err := core.HashPassword(s.Cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ST.CreateUser(context.Background(), username, hash); err != nil {
		t.Fatal(err)
	}
	if err := s.ST.AddRole(context.Background(), username, "ROLE_USER"); err != nil {
		t.Fatal(err)
	}
	token, _, admin, err := s.Auth.Login(context.Background(), username, s.Cfg.AdminPassword)
	if err != nil || admin {
		t.Fatal("synthetic user login failed")
	}
	t.Cleanup(func() {
		_ = s.ST.DeleteUser(context.Background(), username)
		for _, table := range []string{"his_config_info", "config_info", "tenant_info"} {
			_, _ = s.ST.Exec(context.Background(), "DELETE FROM "+table+" WHERE tenant_id=?", ns)
		}
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	request := func(method, path, body, bearer string) int {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	return s, ns, token, request
}

func TestW038NormalUserCannotMutateRegistryConfigurationOrNamespace(t *testing.T) {
	s, ns, token, request := authGuardFixture(t)
	naming := url.Values{"namespaceId": {ns}, "serviceName": {"auth-guard"}, "ip": {"127.0.0.1"}, "port": {"18301"}}
	config := url.Values{"tenant": {ns}, "namespaceId": {ns}, "dataId": {"auth-guard.yaml"}, "group": {"DEFAULT_GROUP"}, "content": {"synthetic: true"}}
	console := url.Values{"namespaceId": {ns}, "namespaceName": {"synthetic namespace"}}
	for _, version := range []string{"v1", "v2"} {
		base := "/nacos/" + version
		routes := []struct{ method, path, body string }{
			{"POST", base + "/ns/instance?" + naming.Encode(), ""},
			{"PUT", base + "/ns/instance?" + naming.Encode(), ""},
			{"DELETE", base + "/ns/instance?" + naming.Encode(), ""},
			{"PUT", base + "/ns/service?" + naming.Encode(), ""},
			{"DELETE", base + "/ns/service?" + naming.Encode(), ""},
		}
		if version == "v1" {
			routes = append(routes, struct{ method, path, body string }{"GET", base + "/ns/instance/beat?" + naming.Encode(), ""}, struct{ method, path, body string }{"PUT", base + "/ns/instance/beat?" + naming.Encode(), ""})
			routes = append(routes, struct{ method, path, body string }{"HEAD", base + "/ns/instance/beat?" + naming.Encode(), ""})
		} else {
			routes = append(routes, struct{ method, path, body string }{"POST", base + "/ns/instance/beat?" + naming.Encode(), ""}, struct{ method, path, body string }{"POST", base + "/ns/service?" + naming.Encode(), ""})
		}
		configPath, namespacePath := base+"/cs/config", base+"/console/namespace"
		if version == "v1" {
			configPath += "s"
			namespacePath += "s"
		}
		routes = append(routes, struct{ method, path, body string }{"POST", configPath, config.Encode()}, struct{ method, path, body string }{"DELETE", configPath + "?" + config.Encode(), ""})
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			routes = append(routes, struct{ method, path, body string }{method, namespacePath + "?" + console.Encode(), ""})
		}
		for _, route := range routes {
			t.Run(version+"-"+route.method+"-"+strings.Split(route.path, "?")[0], func(t *testing.T) {
				if status := request(route.method, route.path, route.body, token); status != http.StatusForbidden {
					t.Errorf("ordinary user mutation returned HTTP %d, want 403", status)
				}
			})
		}
	}
	// All requests were refused before storage. Check the durable history as well
	// as current rows so publish-then-delete cannot conceal a mutation.
	for _, tc := range []struct{ table, column string }{{"naming_instance", "namespace_id"}, {"naming_service", "namespace_id"}, {"config_info", "tenant_id"}, {"his_config_info", "tenant_id"}, {"tenant_info", "tenant_id"}} {
		var count int
		if err := s.ST.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+tc.table+" WHERE "+tc.column+"=?", ns).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("denied requests left %d rows in %s", count, tc.table)
		}
	}
}

func TestW038LiveAccountAndRolesOverrideTokenRoles(t *testing.T) {
	s, ns, token, request := authGuardFixture(t)
	claims, err := s.Auth.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	username := claims.Username
	ctx := context.Background()
	path := "/nacos/v1/ns/instance?" + url.Values{"namespaceId": {ns}, "serviceName": {"role-change"}, "ip": {"127.0.0.1"}, "port": {"18302"}}.Encode()
	if err := s.ST.AddRole(ctx, username, "ROLE_ADMIN"); err != nil {
		t.Fatal(err)
	}
	t.Run("live-grant", func(t *testing.T) {
		if got := request("POST", path, "", token); got != 200 {
			t.Errorf("live role grant returned %d", got)
		}
	})
	adminToken, _, admin, err := s.Auth.Login(ctx, username, s.Cfg.AdminPassword)
	if err != nil || !admin {
		t.Fatal("synthetic admin login failed")
	}
	if err := s.ST.DeleteRole(ctx, username, "ROLE_ADMIN"); err != nil {
		t.Fatal(err)
	}
	t.Run("demoted-token", func(t *testing.T) {
		if got := request("DELETE", path, "", adminToken); got != 403 {
			t.Errorf("demoted token returned %d", got)
		}
	})
	// A signed role label is never enough when the durable user has no grant.
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": username, "roles": []string{"ROLE_ADMIN"}, "iss": "goacos", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}).SignedString([]byte(s.Cfg.TokenSecret))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("token-role-not-authority", func(t *testing.T) {
		if got := request("POST", path, "", forged); got != 403 {
			t.Errorf("ungranted token role returned %d", got)
		}
	})
	if err := s.ST.DeleteRole(ctx, username, "ROLE_USER"); err != nil {
		t.Fatal(err)
	}
	t.Run("enabled-roleless-read-principal", func(t *testing.T) {
		if got := request("GET", "/nacos/v1/console/namespaces", "", token); got != 200 {
			t.Errorf("enabled account without roles returned %d", got)
		}
	})
	if _, err := s.ST.Exec(ctx, "UPDATE users SET enabled=0 WHERE username=?", username); err != nil {
		t.Fatal(err)
	}
	t.Run("disabled-user", func(t *testing.T) {
		if got := request("GET", "/nacos/v1/ns/instance/list?namespaceId="+url.QueryEscape(ns)+"&serviceName=role-change", "", token); got != 403 {
			t.Errorf("disabled user returned %d", got)
		}
	})
	if err := s.ST.DeleteUser(ctx, username); err != nil {
		t.Fatal(err)
	}
	t.Run("deleted-user", func(t *testing.T) {
		if got := request("GET", "/nacos/v1/console/namespaces", "", token); got != 403 {
			t.Errorf("deleted user returned %d", got)
		}
	})
}

func TestW038IssuerContractEnforcedOnAuthenticatedHTTP(t *testing.T) {
	s, ns, _, request := authGuardFixture(t)
	cases := []struct {
		name   string
		method jwt.SigningMethod
		mutate func(jwt.MapClaims)
	}{
		{"missing-issuer", jwt.SigningMethodHS256, func(c jwt.MapClaims) { delete(c, "iss") }},
		{"wrong-issuer", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["iss"] = "another-service" }},
		{"missing-expiry", jwt.SigningMethodHS256, func(c jwt.MapClaims) { delete(c, "exp") }},
		{"expired", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"future-issued-at", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }},
		{"padded-subject", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["sub"] = " " + s.Cfg.AdminUsername + " " }},
		{"HS384", jwt.SigningMethodHS384, func(jwt.MapClaims) {}},
		{"HS512", jwt.SigningMethodHS512, func(jwt.MapClaims) {}},
	}
	path := "/nacos/v1/ns/instance?" + url.Values{"namespaceId": {ns}, "serviceName": {"issuer-guard"}, "ip": {"127.0.0.1"}, "port": {"18304"}}.Encode()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"sub": s.Cfg.AdminUsername, "roles": []string{"ROLE_ADMIN"}, "iss": "goacos", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			tc.mutate(claims)
			token, err := jwt.NewWithClaims(tc.method, claims).SignedString([]byte(s.Cfg.TokenSecret))
			if err != nil {
				t.Fatal(err)
			}
			if got := request("POST", path, "", token); got != 403 {
				t.Errorf("invalid issuer token returned %d", got)
			}
		})
	}
	var count int
	if err := s.ST.QueryRow(context.Background(), "SELECT COUNT(*) FROM naming_instance WHERE namespace_id=?", ns).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid tokens affected storage: count=%d err=%v", count, err)
	}
}

func TestW038ReadListenerAndLiveAdminCompatibility(t *testing.T) {
	s, ns, token, request := authGuardFixture(t)
	for _, path := range []string{"/nacos/v1/cs/configs/listener", "/nacos/v2/cs/config/listener"} {
		t.Run(path, func(t *testing.T) {
			if got := request("POST", path, "", token); got != 200 {
				t.Errorf("read listener returned %d", got)
			}
		})
	}
	adminToken, _, admin, err := s.Auth.Login(context.Background(), s.Cfg.AdminUsername, s.Cfg.AdminPassword)
	if err != nil || !admin {
		t.Fatal("admin login failed")
	}
	params := url.Values{"namespaceId": {ns}, "serviceName": {"admin-allowed"}, "ip": {"127.0.0.1"}, "port": {"18303"}}
	for _, route := range []struct{ method, path string }{{"POST", "/nacos/v1/ns/instance"}, {"GET", "/nacos/v1/ns/instance/beat"}, {"POST", "/nacos/v2/ns/instance/beat"}, {"DELETE", "/nacos/v2/ns/instance"}} {
		t.Run(route.method+route.path, func(t *testing.T) {
			if got := request(route.method, route.path+"?"+params.Encode(), "", adminToken); got != 200 {
				t.Errorf("live admin operation returned %d", got)
			}
		})
	}
	t.Run("unknown-write", func(t *testing.T) {
		if got := request("POST", "/nacos/v3/new-mutating-endpoint", "", token); got != 403 {
			t.Errorf("unknown ordinary-user write returned %d", got)
		}
	})
	t.Run("user-directory", func(t *testing.T) {
		if got := request("GET", "/nacos/v1/auth/users", "", token); got != 403 {
			t.Errorf("user directory returned %d", got)
		}
	})
}
