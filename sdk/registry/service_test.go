package registry

// Tests for the shared registration policy in service.go.
//
// The property under test throughout is not "does it work" but "does it refuse
// to work in the ways that look like working". Every skip path here produces a
// process that boots, serves traffic, and is correctly absent from the
// registry — a healthy-looking outcome that is only correct for the right
// reason. A test that only asserted the success path would pass against an
// implementation that broadcasts a wrong port whenever anything went wrong.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// env builds a getenv over a map, so no test mutates the process environment.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestConfigFromEnvIsOffByDefault(t *testing.T) {
	// A service that was never asked to register must not register, and must
	// not so much as probe for an address it is not going to publish.
	probed := false
	restore := swapDetect(func(getenv func(string) string) string {
		probed = true
		return "10.0.0.9"
	})
	defer restore()

	cfg, err := ConfigFromEnv(env(nil), Service{Name: "acc-go", ListenAddr: ":4101"})
	if err != nil {
		t.Fatalf("空环境不该是错误：%v", err)
	}
	if cfg.Enabled {
		t.Error("未设 "+EnvEnabled+" 却默认开启")
	}
	if probed {
		t.Error("注册关闭时仍去探测通告地址 —— 每次启动都做一件用不上的事")
	}
	if cfg.IP != "" {
		t.Errorf("注册关闭时不该有通告地址，实际 %q", cfg.IP)
	}
}

func TestConfigFromEnvReadsTheFleetVariableNames(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  "http://goacos:8848",
		EnvNamespace:   "kxpms",
		EnvServiceName: "acc-go-prod",
		EnvIP:          "100.106.177.3",
		EnvPort:        "4101",
		EnvUsername:    "kx",
		EnvPassword:    "secret",
	}), Service{Name: "fallback-name", ListenAddr: ":9999"})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if !cfg.Enabled || cfg.ServerAddr != "http://goacos:8848" || cfg.NamespaceID != "kxpms" {
		t.Errorf("环境没读进来：%+v", cfg)
	}
	if cfg.ServiceName != "acc-go-prod" {
		t.Errorf("GOACOS_SERVICE_NAME 应压过服务自身名，实际 %q", cfg.ServiceName)
	}
	if cfg.Port != 4101 {
		t.Errorf("端口 = %d, want 4101", cfg.Port)
	}
}

func TestConfigFromEnvFallsBackToTheServiceName(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{EnvEnabled: "true"}),
		Service{Name: "memora"})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if cfg.ServiceName != "memora" {
		t.Errorf("服务名回落失败，实际 %q", cfg.ServiceName)
	}
}

func TestConfigFromEnvReportsOnlyWhatTheOperatorTyped(t *testing.T) {
	// These two are typos, and the service should be able to decide for itself
	// whether a typo is fatal. Everything downstream (no address, no port,
	// unreachable registry) is deliberately not an error here.
	for _, tc := range []struct{ name, key, val string }{
		{"端口不是数字", EnvPort, "eighty-eighty"},
		{"开关不是布尔", EnvEnabled, "maybe"},
		{"loopback 开关不是布尔", EnvAllowLoopback, "sure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ConfigFromEnv(env(map[string]string{tc.key: tc.val}), Service{Name: "s"}); err == nil {
				t.Fatalf("%s=%q 竟然没报错 —— 打错的配置必须在启动时说出来", tc.key, tc.val)
			}
		})
	}
}

func TestExplicitLoopbackIsUsedAsGiven(t *testing.T) {
	// An address the operator wrote by name is used as written. They may know
	// something the filter does not — and this is the documented escape hatch
	// for a genuinely single-host deployment.
	cfg, err := ConfigFromEnv(env(map[string]string{
		EnvEnabled: "true", EnvIP: "127.0.0.1",
	}), Service{Name: "s"})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if cfg.IP != "127.0.0.1" {
		t.Errorf("显式写的 loopback 被改动了，实际 %q", cfg.IP)
	}
}

