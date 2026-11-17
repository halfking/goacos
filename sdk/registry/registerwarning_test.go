package registry

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// safeBuffer collects log output written by the heartbeat goroutine and by
// the test goroutine at the same time. bytes.Buffer is not safe for that, and
// a race in a test is a test that fails for the wrong reason.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// The container-bridge warning has to be reachable from the path that
// PUBLISHES an address.
//
// It used to live only in AdvertiseIP, which Register never calls: Register
// refuses to guess an address, so by the time it runs the address is always
// already in Config.IP — and the old check read a non-empty Config.IP as
// "the operator chose this one". The warning was therefore reachable only for
// a caller that asked for an address and then did nothing with it, while
// every real registration published a container-local address in silence.
//
// This drives Register on purpose. Calling the warning helper directly would
// prove that the helper works; it would not prove that anything calls it —
// which is exactly the mistake the first version of the sibling test in
// meshaddr_test.go made.
func TestRegisterWarnsWhenTheAdvertisedAddressWasAutoDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := testConfig(srv)
	cfg.IP = "10.89.11.126" // a podman address measured on 252
	cfg.IPAutoDetected = true
	c := NewClient(cfg, logger)

	restore := swapContainerNetns(true)
	defer restore()

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("no container warning on the registration path; log=%q", buf.String())
	}
}

// An address the operator wrote is used as given, and the SDK does not
// second-guess it — even when it looks container-local. The warning is about
// an address nobody chose.
func TestRegisterDoesNotWarnForAnOperatorChosenAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := testConfig(srv)
	cfg.IP = "10.89.11.126"
	cfg.IPAutoDetected = false // the operator chose it
	c := NewClient(cfg, logger)

	restore := swapContainerNetns(true)
	defer restore()

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("warned about an address the operator chose; log=%q", buf.String())
	}
}

// The SDK's own convenience API has to warn.
//
// ConfigFromEnv resolves the address, Announce publishes it, and nobody
// chose it — so this is the path a service that uses the SDK as documented
// takes. If the warning is unreachable here, it is unreachable in practice.
func TestAnnounceWarnsWhenTheAddressWasAutoDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, "ok")
	}))
	defer srv.Close()

	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	getenv := func(key string) string {
		switch key {
		case EnvEnabled:
			return "true"
		case EnvServerAddr:
			return srv.URL
		case EnvServiceName:
			return "redclaw-api"
		case EnvPort:
			return "8080"
		}
		return ""
	}

	restoreDetect := swapDetect(func(func(string) string) string { return "10.89.11.126" })
	defer restoreDetect()
	restoreNetns := swapContainerNetns(true)
	defer restoreNetns()

	cfg, err := ConfigFromEnv(getenv, Service{Name: "redclaw-api"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.IPAutoDetected {
		t.Fatalf("ConfigFromEnv resolved the address but did not record that nobody chose it")
	}

	stop := Announce(context.Background(), cfg, ":8080", logger)
	defer stop()

	if !strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("the Announce path published a container-local address without a warning; log=%q", buf.String())
	}
}

// The exported helper is the one definition an integration will use, so its
// boundaries are part of its contract.
func TestContainerLocalWarningBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		inContainer bool
		ip          string
		want        string // "" means "nothing to say"
	}{
		{"docker default pool, host or not", false, "172.18.0.5", "Docker's default bridge pool"},
		{"private address outside a container", false, "10.89.11.126", ""},
		{"private LAN address outside a container", false, "192.168.1.50", ""},
		{"private address inside a container", true, "10.89.11.126", "container's own bridge address"},
		{"private LAN address inside a container", true, "192.168.1.50", "container's own bridge address"},
		{"mesh address inside a container", true, "100.106.190.139", ""},
		{"public address", true, "8.8.8.8", ""},
		{"loopback", true, "127.0.0.1", ""},
		{"garbage", true, "not-an-ip", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := swapContainerNetns(tc.inContainer)
			defer restore()

			got := ContainerLocalWarning(tc.ip)
			if tc.want == "" {
				if got != "" {
					t.Errorf("ContainerLocalWarning(%q) = %q, want nothing to say", tc.ip, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("ContainerLocalWarning(%q) = %q, want it to mention %q", tc.ip, got, tc.want)
			}
		})
	}
}
