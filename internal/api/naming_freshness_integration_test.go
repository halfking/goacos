//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

// Integration runs must explicitly select an isolated loopback test database.
// No production environment defaults or credentials are read by this helper.
func freshnessServer(t *testing.T) (*Server, string, func(string, string) []byte) {
	t.Helper()
	dsn := os.Getenv("GOACOS_TEST_PG_DSN")
	u, err := url.Parse(dsn)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.HasPrefix(u.Path, "/w038_") {
		t.Fatal("GOACOS_TEST_PG_DSN must select an explicit isolated loopback w038_ test database")
	}
	if u.User == nil {
		t.Fatal("explicit isolated test account required")
	}
	testPassword, hasPassword := u.User.Password()
	if !hasPassword || testPassword == "" {
		t.Fatal("explicit isolated test password required")
	}
	cfg := &config.Config{
		DBType: "postgres", DBDSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2,
		AdminUsername: "w038-test-admin", AdminPassword: testPassword,
		AuthEnabled: true, TokenSecret: storage.NewID(), TokenTTLSeconds: 60,
		HeartbeatIntervalMs: 5000, HeartbeatTimeoutMs: 60000,
		EphemeralDeleteAfterMs: 120000, SweepInterval: time.Hour,
	}
	st, err := storage.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth := core.NewAuthService(cfg, st)
	// Obtain the real signed admin token from stored password/role verification.
	token, _, admin, err := auth.Login(context.Background(), cfg.AdminUsername, cfg.AdminPassword)
	if err != nil || !admin {
		t.Fatalf("synthetic test admin login failed: %v", err)
	}
	s := &Server{Cfg: cfg, ST: st, Auth: auth, Started: time.Now()}
	ns := "w038-" + storage.NewID()
	t.Cleanup(func() {
		_, _ = st.Exec(context.Background(), "DELETE FROM naming_instance WHERE namespace_id=?", ns)
		_, _ = st.Exec(context.Background(), "DELETE FROM naming_service WHERE namespace_id=?", ns)
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	request := func(method, path string) []byte {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: HTTP %d, read=%v", method, path, resp.StatusCode, err)
		}
		return body
	}
	return s, ns, request
}

func freshnessDecode(t *testing.T, version string, body []byte, result any) {
	t.Helper()
	if version == "v2" {
		var envelope struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != 0 {
			t.Fatalf("invalid v2 envelope: code=%d error=%v", envelope.Code, err)
		}
		body = envelope.Data
	}
	if err := json.Unmarshal(body, result); err != nil {
		t.Fatal(err)
	}
}

func TestW038ReadTimeHealthWithoutSweeper(t *testing.T) {
	s, ns, request := freshnessServer(t)
	ctx := context.Background()
	now := storage.NowMS()
	cases := []struct {
		name      string
		port      uint32
		ephemeral bool
		heartbeat int64
		storedUp  bool
		wantUp    bool
	}{
		{"fresh", 18101, true, now, true, true},
		{"stale", 18102, true, now - 120000, true, false},
		{"missing-heartbeat", 18103, true, 0, true, false},
		{"future-heartbeat", 18104, true, now + 3600000, true, false},
		{"persistent", 18105, false, now - 3600000, true, true},
		{"explicit-unhealthy", 18106, true, now, false, false},
	}
	for _, tc := range cases {
		if _, err := s.ST.RegisterInstance(ctx, storage.InstanceUpsert{
			NamespaceId: ns, GroupName: "DEFAULT_GROUP", ServiceName: "readiness",
			IP: "127.0.0.1", Port: tc.port, Enabled: true, Ephemeral: tc.ephemeral, Weight: 1,
		}); err != nil {
			t.Fatal(err)
		}
		storedHealthy := 0
		if tc.storedUp {
			storedHealthy = 1
		}
		if _, err := s.ST.Exec(ctx, "UPDATE naming_instance SET last_heartbeat_ms=?,healthy=? WHERE namespace_id=? AND port=?",
			tc.heartbeat, storedHealthy, ns, tc.port); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			q := url.Values{"namespaceId": {ns}, "serviceName": {"readiness"}, "ip": {"127.0.0.1"}}
			base := "/nacos/" + version + "/ns/instance"
			for _, healthyOnly := range []bool{false, true} {
				t.Run("list-healthyOnly-"+strconv.FormatBool(healthyOnly), func(t *testing.T) {
					q.Set("healthyOnly", strconv.FormatBool(healthyOnly))
					var list ServiceInfoResp
					freshnessDecode(t, version, request(http.MethodGet, base+"/list?"+q.Encode()), &list)
					seen := map[uint32]bool{}
					for _, host := range list.Hosts {
						seen[host.Port] = host.Healthy
					}
					for _, tc := range cases {
						got, exists := seen[tc.port]
						if healthyOnly && !tc.wantUp {
							if exists {
								t.Errorf("%s appeared in healthyOnly list (healthy=%t)", tc.name, got)
							}
						} else if !exists || got != tc.wantUp {
							t.Errorf("%s list health=%t exists=%t, want %t", tc.name, got, exists, tc.wantUp)
						}
					}
					if want := 6; !healthyOnly && len(list.Hosts) != want {
						t.Errorf("all-host list has %d rows, want %d", len(list.Hosts), want)
					}
					if healthyOnly && len(list.Hosts) != 2 {
						t.Errorf("healthyOnly has %d rows, want fresh+persistent", len(list.Hosts))
					}
				})
			}
			q.Del("healthyOnly")
			for _, tc := range cases {
				t.Run("get-"+tc.name, func(t *testing.T) {
					q.Set("port", strconv.Itoa(int(tc.port)))
					var host InstanceResp
					freshnessDecode(t, version, request(http.MethodGet, base+"?"+q.Encode()), &host)
					if host.Healthy != tc.wantUp {
						t.Errorf("get %s health=%t, want %t", tc.name, host.Healthy, tc.wantUp)
					}
				})
			}
		})
	}
	// Reads must derive health without mutating flags or running a sweeper.
	var healthy int
	if err := s.ST.QueryRow(ctx, "SELECT COUNT(*) FROM naming_instance WHERE namespace_id=? AND healthy=1", ns).Scan(&healthy); err != nil || healthy != 5 {
		t.Fatalf("read changed stored health flags: healthy=%d error=%v", healthy, err)
	}
}

func TestW038HeartbeatRecoversWithoutSweeper(t *testing.T) {
	s, ns, request := freshnessServer(t)
	ctx := context.Background()
	if _, err := s.ST.RegisterInstance(ctx, storage.InstanceUpsert{NamespaceId: ns, GroupName: "DEFAULT_GROUP", ServiceName: "recover", IP: "127.0.0.1", Port: 18201, Enabled: true, Ephemeral: true, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ST.Exec(ctx, "UPDATE naming_instance SET last_heartbeat_ms=? WHERE namespace_id=?", storage.NowMS()-120000, ns); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"namespaceId": {ns}, "serviceName": {"recover"}, "ip": {"127.0.0.1"}, "port": {"18201"}}
	path := "/nacos/v1/ns/instance?" + q.Encode()
	var before InstanceResp
	freshnessDecode(t, "v1", request(http.MethodGet, path), &before)
	if before.Healthy {
		t.Error("expired heartbeat was reported healthy before recovery")
	}
	request(http.MethodPut, "/nacos/v1/ns/instance/beat?"+q.Encode())
	var after InstanceResp
	freshnessDecode(t, "v1", request(http.MethodGet, path), &after)
	if !after.Healthy {
		t.Error("verified heartbeat did not restore effective health")
	}
}
