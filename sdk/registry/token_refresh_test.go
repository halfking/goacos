package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The bug these guard: the login response advertises tokenTtl (18000s on a
// default goacos) and the client used to cache the token forever. Five hours
// into a long-running process every register and beat starts returning 401, goacos
// marks the ephemeral instance unhealthy and then drops it — and because the
// stale token is still cached and still considered valid, the instance never
// comes back. Service discovery fails silently and permanently, with a process
// restart as the only cure.
func TestTokenIsRefreshedAfterAdvertisedTTL(t *testing.T) {
	var (
		logins atomic.Int32
		tokens atomic.Value // string: the bearer the server last accepted
		issued = make(chan string, 8)
	)
	tokens.Store("tok-0")

	// ttl=2s so the refresh boundary is reachable inside a test without
	// sleeping for the real five hours.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathPrefix+pathLogin:
			n := logins.Add(1)
			tok := "tok-" + string(rune('0'+n))
			tokens.Store(tok)
			select {
			case issued <- tok:
			default:
			}
			_, _ = w.Write([]byte(`{"accessToken":"` + tok + `","tokenTtl":2,"code":200,"message":"success"}`))
		case r.URL.Path == pathPrefix+pathInstance:
			if got := r.Header.Get("Authorization"); got != "Bearer "+tokens.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":401,"message":"unknown user"}`))
				return
			}
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
		t.Fatalf("first Register: %v", err)
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("logins after first Register = %d, want 1", got)
	}

	// Still inside the TTL: the cached token must be reused, not re-fetched.
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if got := logins.Load(); got != 1 {
		t.Errorf("logins after a second Register inside the TTL = %d, want 1 (token must be cached)", got)
	}

	// Past the TTL plus the refresh margin: a fresh login is required.
	c.mu.Lock()
	c.tokenExpiry = time.Now().Add(-time.Second)
	c.mu.Unlock()

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register after expiry: %v", err)
	}
	if got := logins.Load(); got != 2 {
		t.Errorf("logins after the TTL elapsed = %d, want 2 — an expired token must be replaced, not reused", got)
	}
}

// A registry that rejects a token we still believe is valid must trigger
// exactly one re-login and one retry. This is the recovery path for what the
// TTL arithmetic cannot predict: a registry restart, an out-of-band
// revocation, or a server whose advertised TTL outlasts the token it honours.
func TestAuthRejectionRefreshesAndRetriesOnce(t *testing.T) {
	var (
		logins        atomic.Int32
		instanceCalls atomic.Int32
		// revokedFrom is the login count from which tokens issued BEFORE it are
		// refused, while a freshly minted token is accepted. That models the
		// real situation a re-login fixes: our cached token was revoked
		// out of band, but the registry will happily mint another.
		revokedFrom atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathPrefix+pathLogin:
			n := logins.Add(1)
			_, _ = w.Write([]byte(`{"accessToken":"tok-` +
				string(rune('0'+n)) + `","tokenTtl":18000,"code":200,"message":"success"}`))
		case r.URL.Path == pathPrefix+pathInstance:
			instanceCalls.Add(1)
			from := revokedFrom.Load()
			if from > 0 && logins.Load() < from {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":403,"message":"token invalid"}`))
				return
			}
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
	// From here on the token we are holding (tok-1) is revoked; only a fresh
	// login yields one this server accepts.
	revokedFrom.Store(2)
	baselineCalls := instanceCalls.Load()

	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register against a revoked token should recover via re-login, got: %v", err)
	}
	if got := instanceCalls.Load() - baselineCalls; got != 2 {
		t.Errorf("instance calls for one Register = %d, want 2 (original + exactly one retry)", got)
	}
	if got := logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2 (initial + refresh triggered by the rejection)", got)
	}

	// Now the registry refuses every token, including freshly minted ones
	// (revokedFrom above the current login count). The call must FAIL rather
	// than loop: an unbounded retry turns a genuinely bad password into a
	// login storm against an already-unhealthy registry.
	revokedFrom.Store(logins.Load() + 10)
	callsBefore := instanceCalls.Load()
	loginsBefore := logins.Load()
	if err := c.Register(context.Background()); err == nil {
		t.Error("Register must fail when the registry keeps rejecting our credentials")
	}
	if got := instanceCalls.Load() - callsBefore; got != 2 {
		t.Errorf("instance calls for the failing Register = %d, want 2 — the retry must be bounded", got)
	}
	// Exactly ONE re-login, not one per attempt: the first attempt reuses the
	// cached (wrong) token, the retry is what pays for the new login. Two
	// attempts must not mean two logins.
	if got := logins.Load() - loginsBefore; got != 1 {
		t.Errorf("logins for the failing Register = %d, want 1 — re-login must not loop", got)
	}
}

