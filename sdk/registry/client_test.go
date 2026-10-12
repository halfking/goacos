package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig returns a Config pointed at srv with a short request timeout so a
// hung handler cannot stall a test.
func testConfig(srv *httptest.Server) Config {
	return Config{
		Enabled:        true,
		ServerAddr:     srv.URL,
		ServiceName:    "acc-go",
		GroupName:      DefaultGroupName,
		ClusterName:    DefaultClusterName,
		IP:             "10.0.0.5",
		Port:           4101,
		Weight:         1.0,
		Metadata:       map[string]string{"role": "control-plane"},
		RequestTimeout: 2 * time.Second,
	}
}

// testLogger keeps test output readable: only warnings and worse are printed.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeEnvelope writes the goacos success envelope with an optional data body.
func writeEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	env := map[string]any{"code": 0, "message": "success"}
	if data != nil {
		env["data"] = data
	}
	if err := json.NewEncoder(w).Encode(env); err != nil {
		t.Errorf("encode envelope: %v", err)
	}
}

func TestRegisterSendsCorrectRequestAndParsesEnvelope(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   registerRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode register body: %v", err)
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Metadata = map[string]string{"k": "v", "zone": "a"}
	c := NewClient(cfg, testLogger())

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := pathPrefix + pathInstance; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}

	want := registerRequest{
		ServiceName: "acc-go",
		IP:          "10.0.0.5",
		Port:        4101,
		NamespaceID: "",
		GroupName:   DefaultGroupName,
		ClusterName: DefaultClusterName,
		Weight:      1.0,
		Enabled:     true,
		Ephemeral:   true,
		Metadata:    map[string]string{"k": "v", "zone": "a"},
	}
	// reflect.DeepEqual rather than ==: the struct holds a map.
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("register body mismatch\n got: %+v\nwant: %+v", gotBody, want)
	}
}

func TestRegisterAppliesDefaults(t *testing.T) {
	var gotBody registerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.GroupName = ""
	cfg.ClusterName = ""
	cfg.Weight = 0
	c := NewClient(cfg, testLogger())

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if gotBody.GroupName != DefaultGroupName {
		t.Errorf("groupName = %q, want %q", gotBody.GroupName, DefaultGroupName)
	}
	if gotBody.ClusterName != DefaultClusterName {
		t.Errorf("clusterName = %q, want %q", gotBody.ClusterName, DefaultClusterName)
	}
	if gotBody.Weight != 1.0 {
		t.Errorf("weight = %v, want 1.0 (default for <= 0)", gotBody.Weight)
	}
	if gotBody.NamespaceID != "" {
		t.Errorf("namespaceId = %q, want empty (public namespace)", gotBody.NamespaceID)
	}
}