func TestDetectedLoopbackIsRefusedUnlessAllowed(t *testing.T) {
	restore := swapDetect(func(func(string) string) string { return "127.0.0.1" })
	defer restore()

	// Off by default. Publishing this means every peer resolves it to itself:
	// healthy in the registry, unroutable everywhere else.
	cfg, err := ConfigFromEnv(env(map[string]string{EnvEnabled: "true"}), Service{Name: "s"})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if cfg.IP != "" {
		t.Fatalf("默认竟然放行了探测到的 loopback：%q", cfg.IP)
	}

	// The opt-in is the single-host case, and it is honoured.
	cfg, err = ConfigFromEnv(
		env(map[string]string{EnvEnabled: "true", EnvAllowLoopback: "true"}),
		Service{Name: "s"})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if cfg.IP != "127.0.0.1" {
		t.Errorf("显式放行后仍是 %q, want 127.0.0.1", cfg.IP)
	}

	// And the codebase-level opt-in works without the variable, which is the
	// point of having it on Service at all.
	cfg, err = ConfigFromEnv(env(map[string]string{EnvEnabled: "true"}),
		Service{Name: "s", AllowLoopback: true})
	if err != nil {
		t.Fatalf("合法环境被拒：%v", err)
	}
	if cfg.IP != "127.0.0.1" {
		t.Errorf("Service.AllowLoopback 未生效：%q", cfg.IP)
	}
}

func TestDetectedAddressIsFilteredNotPassedThrough(t *testing.T) {
	for _, detected := range []string{"0.0.0.0", "::1", "not-an-ip", ""} {
		t.Run("拒绝_"+detected, func(t *testing.T) {
			restore := swapDetect(func(func(string) string) string { return detected })
			defer restore()
			// Even with the single-host opt-in on: garbage and the unspecified
			// address are not "an address that is fine because you are alone".
			cfg, err := ConfigFromEnv(
				env(map[string]string{EnvEnabled: "true", EnvAllowLoopback: "true"}),
				Service{Name: "s"})
			if err != nil {
				t.Fatalf("合法环境被拒：%v", err)
			}
			if detected == "::1" {
				// ::1 is loopback, so the opt-in legitimately admits it.
				if cfg.IP != "::1" {
					t.Fatalf("::1 在显式放行下应被接受，实际 %q", cfg.IP)
				}
				return
			}
			if cfg.IP != "" {
				t.Fatalf("放行了 %q —— 它不是「只在本机有意义」的合法地址", detected)
			}
		})
	}
}

func TestAdvertisedPortNeverGuesses(t *testing.T) {
	tests := []struct {
		name      string
		explicit  int
		listen    string
		want      int
	}{
		{"显式端口优先", 4101, ":9999", 4101},
		{"从监听地址取", 0, "0.0.0.0:8080", 8080},
		{"裸端口号", 0, "8080", 8080},
		{"带空白", 0, " :27094 ", 27094},
		{"越界显式端口不算数", 70000, ":8080", 8080},
		{"什么都没有就拒绝", 0, "", 0},
		{"监听地址不可解析", 0, "not-an-address", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := advertisedPort(tc.explicit, tc.listen); got != tc.want {
				t.Errorf("advertisedPort(%d, %q) = %d, want %d", tc.explicit, tc.listen, got, tc.want)
			}
		})
	}
}

// TestAnnounceRefusesToPublishSomethingWrong is the one that matters: each skip
// must leave a process that serves traffic and is correctly absent from the
// registry, for the stated reason.
func TestAnnounceRefusesToPublishSomethingWrong(t *testing.T) {
	// Any accidental registration attempt shows up as a request to this server.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeEnvelope(t, w, nil)
	}))
	defer srv.Close()

	base := func() Config {
		return Config{
			Enabled: true, ServerAddr: srv.URL, ServiceName: "s",
			IP: "10.0.0.5", Port: 8080,
		}
	}

	tests := []struct {
		name    string
		cfg     Config
		listen  string
		wantHit bool
	}{
		{"注册关闭", func() Config { c := base(); c.Enabled = false; return c }(), ":8080", false},
		{"没有服务名", func() Config { c := base(); c.ServiceName = ""; return c }(), ":8080", false},
		{"没有可拨地址", func() Config { c := base(); c.IP = ""; return c }(), ":8080", false},
		{"地址不是 IP", func() Config { c := base(); c.IP = "localhost"; return c }(), ":8080", false},
		{"定不出端口", func() Config { c := base(); c.Port = 0; return c }(), "", false},
		{"注册成功（对照）", base(), "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits = 0
			stop := Announce(context.Background(), tc.cfg, tc.listen, testLogger())
			if stop == nil {
				t.Fatal("stop 恒为非 nil，main() 不该被迫判空")
			}
			stop() // must be safe to call even when nothing was registered
			if tc.wantHit {
				if hits == 0 {
					t.Error("对照用例本该注册成功，却一次请求都没打 —— 说明这条判据测不到东西")
				}
				return
			}
			if hits != 0 {
				t.Errorf("跳过的路径仍然打了 %d 次注册请求 —— 它会在注册中心里留下一条不该有的记录", hits)
			}
		})
	}
}