// A registry that authenticates but advertises no TTL must not leave us
// caching the token forever. Assuming the registry's own default is the
// conservative choice: it refreshes on a schedule instead of never.
func TestMissingTokenTTLIsNotCachedForever(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathPrefix+pathLogin:
			logins.Add(1)
			_, _ = w.Write([]byte(`{"accessToken":"tok","code":200,"message":"success"}`))
		case r.URL.Path == pathPrefix+pathInstance:
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

	c.mu.Lock()
	hasExpiry := !c.tokenExpiry.IsZero()
	lifetime := c.tokenExpiry.Sub(time.Now())
	c.mu.Unlock()

	if !hasExpiry {
		t.Fatal("a login with no advertised TTL must still produce a refresh deadline")
	}
	if lifetime <= 0 {
		t.Errorf("assumed token lifetime = %s, want a positive schedule", lifetime)
	}
	if lifetime > defaultTokenTTL {
		t.Errorf("assumed token lifetime = %s, must not exceed the registry default %s",
			lifetime, defaultTokenTTL)
	}
}

// A caller-supplied token is not ours to refresh. If it is rejected we must
// report the failure rather than clearing it, because clearing would turn a
// recoverable 401 into permanent failure for the life of the process.
func TestCallerSuppliedTokenIsNeverCleared(t *testing.T) {
	c := NewClient(Config{
		Enabled:    true,
		ServerAddr: "http://127.0.0.1:1",
		Token:      "operator-token",
	}, testLogger())

	c.mu.Lock()
	c.token = "operator-token"
	c.mu.Unlock()

	c.invalidateToken()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "operator-token" {
		t.Errorf("token = %q, want the caller's token left untouched", c.token)
	}
}

// A transport failure must NOT trigger a re-login. Re-logging in cannot fix a
// dial timeout, and doing so on every blip would hammer the login endpoint
// precisely when the registry is already unwell.
func TestTransportFailureDoesNotTriggerRelogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing is listening now: every call is a transport error

	var logins atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathPrefix+pathLogin {
			logins.Add(1)
			_, _ = w.Write([]byte(`{"accessToken":"tok","tokenTtl":18000,"code":200}`))
			return
		}
		writeEnvelope(t, w, "ok")
	}))
	defer srv2.Close()

	cfg := testConfig(srv2)
	cfg.Username = "nacos"
	cfg.Password = "nacos"
	c := NewClient(cfg, testLogger())
	// Warm the token cache so a later transport failure cannot be explained by
	// "it had not logged in yet".
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("warm-up Register: %v", err)
	}
	before := logins.Load()

	deadAddr := strings.Replace(addr, "127.0.0.1", "127.0.0.1", 1)
	c.cfg.ServerAddr = deadAddr
	c.base = deadAddr

	err := c.Register(context.Background())
	if err == nil {
		t.Fatal("Register against a dead registry must fail")
	}
	if got := logins.Load(); got != before {
		t.Errorf("logins went from %d to %d on a transport failure; re-login cannot fix a dial error", before, got)
	}
}