// A whitespace-only group/cluster is not an "unset" name to be filled in
// later — it is a DIFFERENT, unreachable name. Without normalization the
// instance lands in a bucket nobody queries, while every layer above it
// reports success. The Python binding already strips env values; this keeps
// the three SDKs agreeing on what an unset variable means.
func TestRegisterTreatsWhitespaceAsUnset(t *testing.T) {
	var gotBody struct {
		GroupName   string `json:"groupName"`
		ClusterName string `json:"clusterName"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.GroupName = "   "
	cfg.ClusterName = "\t "
	c := NewClient(cfg, testLogger())

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if gotBody.GroupName != DefaultGroupName {
		t.Errorf("whitespace groupName = %q, want %q", gotBody.GroupName, DefaultGroupName)
	}
	if gotBody.ClusterName != DefaultClusterName {
		t.Errorf("whitespace clusterName = %q, want %q", gotBody.ClusterName, DefaultClusterName)
	}
}

func TestRegisterFailsOnHTTP400PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// goacos answers a bad register with a 400 and plain text.
		http.Error(w, "serviceName/ip/port required", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())
	err := c.Register(context.Background())
	if err == nil {
		t.Fatal("Register: want error on HTTP 400, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention the status code, got: %v", err)
	}
	if !strings.Contains(err.Error(), "serviceName/ip/port required") {
		t.Errorf("error should quote the server body, got: %v", err)
	}
}

func TestRegisterRequiresIPAndPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called when identity is incomplete")
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	noIP := testConfig(srv)
	noIP.IP = ""
	err := NewClient(noIP, testLogger()).Register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "IP is not set") {
		t.Errorf("missing IP: want a clear error, got: %v", err)
	}

	noPort := testConfig(srv)
	noPort.Port = 0
	err = NewClient(noPort, testLogger()).Register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("missing port: want a clear error, got: %v", err)
	}
}

func TestBeatUsesServerAdvertisedInterval(t *testing.T) {
	var beats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathPrefix+pathBeat {
			// Start() registers too; only the beat is under test here.
			writeEnvelope(t, w, "ok")
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("beat method = %q, want POST", r.Method)
		}
		if got := r.URL.Query().Get("serviceName"); got != "acc-go" {
			t.Errorf("serviceName = %q, want acc-go", got)
		}
		if got := r.URL.Query().Get("port"); got != "4101" {
			t.Errorf("port = %q, want 4101", got)
		}
		beats.Add(1)
		writeEnvelope(t, w, beatData{
			ClientBeatInterval: 5000,
			Code:               10200,
			LightBeatEnabled:   true,
			Created:            false,
		})
	}))
	defer srv.Close()

	// A 20ms configured interval would produce ~25 beats in 500ms if the
	// client ignored the server and used its own value.
	cfg := testConfig(srv)
	cfg.HeartbeatInterval = 20 * time.Millisecond
	c := NewClient(cfg, testLogger())

	if err := c.Beat(context.Background()); err != nil {
		t.Fatalf("Beat: %v", err)
	}
	if got := c.heartbeatInterval(); got != 5*time.Second {
		t.Fatalf("after Beat, interval = %v, want 5s (the advertised value)", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	if n := beats.Load(); n != 1 {
		t.Errorf("beat count = %d after 300ms, want 1: the client must not hammer "+
			"using its own 20ms interval instead of the advertised 5s", n)
	}
}

func TestBeatClampsAbsurdlySmallAdvertisedInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, beatData{ClientBeatInterval: 1})
	}))
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())
	if err := c.Beat(context.Background()); err != nil {
		t.Fatalf("Beat: %v", err)
	}
	if got := c.heartbeatInterval(); got != minAdvertisedBeatInterval {
		t.Errorf("interval = %v, want the %v clamp", got, minAdvertisedBeatInterval)
	}
}

func TestDeregisterIssuesDeleteWithQueryParams(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.NamespaceID = "tenant-1"
	c := NewClient(cfg, testLogger())

	if err := c.Deregister(context.Background()); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if want := pathPrefix + pathInstance; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	for k, want := range map[string]string{
		"serviceName": "acc-go",
		"ip":          "10.0.0.5",
		"port":        "4101",
		"clusterName": DefaultClusterName,
		"namespaceId": "tenant-1",
		"groupName":   DefaultGroupName,
	} {
		if got := gotQuery.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
}

func TestListInstancesParsesHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != pathPrefix+pathInstanceList {
			t.Errorf("hit %s %s, want GET %s", r.Method, r.URL.Path, pathPrefix+pathInstanceList)
		}
		if got := r.URL.Query().Get("healthyOnly"); got != "true" {
			t.Errorf("healthyOnly = %q, want true", got)
		}
		writeEnvelope(t, w, instanceListData{
			Name: "DEFAULT_GROUP@@acc-go",
			Hosts: []Instance{
				{
					InstanceID: "10.0.0.5#4101#DEFAULT#DEFAULT_GROUP@@acc-go",
					IP:         "10.0.0.5", Port: 4101, Weight: 1,
					Healthy: true, Enabled: true, Ephemeral: true,
					ClusterName: "DEFAULT", ServiceName: "DEFAULT_GROUP@@acc-go",
					Metadata: map[string]string{"role": "control-plane"},
				},
				{
					InstanceID: "10.0.0.6#4101#DEFAULT#DEFAULT_GROUP@@acc-go",
					IP:         "10.0.0.6", Port: 4101, Weight: 1,
					Healthy: false, Enabled: true, Ephemeral: true,
					ClusterName: "DEFAULT", ServiceName: "DEFAULT_GROUP@@acc-go",
					Metadata: map[string]string{},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())
	hosts, err := c.ListInstances(context.Background(), "acc-go")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(hosts))
	}
	if !hosts[0].Healthy {
		t.Error("hosts[0].Healthy = false, want true")
	}
	if hosts[0].Metadata["role"] != "control-plane" {
		t.Errorf("hosts[0].Metadata[role] = %q, want control-plane", hosts[0].Metadata["role"])
	}
	if hosts[1].Healthy {
		t.Error("hosts[1].Healthy = true, want false")
	}
	if hosts[0].Port != 4101 || hosts[0].IP != "10.0.0.5" {
		t.Errorf("hosts[0] address = %s:%d, want 10.0.0.5:4101", hosts[0].IP, hosts[0].Port)
	}
}

func TestListInstancesRequiresServiceName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called without a service name")
	}))
	defer srv.Close()

	if _, err := NewClient(testConfig(srv), testLogger()).ListInstances(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty service name")
	}
}

func TestListServicesParsesNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathPrefix+pathServiceList {
			t.Errorf("path = %q, want %q", r.URL.Path, pathPrefix+pathServiceList)
		}
		if got := r.URL.Query().Get("pageNo"); got != "2" {
			t.Errorf("pageNo = %q, want 2", got)
		}
		if got := r.URL.Query().Get("pageSize"); got != "50" {
			t.Errorf("pageSize = %q, want 50", got)
		}
		writeEnvelope(t, w, serviceListData{
			Count: 2, PageNumber: 2, PageSize: 50,
			Services: []string{"DEFAULT_GROUP@@acc-go", "DEFAULT_GROUP@@acc-worker"},
		})
	}))
	defer srv.Close()

	services, err := NewClient(testConfig(srv), testLogger()).ListServices(context.Background(), 2, 50)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(services) != 2 || services[0] != "DEFAULT_GROUP@@acc-go" {
		t.Errorf("services = %v", services)
	}
}

func TestPing(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != pathPrefix+pathHealth {
				t.Errorf("path = %q, want %q", r.URL.Path, pathPrefix+pathHealth)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"UP","components":{}}`))
		}))
		defer srv.Close()
		if err := NewClient(testConfig(srv), testLogger()).Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("down", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"DOWN"}`))
		}))
		defer srv.Close()
		err := NewClient(testConfig(srv), testLogger()).Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "DOWN") {
			t.Fatalf("Ping on a DOWN registry: want an error, got: %v", err)
		}
	})
}

func TestAuthLogsInAndSendsBearerHeader(t *testing.T) {
	var (
		logins          atomic.Int32
		registerAuthHdr string
		loginForm       string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathPrefix+pathLogin:
			logins.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("login method = %q, want POST", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				t.Errorf("login Content-Type = %q, want form-encoded", ct)
			}
			body, _ := io.ReadAll(r.Body)
			loginForm = string(body)
			_, _ = w.Write([]byte(`{"accessToken":"tok-flat","tokenTtl":18000,"globalAdmin":true,` +
				`"username":"nacos","code":200,"message":"success",` +
				`"data":{"accessToken":"tok-enveloped","tokenTtl":18000}}`))
		case r.URL.Path == pathPrefix+pathInstance:
			registerAuthHdr = r.Header.Get("Authorization")
			writeEnvelope(t, w, "ok")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Username = "nacos"
	cfg.Password = "nacos"
	c := NewClient(cfg, testLogger())

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.Contains(loginForm, "username=nacos") || !strings.Contains(loginForm, "password=nacos") {
		t.Errorf("login form = %q, want username=nacos&password=nacos", loginForm)
	}
	if registerAuthHdr != "Bearer tok-flat" {
		t.Errorf("Authorization = %q, want %q", registerAuthHdr, "Bearer tok-flat")
	}

	// The token is cached: a second call must not log in again.
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("login count = %d, want 1 (the token must be cached)", n)
	}
}

func TestAuthPrefersEnvelopedToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathPrefix+pathLogin {
			// Only the enveloped copy is present.
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"accessToken":"tok-env"}}`))
			return
		}
		auth = r.Header.Get("Authorization")
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Username, cfg.Password = "nacos", "nacos"
	if err := NewClient(cfg, testLogger()).Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if auth != "Bearer tok-env" {
		t.Errorf("Authorization = %q, want Bearer tok-env (enveloped fallback)", auth)
	}
}

func TestAuthBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"unknown user"}`))
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Username, cfg.Password = "nacos", "wrong"
	err := NewClient(cfg, testLogger()).Register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Register with bad creds: want a 401 error, got: %v", err)
	}
}

func TestPreObtainedTokenSkipsLogin(t *testing.T) {
	var (
		loginHits atomic.Int32
		auth      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathPrefix+pathLogin {
			loginHits.Add(1)
			t.Error("a pre-supplied token must skip the login round trip")
		}
		auth = r.Header.Get("Authorization")
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Token = "preset"
	if err := NewClient(cfg, testLogger()).Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if auth != "Bearer preset" {
		t.Errorf("Authorization = %q, want Bearer preset", auth)
	}
	if loginHits.Load() != 0 {
		t.Error("login endpoint was hit despite a pre-obtained token")
	}
}

func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	ctx := context.Background()

	if got := c.AdvertiseIP(); got != "" {
		t.Errorf("AdvertiseIP() = %q, want \"\"", got)
	}
	if got := c.ServerAddr(); got != "" {
		t.Errorf("ServerAddr() = %q, want \"\"", got)
	}
	if c.Enabled() {
		t.Error("Enabled() = true on a nil client")
	}

	for name, call := range map[string]func() error{
		"Register":      func() error { return c.Register(ctx) },
		"Beat":          func() error { return c.Beat(ctx) },
		"Deregister":    func() error { return c.Deregister(ctx) },
		"ListInstances": func() error { _, err := c.ListInstances(ctx, "acc-go"); return err },
		"ListServices":  func() error { _, err := c.ListServices(ctx, 1, 10); return err },
		"Ping":          func() error { return c.Ping(ctx) },
		"usable":        func() error { return c.usable() },
		"ready":         func() error { return c.ready() },
	} {
		if err := call(); !errors.Is(err, ErrNilClient) {
			t.Errorf("nil client %s: err = %v, want ErrNilClient", name, err)
		}
	}

	// Stop and Start are deliberately tolerant no-ops rather than errors: a
	// shutdown path must never fail because the registry was never wired up.
	if err := c.Stop(ctx); err != nil {
		t.Errorf("nil client Stop = %v, want nil", err)
	}
	if got := c.heartbeatInterval(); got != 0 {
		t.Errorf("nil client heartbeatInterval() = %v, want 0", got)
	}
	c.Start(ctx)
}

func TestDisabledClientIsInert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a disabled client must not issue requests")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.Enabled = false
	c := NewClient(cfg, testLogger())

	if c.Enabled() {
		t.Error("Enabled() = true on a disabled client")
	}
	if err := c.Register(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("Register: err = %v, want ErrDisabled", err)
	}

	c.Start(context.Background())
	if err := c.Stop(context.Background()); err != nil {
		t.Errorf("Stop on a disabled client = %v, want nil", err)
	}
}

func TestStartIsNoOpWhenUnderConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an under-configured client must not issue requests")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.IP = "" // nothing to register as
	c := NewClient(cfg, testLogger())

	c.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	if err := c.Stop(context.Background()); err != nil {
		t.Errorf("Stop = %v, want nil", err)
	}
}

func TestStopWithoutStartDeregistersAndIsIdempotent(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())

	// Stop without Start: best-effort cleanup, and the server says ok even
	// though nothing was ever registered.
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if deletes.Load() != 1 {
		t.Errorf("DELETE count = %d after first Stop, want 1", deletes.Load())
	}

	// Stop twice must not panic or re-issue the request.
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if deletes.Load() != 1 {
		t.Errorf("DELETE count = %d after second Stop, want 1 (Stop must be idempotent)", deletes.Load())
	}
}

func TestStopWithCancelledContextSkipsDeregistration(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := NewClient(testConfig(srv), testLogger()).Stop(ctx); err != nil {
		t.Fatalf("Stop with a cancelled ctx = %v, want nil (best-effort)", err)
	}
	if deletes.Load() != 0 {
		t.Errorf("DELETE count = %d, want 0 (no request can be sent)", deletes.Load())
	}
}

func TestStartRegistersThenBeatsAndStopDeregisters(t *testing.T) {
	var (
		registers atomic.Int32
		beats     atomic.Int32
		deletes   atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathPrefix+pathInstance && r.Method == http.MethodPost:
			registers.Add(1)
			writeEnvelope(t, w, "ok")
		case r.URL.Path == pathPrefix+pathBeat:
			beats.Add(1)
			// A short advertised interval so this test observes several
			// beats; TestBeatUsesServerAdvertisedInterval covers the long one.
			writeEnvelope(t, w, beatData{ClientBeatInterval: 10})
		case r.URL.Path == pathPrefix+pathInstance && r.Method == http.MethodDelete:
			deletes.Add(1)
			writeEnvelope(t, w, "ok")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.HeartbeatInterval = 20 * time.Millisecond
	c := NewClient(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	waitFor(t, time.Second, func() bool { return beats.Load() >= 2 })

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if registers.Load() == 0 {
		t.Error("expected at least one registration")
	}
	if deletes.Load() != 1 {
		t.Errorf("DELETE count = %d, want 1", deletes.Load())
	}

	// Cancelling ctx must stop the goroutine promptly.
	cancel()
	time.Sleep(100 * time.Millisecond)
	afterCancel := beats.Load()
	time.Sleep(100 * time.Millisecond)
	if beats.Load() != afterCancel {
		t.Errorf("beats continued after ctx cancellation: %d -> %d", afterCancel, beats.Load())
	}
}

func TestStartRetriesRegistrationUntilItSucceeds(t *testing.T) {
	var registers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathPrefix+pathBeat {
			writeEnvelope(t, w, beatData{ClientBeatInterval: 5000})
			return
		}
		// Fail the first three registration attempts.
		if registers.Add(1) <= 3 {
			http.Error(w, "registry starting up", http.StatusServiceUnavailable)
			return
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.HeartbeatInterval = 20 * time.Millisecond
	c := NewClient(cfg, testLogger())

	// Shrink this client's retry ladder so the test does not spend seconds in
	// backoff. Per-client rather than global: a package variable would race
	// with any lifecycle goroutine still winding down from an earlier test.
	// The default cap is asserted in TestNextBackoffCapsAtThirtySeconds.
	c.registerBackoffInitial = 5 * time.Millisecond
	c.registerBackoffMax = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	waitFor(t, 3*time.Second, func() bool { return registers.Load() >= 4 })
	stopClient(t, c, cancel)
}

func TestReRegistersAfterRepeatedBeatFailure(t *testing.T) {
	var (
		registers atomic.Int32
		beats     atomic.Int32
		mu        sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathPrefix+pathBeat {
			n := beats.Add(1)
			// Fail beats 1..3, succeed afterwards.
			if n <= int32(beatFailureThreshold) {
				http.Error(w, "beat rejected", http.StatusInternalServerError)
				return
			}
			writeEnvelope(t, w, beatData{ClientBeatInterval: 5000})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == pathPrefix+pathInstance {
			mu.Lock()
			registers.Add(1)
			mu.Unlock()
			writeEnvelope(t, w, "ok")
			return
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	cfg := testConfig(srv)
	cfg.HeartbeatInterval = 10 * time.Millisecond
	c := NewClient(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return registers.Load() >= 2
	})
	stopClient(t, c, cancel)
}

func TestNextBackoffCapsAtThirtySeconds(t *testing.T) {
	c := NewClient(Config{Enabled: true}, testLogger())
	if c.registerBackoffInitial != time.Second || c.registerBackoffMax != 30*time.Second {
		t.Fatalf("default ladder = %v..%v, want 1s..30s",
			c.registerBackoffInitial, c.registerBackoffMax)
	}
	d := c.registerBackoffInitial
	for i := 0; i < 10; i++ {
		d = c.nextBackoff(d)
	}
	if d != 30*time.Second {
		t.Errorf("backoff after growth = %v, want 30s (capped)", d)
	}
}

func TestAdvertiseIPFallbackOrdering(t *testing.T) {
	// Stub interface table: down interfaces, loopback, link-local, then a
	// usable global-unicast IPv4.
	fakeIfaces := func() ([]netInterface, error) {
		return []netInterface{
			{Name: "down0", Flags: 0, Addrs: fakeAddrs("192.0.2.9")},
			{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: fakeAddrs("127.0.0.1", "::1")},
			{Name: "docker0", Flags: net.FlagUp, Addrs: fakeAddrs("fe80::1", "169.254.1.1")},
			{Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast, Addrs: fakeAddrs("10.0.0.7")},
			{Name: "eth1", Flags: net.FlagUp, Addrs: fakeAddrs("10.0.0.8")},
		}, nil
	}

	if got := selectAdvertiseIP("10.1.1.1", fakeIfaces); got != "10.1.1.1" {
		t.Errorf("explicit config: got %q, want 10.1.1.1 (explicit always wins)", got)
	}
	if got := selectAdvertiseIP("", fakeIfaces); got != "10.0.0.7" {
		t.Errorf("auto-detect: got %q, want 10.0.0.7", got)
	}
	// Only loopback and link-local available: no auto-detect answer, and the
	// caller falls back to loopback.
	loopbackOnly := func() ([]netInterface, error) {
		return []netInterface{
			{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: fakeAddrs("127.0.0.1")},
		}, nil
	}
	if got := selectAdvertiseIP("", loopbackOnly); got != "" {
		t.Errorf("loopback-only auto-detect: got %q, want \"\" so AdvertiseIP falls back", got)
	}
	// Lister failure must not panic.
	boom := func() ([]netInterface, error) { return nil, errors.New("boom") }
	if got := selectAdvertiseIP("", boom); got != "" {
		t.Errorf("failing lister: got %q, want \"\"", got)
	}
	if got := selectAdvertiseIP("", nil); got != "" {
		t.Errorf("nil lister: got %q, want \"\"", got)
	}
}

// TestAdvertiseIPFallsBackToLoopback covers the last link of the chain against
// an injected lister, so the result does not depend on the host's real NICs.
func TestAdvertiseIPFallsBackToLoopback(t *testing.T) {
	orig := ifaces
	t.Cleanup(func() { ifaces = orig })
	ifaces = func() ([]netInterface, error) {
		return []netInterface{
			{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: fakeAddrs("127.0.0.1")},
		}, nil
	}

	if got := NewClient(Config{Enabled: true}, testLogger()).AdvertiseIP(); got != "127.0.0.1" {
		t.Errorf("AdvertiseIP() fallback = %q, want 127.0.0.1", got)
	}
}

func TestAdvertiseIPPrefersExplicitConfig(t *testing.T) {
	// The package-level lister is replaced so this test does not depend on
	// the host's real interfaces. Restored via t.Cleanup; no t.Parallel
	// anywhere in this file keeps this race-free.
	orig := ifaces
	t.Cleanup(func() { ifaces = orig })
	ifaces = func() ([]netInterface, error) { return nil, errors.New("should not be called") }

	c := NewClient(Config{Enabled: true, IP: "192.168.1.10"}, testLogger())
	if got := c.AdvertiseIP(); got != "192.168.1.10" {
		t.Errorf("AdvertiseIP() = %q, want 192.168.1.10", got)
	}
}

func TestNewClientToleratesNilLogger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	c := NewClient(testConfig(srv), nil) // must not panic
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register with a nil logger: %v", err)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"goacos:8848":                "http://goacos:8848",
		"http://goacos:8848":         "http://goacos:8848",
		"http://goacos:8848/":        "http://goacos:8848",
		"https://nacos.example.com/": "https://nacos.example.com",
		"  http://goacos:8848  ":     "http://goacos:8848",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsableRequiresServerAddrAndServiceName(t *testing.T) {
	noAddr := NewClient(Config{Enabled: true, ServiceName: "acc-go"}, testLogger())
	if err := noAddr.usable(); !errors.Is(err, ErrDisabled) {
		t.Errorf("no ServerAddr: err = %v, want ErrDisabled", err)
	}
	noSvc := NewClient(Config{Enabled: true, ServerAddr: "http://goacos:8848"}, testLogger())
	if err := noSvc.usable(); !errors.Is(err, ErrDisabled) {
		t.Errorf("no ServiceName: err = %v, want ErrDisabled", err)
	}
}

// fakeAddrs builds net.Addr values for the injected interface lister.
func fakeAddrs(ips ...string) []net.Addr {
	out := make([]net.Addr, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			out = append(out, &net.IPNet{IP: v4, Mask: net.CIDRMask(24, 32)})
			continue
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(64, 128)})
	}
	return out
}

// waitFor polls cond until it holds or the timeout expires. Polling beats a
// fixed sleep here because the lifecycle loop's cadence depends on the values
// under test.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// stopClient cancels ctx and then waits for the lifecycle goroutine to exit,
// so one test cannot leak a running loop into the next one.
func stopClient(t *testing.T, c *Client, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