// TestAnnounceDeregistersUnderACancelledContext is the invariant that a manual
// deregister call silently breaks: signal.NotifyContext cancels ctx the moment
// shutdown begins, so the stop function handed to main() must not depend on it.
func TestAnnounceDeregistersUnderACancelledContext(t *testing.T) {
	var mu sync.Mutex
	var deleted int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted++
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/beat") {
			writeEnvelope(t, w, map[string]any{"clientBeatInterval": 5000})
			return
		}
		writeEnvelope(t, w, nil)
	}))
	defer srv.Close()

	// This is exactly what main() hands to Announce.
	ctx, cancel := context.WithCancel(context.Background())

	cfg := Config{
		Enabled: true, ServerAddr: srv.URL, ServiceName: "acc-go",
		IP: "10.0.0.5", Port: 4101, RequestTimeout: 2 * time.Second,
	}
	stop := Announce(ctx, cfg, ":4101", testLogger())

	cancel() // SIGTERM arrives: ctx is dead before main() calls stop()

	stop()

	mu.Lock()
	got := deleted
	mu.Unlock()
	if got == 0 {
		t.Fatal("停机没有发出注销 —— 服务会作为幽灵实例留在注册表里直到被淘汰")
	}
}

// TestAnnounceStopIsIdempotent pins the observable invariant: however many
// times main() calls stop(), exactly one deregistration reaches the registry.
// Two deregistrations look harmless but the second races the first.
//
// It deliberately asserts the outcome rather than the mechanism. Removing this
// package's own sync.Once was tried as a mutation and **survived**, because
// Client.Stop carries its own `stopped` guard — the invariant is upheld by two
// layers, so no test here can discriminate between them. That is worth knowing
// rather than papering over: this judgment is load-bearing on the SDK's
// idempotency, and would stop being load-bearing the day that guard is removed.
// The one way to tell the layers apart would be to count attempts rather than
// requests, and nothing outside this package can observe those.
func TestAnnounceStopIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	var deleted int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted++
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/beat") {
			writeEnvelope(t, w, map[string]any{"clientBeatInterval": 5000})
			return
		}
		writeEnvelope(t, w, nil)
	}))
	defer srv.Close()

	cfg := Config{
		Enabled: true, ServerAddr: srv.URL, ServiceName: "acc-go",
		IP: "10.0.0.5", Port: 4101, RequestTimeout: 2 * time.Second,
	}
	stop := Announce(context.Background(), cfg, ":4101", testLogger())
	stop()
	stop()
	stop()

	mu.Lock()
	got := deleted
	mu.Unlock()
	if got != 1 {
		t.Fatalf("注销发出了 %d 次, want 1 —— 重复注销会与仍在跑的心跳赛跑", got)
	}
}

// swapDetect replaces the address-detection seam for the duration of a test.
func swapDetect(fn func(func(string) string) string) func() {
	prev := detectedAdvertiseIP
	detectedAdvertiseIP = fn
	return func() { detectedAdvertiseIP = prev }
}

// Guard against the JSON decoder in this file silently drifting from the wire
// shape: if registerRequest loses a field, the tests above would still pass
// because they only count requests.
func TestAnnouncePublishesExactlyTheResolvedIdentity(t *testing.T) {
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, "/beat") {
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
		if strings.HasSuffix(r.URL.Path, "/beat") {
			writeEnvelope(t, w, map[string]any{"clientBeatInterval": 5000})
			return
		}
		writeEnvelope(t, w, nil)
	}))
	defer srv.Close()

	cfg := Config{
		Enabled: true, ServerAddr: srv.URL, ServiceName: "memora",
		NamespaceID: "itestu", IP: "100.106.177.3", RequestTimeout: 2 * time.Second,
	}
	stop := Announce(context.Background(), cfg, ":8091", testLogger())
	defer stop()

	if got["serviceName"] != "memora" {
		t.Errorf("上线报文里服务名 = %v, want memora", got["serviceName"])
	}
	if got["ip"] != "100.106.177.3" {
		t.Errorf("上线报文里地址 = %v, want 100.106.177.3", got["ip"])
	}
	// The port came from the listen address, not from a default.
	if got["port"] != float64(8091) {
		t.Errorf("上线报文里端口 = %v, want 8091（应从监听地址推导，不得有默认值）", got["port"])
	}
	if got["namespaceId"] != "itestu" {
		t.Errorf("上线报文里命名空间 = %v, want itestu", got["namespaceId"])
	}
}
