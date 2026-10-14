package gateway

// Tests for the path-level controls in security.go.
//
// Each test stands up a real upstream and asserts both what the client received
// *and* whether the upstream was reached. A rule that refused but still
// forwarded would pass a client-side-only assertion whenever the upstream
// happens to answer the same status — so both halves are checked.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/config"
)

// secUpstream starts a server that records whether it was reached.
func secUpstream(t *testing.T, reached *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// secTable wires one route on sec.test to a live upstream.
func secTable(t *testing.T, up *httptest.Server) *Table {
	t.Helper()
	host, port := splitHostPort(t, up.URL)
	return buildTable(testRoute{
		domain:    "sec.test",
		component: "api",
		service:   "sec-api",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
}

// TestSecurityHeadersReachTheClient is the whole point of the feature: the
// four headers a fronting nginx adds with `add_header ... always` must survive
// the domain moving onto this gateway.
func TestSecurityHeadersReachTheClient(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewaySecurityHeaders = []string{
			"X-Content-Type-Options: nosniff",
			"X-Frame-Options: SAMEORIGIN",
			"Referrer-Policy: no-referrer-when-downgrade",
			"Permissions-Policy: geolocation=(), microphone=(), camera=()",
		}
	})

	rec := httptest.NewRecorder()
	g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://sec.test/", nil), false)

	if !reached {
		t.Fatal("upstream was not reached; this test cannot prove the headers appear on the served path")
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer-when-downgrade",
		"Permissions-Policy":     "geolocation=(), microphone=(), camera=()",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestSecurityHeadersSurviveARejection pins that headers are written before the
// status. A header set after WriteHeader is silently discarded — which is the
// exact shape of the bug this replaces.
func TestSecurityHeadersSurviveARejection(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewaySecurityHeaders = []string{"X-Content-Type-Options: nosniff"}
		cfg.GatewayBlockUserAgents = []string{`(?i)sqlmap`}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://sec.test/", nil)
	req.Header.Set("User-Agent", "sqlmap/1.7")
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q on a refused response; the header was lost with the status",
			got)
	}
}

// TestScannerUserAgentIsRefusedBeforeForwarding covers the 403 rule. The
// upstream must NOT be reached — rewriting the response after forwarding would
// still leak the request.
func TestScannerUserAgentIsRefusedBeforeForwarding(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewayBlockUserAgents = []string{`(?i)sqlmap|nikto|nessus|nmap|masscan|zgrab`}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://sec.test/", nil)
	req.Header.Set("User-Agent", "sqlmap/1.7.2#stable")
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if reached {
		t.Error("scanner request reached the upstream; it must be refused before forwarding")
	}
}

// TestOrdinaryUserAgentStillPasses is the negative case. Without it, a rule
// that refused every request would satisfy the test above.
func TestOrdinaryUserAgentStillPasses(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewayBlockUserAgents = []string{`(?i)sqlmap|nikto|nessus|nmap|masscan|zgrab`}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://sec.test/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a normal browser", rec.Code)
	}
	if !reached {
		t.Error("a normal browser request did not reach the upstream")
	}
}

// TestSensitivePathsAreRefused covers the 404 rule and pins 404 rather than
// 403: a scanner probing /.git/config should not learn the path exists.
func TestSensitivePathsAreRefused(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewayBlockPaths = []string{
			`(^|/)\.(git|env|svn|hg|bzr)(/|$)`,
			`/(wp-admin|wp-login|phpmyadmin|adminer)(\.php)?$`,
		}
	})

	for _, path := range []string{"/.git/config", "/app/.env", "/wp-admin", "/adminer.php"} {
		reached = false
		rec := httptest.NewRecorder()
		g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://sec.test"+path, nil), false)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if reached {
			t.Errorf("%s: reached the upstream; sensitive paths must be refused before forwarding", path)
		}
	}

	// The admin rule is anchored at the end of the path, exactly as the nginx
	// snippet's `location ~* /(wp-admin|…)(\.php)?$` is. So `/wp-admin/install.php`
	// is NOT covered — and is not covered behind nginx today either. Recorded
	// here so nobody reads this test as "all wp-admin paths are blocked".
	// Tightening it would change behaviour relative to the current front door.

	// A path that merely resembles one must still be served — otherwise the
	// rule silently breaks a real application route.
	reached = false
	rec := httptest.NewRecorder()
	g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://sec.test/environment", nil), false)
	if rec.Code != http.StatusOK || !reached {
		t.Errorf("/environment: status=%d reached=%v, want 200 and reached — the rule over-matches",
			rec.Code, reached)
	}
}

// TestMalformedEntriesAreSkippedNotFatal pins the documented trade-off: one bad
// line must not disable the valid rules, nor the domain itself.
func TestMalformedEntriesAreSkippedNotFatal(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), func(cfg *config.Config) {
		cfg.GatewaySecurityHeaders = []string{
			"this line has no colon and must be skipped",
			": a leading colon means an empty name and must be skipped",
			"X-Kept: yes",
			"X-Colon-In-Value: default-src 'self'; frame-ancestors 'none'",
		}
		cfg.GatewayBlockUserAgents = []string{`[unclosed`, `(?i)sqlmap`}
	})

	rec := httptest.NewRecorder()
	g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://sec.test/", nil), false)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("status=%d reached=%v, want 200 and reached", rec.Code, reached)
	}
	if got := rec.Header().Get("X-Kept"); got != "yes" {
		t.Errorf("X-Kept = %q, want %q — a malformed sibling line swallowed a valid one", got, "yes")
	}
	if got := rec.Header().Get("X-Colon-In-Value"); !strings.HasPrefix(got, "default-src") {
		t.Errorf("X-Colon-In-Value = %q; the value must keep its own colons", got)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://sec.test/", nil)
	req.Header.Set("User-Agent", "sqlmap/1.7")
	g.dispatch(rec, req, false)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — an uncompilable sibling pattern disabled the valid one",
			rec.Code)
	}
}

// TestNoConfigurationAddsNothing guards the default path: with the feature off
// a gateway must behave exactly as it did before.
func TestNoConfigurationAddsNothing(t *testing.T) {
	var reached bool
	up := secUpstream(t, &reached)
	g := testGateway(secTable(t, up), nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://sec.test/.git/config", nil)
	req.Header.Set("User-Agent", "sqlmap/1.7")
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status=%d reached=%v, want 200 and reached — with nothing configured nothing may be refused",
			rec.Code, reached)
	}
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options",
		"Referrer-Policy", "Permissions-Policy"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want absent when no headers are configured", h, got)
		}
	}
}
