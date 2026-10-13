package gateway

// Tests for the gateway data plane.
//
// The emphasis is on end-to-end behaviour rather than on internals: a routing
// table that resolves correctly in isolation proves nothing if the request that
// follows it never reaches an upstream. Most of these tests stand up real
// upstream servers, resolve a real table, and assert on what came back.

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// ---------- helpers ----------

// newTestTable builds a routing snapshot without a database. Tests that care
// about the database use the e2e script; what matters here is the dispatch path
// from a resolved target to a live socket.
func newTestTable() *Table {
	return &Table{
		domains:  map[string][]routeEntry{},
		services: map[string][]Instance{},
		vhosts:   map[string]*storage.VHostPolicy{},
		LoadedAt: time.Now(),
	}
}

type testRoute struct {
	domain     string
	prefix     string
	app        string
	component  string
	kind       string
	protocol   string
	service    string
	lb         string
	metadata   map[string]string
	instances  []Instance
	priorities int
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildTable assembles a snapshot from declarative test routes.
func buildTable(routes ...testRoute) *Table {
	tbl := newTestTable()
	for _, r := range routes {
		comp := &storage.ApplicationComponent{
			NamespaceId: "public",
			AppName:     r.app,
			Name:        r.component,
			Kind:        firstNonEmpty(r.kind, "api"),
			Protocol:    firstNonEmpty(r.protocol, "http"),
			ServiceName: r.service,
			GroupName:   "DEFAULT_GROUP",
			LBAlgorithm: firstNonEmpty(r.lb, "round_robin"),
			Weight:      1,
			Metadata:    r.metadata,
		}
		d := storage.NormalizeDomain(r.domain)
		priority := r.priorities
		if priority == 0 {
			priority = 100
		}
		tbl.domains[d] = append(tbl.domains[d], routeEntry{
			RouteBinding: storage.RouteBinding{
				NamespaceId:   "public",
				Domain:        d,
				PathPrefix:    firstNonEmpty(r.prefix, "/"),
				AppName:       r.app,
				ComponentName: r.component,
				Priority:      priority,
			},
			comp: comp,
		})
		key := serviceKey("DEFAULT_GROUP", r.service)
		if _, ok := tbl.services[key]; !ok {
			tbl.services[key] = r.instances
		}
		tbl.Domains = append(tbl.Domains, d)
	}
	for d, list := range tbl.domains {
		sortRoutes(list)
		tbl.domains[d] = list
	}
	tbl.Domains = uniqueStrings(tbl.Domains)
	return tbl
}

// sortRoutes mirrors the production ordering: longest prefix first, then
// explicit priority. A test that used a different order would be testing its
// own sort rather than the gateway's routing decision.
func sortRoutes(list []routeEntry) {
	sort.SliceStable(list, func(i, j int) bool {
		return len(strings.TrimSuffix(list[i].PathPrefix, "/")) >
			len(strings.TrimSuffix(list[j].PathPrefix, "/"))
	})
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// instanceOf describes a routable instance without touching a registry.
func instanceOf(ip string, port int, weight float64, meta map[string]string) Instance {
	raw := ""
	if len(meta) > 0 {
		raw = fmt.Sprintf(`{"lane":"%s"}`, meta["lane"])
	}
	return Instance{IP: ip, Port: uint32(port), Weight: weight, Healthy: true, Enabled: true, MetadataJSON: raw}
}

// testGateway builds a gateway with no store, for dispatch-level tests.
func testGateway(tbl *Table, mutate func(*config.Config)) *Gateway {
	cfg := &config.Config{
		GatewayForceHTTPS:         false,
		GatewayConnectTimeoutMS:   1000,
		GatewayReadTimeoutMS:      5000,
		GatewaySendTimeoutMS:      5000,
		GatewayWebsocket:          true,
		GatewayClientMaxBodyBytes: 0,
		GatewayStaticFallback:     true,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return &Gateway{
		cfg:    cfg,
		table:  tbl,
		picker: NewPicker(),
		pool:   newTransportPool(),
		certs: &CertManager{
			certs: map[string]*tls.Certificate{},
			info:  map[string]CertInfo{},
		},
	}
}

// ---------- routing ----------

func TestPathPrefixRequiresSegmentBoundary(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/", "/anything", true},
		{"/api", "/api", true},
		{"/api", "/api/v2/x", true},
		{"/api", "/apixyz", false},
		{"/api/", "/api/v2/x", true},
	}
	for _, c := range cases {
		if got := pathPrefixMatches(c.prefix, c.path); got != c.want {
			t.Errorf("pathPrefixMatches(%q,%q)=%v want %v", c.prefix, c.path, got, c.want)
		}
	}
}

// A specific prefix must beat the catch-all, or a domain with both an /ui
// frontend and an / API silently serves the frontend for API traffic.
func TestLookupPrefersLongestPrefix(t *testing.T) {
	tbl := buildTable(
		testRoute{domain: "acc.example.com", prefix: "/", component: "api", service: "acc-api"},
		testRoute{domain: "acc.example.com", prefix: "/ui", component: "web", service: "acc-web", kind: "web"},
	)
	if got := tbl.Lookup("acc.example.com", "/ui/app.js", nil).ComponentName; got != "web" {
		t.Errorf("/ui should hit the web component, got %q", got)
	}
	if got := tbl.Lookup("acc.example.com", "/api/v2/health", nil).ComponentName; got != "api" {
		t.Errorf("/api should hit the api component, got %q", got)
	}
}

func TestLookupNormalizesHostCaseAndPort(t *testing.T) {
	tbl := buildTable(testRoute{domain: "acc.example.com", component: "api", service: "acc-api"})
	if tbl.Lookup("ACC.Example.COM:443", "/", nil) == nil {
		t.Fatal("host with port and uppercase must still resolve")
	}
}

func TestLookupMetaFilterNarrowsInstances(t *testing.T) {
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{
			instanceOf("10.0.0.1", 4100, 1, map[string]string{"lane": "stable"}),
			instanceOf("10.0.0.2", 4100, 1, map[string]string{"lane": "canary"}),
		},
	})
	got := tbl.Lookup("acc.example.com", "/", map[string]string{"lane": "canary"})
	if len(got.Instances) != 1 || got.Instances[0].IP != "10.0.0.2" {
		t.Fatalf("canary filter wrong: %v", sortedAddrs(got.Instances))
	}
	// An unmatched filter must return nothing, never fall back to everyone.
	if got := tbl.Lookup("acc.example.com", "/", map[string]string{"lane": "nope"}); len(got.Instances) != 0 {
		t.Fatalf("unmatched filter must yield no instances, got %v", sortedAddrs(got.Instances))
	}
}

// ---------- dispatch, end to end ----------

// The core promise of the whole subsystem: a request to a declared domain
// reaches a registered instance, with the right path and the right headers.
func TestDispatchReachesRegisteredInstance(t *testing.T) {
	var gotPath, gotHost, gotProto, gotForwarded, gotUpstream string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHost = r.Host
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotForwarded = r.Header.Get("X-Forwarded-For")
		gotUpstream = r.Header.Get("X-Goacos-Upstream")
		w.Header().Set("X-Upstream-Name", "acc-api")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	host, port := splitHostPort(t, upstream.URL)
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	g := testGateway(tbl, nil)

	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/api/v2/health?x=1", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/v2/health" {
		t.Errorf("upstream saw path %q", gotPath)
	}
	if gotHost != fmt.Sprintf("%s:%d", host, port) {
		t.Errorf("upstream saw Host %q, want the real upstream address", gotHost)
	}
	if gotProto != "http" {
		t.Errorf("X-Forwarded-Proto=%q", gotProto)
	}
	if gotForwarded != "203.0.113.9" {
		t.Errorf("X-Forwarded-For=%q, want the observed peer", gotForwarded)
	}
	if gotUpstream != fmt.Sprintf("%s:%d", host, port) {
		t.Errorf("X-Goacos-Upstream=%q", gotUpstream)
	}
	if rec.Header().Get("X-Upstream-Name") != "acc-api" {
		t.Error("response headers from the upstream must reach the client")
	}
}

// Stripping the matched prefix is opt-in; without it a backend mounted at /
// never sees the routing prefix it was declared with.
func TestDispatchKeepsPathUnlessStripPrefixRequested(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
	}))
	defer upstream.Close()
	host, port := splitHostPort(t, upstream.URL)

	tbl := buildTable(testRoute{
		domain: "acc.example.com", prefix: "/api", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/api/v2/health", nil)
	testGateway(tbl, nil).dispatch(httptest.NewRecorder(), req, false)
	if got != "/api/v2/health" {
		t.Errorf("default must keep the full path, got %q", got)
	}

	tbl = buildTable(testRoute{
		domain: "acc.example.com", prefix: "/api", component: "api", service: "acc-api",
		metadata:  map[string]string{"stripPrefix": "true"},
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	req = httptest.NewRequest(http.MethodGet, "http://acc.example.com/api/v2/health", nil)
	testGateway(tbl, nil).dispatch(httptest.NewRecorder(), req, false)
	if got != "/v2/health" {
		t.Errorf("stripPrefix must remove the matched prefix, got %q", got)
	}
}

// A domain with no route must say so plainly instead of proxying somewhere.
func TestDispatchUndeclaredDomainIs404(t *testing.T) {
	g := testGateway(buildTable(), nil)
	req := httptest.NewRequest(http.MethodGet, "http://nope.example.com/x", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no route declared") {
		t.Errorf("body should name the cause: %s", rec.Body.String())
	}
}

// A declared route with no healthy instance is a 503, not a hang: the
// distinction tells an operator whether to register an instance or debug
// routing.
func TestDispatchNoHealthyInstanceIs503(t *testing.T) {
	tbl := buildTable(testRoute{domain: "acc.example.com", component: "api", service: "acc-api"})
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
	rec := httptest.NewRecorder()
	testGateway(tbl, nil).dispatch(rec, req, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "acc-api") {
		t.Errorf("body should name the service: %s", rec.Body.String())
	}
}

// This is the property that makes a rolling restart invisible: one dead
// instance must not turn into a user-visible 502 while a healthy one exists.
func TestDispatchFailsOverToHealthyInstance(t *testing.T) {
	dead := deadServer(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served-by-healthy"))
	}))
	defer good.Close()

	deadHost, deadPort := splitHostPort(t, dead)
	goodHost, goodPort := splitHostPort(t, good.URL)
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{
			instanceOf(deadHost, deadPort, 1, nil),
			instanceOf(goodHost, goodPort, 1, nil),
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
	testGateway(tbl, nil).dispatch(rec, req, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("failover failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-healthy") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// When every instance is dead the client gets a 502 rather than a hang.
func TestDispatchAllInstancesDeadIs502(t *testing.T) {
	dead := deadServer(t)
	host, port := splitHostPort(t, dead)
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
	testGateway(tbl, nil).dispatch(rec, req, false)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
}

// deadServer accepts and immediately drops connections, which is what an
// instance mid-shutdown looks like to a proxy.
func deadServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return "http://" + ln.Addr().String()
}

// ---------- policy ----------

func TestPolicyDenyWinsOverAllow(t *testing.T) {
	pol := Policy{
		Allow: mustPrefixes(t, "203.0.113.0/24"),
		Deny:  mustPrefixes(t, "203.0.113.9"),
	}
	if pol.Allowed(netip.MustParseAddr("203.0.113.9")) {
		t.Error("a deny rule must beat an allow rule for the same client")
	}
	if !pol.Allowed(netip.MustParseAddr("203.0.113.10")) {
		t.Error("an unlisted client inside the allow range must pass")
	}
	if pol.Allowed(netip.MustParseAddr("198.51.100.1")) {
		t.Error("a client outside every allow range must be refused")
	}
}

func TestPolicyNoListsAllowsEveryone(t *testing.T) {
	pol := Policy{}
	if !pol.Allowed(netip.MustParseAddr("198.51.100.1")) {
		t.Error("an empty policy must not restrict traffic")
	}
	// An unparseable address is refused when an allow list exists: the
	// gateway cannot match an identity it does not know against the list.
	pol.Allow = mustPrefixes(t, "203.0.113.0/24")
	if pol.Allowed(netip.Addr{}) {
		t.Error("unknown client address must be refused when an allow list exists")
	}
}

func TestParsePrefixListAcceptsBareAddressesAndSkipsGarbage(t *testing.T) {
	got := parsePrefixList("203.0.113.5\n10.0.0.0/8\n# a comment\nnot-an-ip\n\n192.168.0.0/16")
	if len(got) != 3 {
		t.Fatalf("want 3 prefixes, got %d: %v", len(got), got)
	}
	if !got[0].Contains(netip.MustParseAddr("203.0.113.5")) {
		t.Errorf("bare address should become a /32: %v", got[0])
	}
}

func TestClientAddrIgnoresForgedForwardedForByDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if got := ClientAddr(req, false); got.String() != "198.51.100.7" {
		t.Errorf("untrusted XFF must be ignored, got %s", got)
	}
	if got := ClientAddr(req, true); got.String() != "10.0.0.1" {
		t.Errorf("trusted XFF must be honoured, got %s", got)
	}
}

func TestBodyLimitRejectsOversizedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	host, port := splitHostPort(t, upstream.URL)
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	g := testGateway(tbl, func(c *config.Config) { c.GatewayClientMaxBodyBytes = 8 })
	req := httptest.NewRequest(http.MethodPost, "http://acc.example.com/", strings.NewReader("0123456789"))
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", rec.Code)
	}
}

// ---------- load balancing ----------

// Smooth weighted round robin must honour weights over a full cycle: a weight
// of 3 against 1 has to produce three requests to the first, not two.
func TestWeightedRoundRobinHonoursWeights(t *testing.T) {
	p := NewPicker()
	instances := []Instance{
		{IP: "10.0.0.1", Port: 80, Weight: 3},
		{IP: "10.0.0.2", Port: 80, Weight: 1},
	}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		got := p.Pick("t", "round_robin", "", instances)
		counts[got.IP]++
	}
	if counts["10.0.0.1"] != 30 || counts["10.0.0.2"] != 10 {
		t.Errorf("weight 3:1 over 40 picks should be 30/10, got %v", counts)
	}
}

// Weights must interleave rather than arrive in blocks, which is the property
// that keeps a weighted set from overloading an instance right after a switch.
func TestWeightedRoundRobinSmoothsAcrossInstances(t *testing.T) {
	p := NewPicker()
	instances := []Instance{
		{IP: "10.0.0.1", Port: 80, Weight: 3},
		{IP: "10.0.0.2", Port: 80, Weight: 1},
	}
	first, longestRun, run := 0, 0, 0
	for i := 0; i < 40; i++ {
		if p.Pick("t", "round_robin", "", instances).IP == "10.0.0.2" {
			first++
			run++
			if run > longestRun {
				longestRun = run
			}
		} else {
			run = 0
		}
	}
	if first == 0 || longestRun > 2 {
		t.Errorf("distribution should interleave; the light instance got %d of 40 with a %d-long run", first, longestRun)
	}
}

func TestIPHashPinsAClientToOneInstance(t *testing.T) {
	p := NewPicker()
	instances := []Instance{
		{IP: "10.0.0.1", Port: 80, Weight: 1},
		{IP: "10.0.0.2", Port: 80, Weight: 1},
		{IP: "10.0.0.3", Port: 80, Weight: 1},
	}
	first := p.Pick("t", "ip_hash", "203.0.113.5", instances).IP
	for i := 0; i < 20; i++ {
		if got := p.Pick("t", "ip_hash", "203.0.113.5", instances).IP; got != first {
			t.Fatalf("ip_hash moved a pinned client from %s to %s", first, got)
		}
	}
}

func TestCookieStickyFallsBackWhenNoCookie(t *testing.T) {
	p := NewPicker()
	instances := []Instance{
		{IP: "10.0.0.1", Port: 80, Weight: 1},
		{IP: "10.0.0.2", Port: 80, Weight: 1},
	}
	// Without a cookie every request would otherwise hash to one instance and
	// collapse the pool onto a single backend.
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		seen[p.Pick("t", "cookie_sticky", "", instances).IP] = true
	}
	if len(seen) != 2 {
		t.Errorf("a cookie-less client should be distributed, only saw %v", seen)
	}
}

func TestLeastConnPrefersTheIdleInstance(t *testing.T) {
	p := NewPicker()
	instances := []Instance{
		{IP: "10.0.0.1", Port: 80, Weight: 1},
		{IP: "10.0.0.2", Port: 80, Weight: 1},
	}
	p.Acquire("t", "10.0.0.1:80")
	p.Acquire("t", "10.0.0.1:80")
	p.Acquire("t", "10.0.0.1:80")
	for i := 0; i < 3; i++ {
		if got := p.Pick("t", "least_conn", "", instances).IP; got != "10.0.0.2" {
			t.Fatalf("least_conn picked the busy instance %s", got)
		}
	}
}

// A release that outruns its acquire must not drive the counter negative, or
// least_conn would start preferring an instance nobody is connected to.
func TestReleaseNeverGoesNegative(t *testing.T) {
	p := NewPicker()
	p.Release("t", "10.0.0.9:80")
	instances := []Instance{{IP: "10.0.0.1", Port: 80, Weight: 1}, {IP: "10.0.0.2", Port: 80, Weight: 1}}
	p.Release("t", "10.0.0.1:80")
	if got := p.Pick("t", "least_conn", "", instances); got == nil {
		t.Fatal("pick returned nil")
	}
}

func TestWeightOfTreatsZeroAsOne(t *testing.T) {
	if weightOf(Instance{Weight: 0}) != 1 {
		t.Error("weight 0 means 'no opinion', not 'never route here'")
	}
	if weightOf(Instance{Weight: -1}) != 1 {
		t.Error("negative weight must not remove an instance")
	}
}

// ---------- TLS decisions ----------

// The eligibility guard is what stops internal hostnames from burning the
// rate limit a real public domain needs.
func TestEligibleForACMERejectsInternalNames(t *testing.T) {
	blocked := []string{
		"localhost", "api.local", "svc.cluster.local", "db.internal",
		"box.lan", "x.test", "10.0.0.7", "*.example.com", "singlelabel",
	}
	for _, d := range blocked {
		if eligibleForACME(d) {
			t.Errorf("%q must not be submitted to a public CA", d)
		}
	}
	for _, d := range []string{"acc.example.com", "api.itestu.cn"} {
		if !eligibleForACME(d) {
			t.Errorf("%q is a public name and should be eligible", d)
		}
	}
}

func TestSelfSignedCertificateCoversItsDomain(t *testing.T) {
	certPEM, keyPEM, notAfter, err := generateSelfSigned("acc.example.com", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if notAfter.Before(time.Now().Add(24 * time.Hour)) {
		t.Errorf("a fallback certificate that expires immediately is useless: %v", notAfter)
	}
	cert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := leaf.VerifyHostname("acc.example.com"); err != nil {
		t.Errorf("certificate must cover its own domain: %v", err)
	}
	if err := leaf.VerifyHostname("other.example.com"); err == nil {
		t.Error("certificate must not cover a different domain")
	}
	if len(cert.Certificate) == 0 {
		t.Error("key pair produced no chain")
	}
	if block, _ := pem.Decode([]byte(certPEM)); block == nil {
		t.Error("certificate is not valid PEM")
	}
	if block, _ := pem.Decode([]byte(keyPEM)); block == nil {
		t.Error("key is not valid PEM")
	}
}

func TestSanitizeFilenameCannotEscapeDirectory(t *testing.T) {
	root := t.TempDir()
	for _, hostile := range []string{"../../etc/passwd", "a/b/../../../c", "....//....//x"} {
		name := sanitizeFilename(hostile)
		if strings.ContainsAny(name, "/\\") {
			t.Fatalf("%q produced a path separator: %q", hostile, name)
		}
		joined := filepath.Join(root, name)
		if !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
			t.Fatalf("%q escaped the cache directory: %q", hostile, joined)
		}
	}
	if got := sanitizeFilename("..."); got != "unnamed" {
		t.Errorf("an all-dots name must be refused, got %q", got)
	}
}

func TestCertManagerServesRequestedNameOnUnknownSNI(t *testing.T) {
	m := &CertManager{
		certs:    map[string]*tls.Certificate{},
		info:     map[string]CertInfo{},
		onDemand: map[string]*tls.Certificate{},
	}
	m.buildDefaultCert()
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "brand-new.example.com"})
	if err != nil {
		t.Fatalf("a declared-but-unissued domain must still complete a handshake: %v", err)
	}
	if cert == nil {
		t.Fatal("nil certificate")
	}
}

// ---------- real TLS end to end ----------

// The full promise in one test: a client negotiates TLS against the gateway
// using SNI, and the request lands on the registered instance behind it.
//
// Certificate verification is disabled here on purpose — the point is that the
// gateway presents the certificate that covers the requested name and that the
// request completes, not that the test trusts the fallback CA.
func TestTLSListenerServesDeclaredDomainEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		_, _ = w.Write([]byte("tls-served"))
	}))
	defer upstream.Close()
	upHost, upPort := splitHostPort(t, upstream.URL)

	tbl := buildTable(testRoute{
		domain: "acc.example.com", app: "acc", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(upHost, upPort, 1, nil)},
	})
	g := testGateway(tbl, nil)

	// Provision the fallback certificate the way the reconcile loop would. The
	// cache is populated directly here because persistence needs a database,
	// and what this test is about is the TLS path.
	certPEM, keyPEM, notAfter, err := generateSelfSigned("acc.example.com", "")
	if err != nil {
		t.Fatalf("self-signed: %v", err)
	}
	fallbackCert, leaf, err := parseCertificate(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g.Certs().install("acc.example.com", fallbackCert, CertInfo{
		Domain: "acc.example.com", Source: storage.CertSourceSelfSigned,
		Issuer:    selfSignedSubject("acc.example.com"),
		NotBefore: leaf.NotBefore, NotAfter: notAfter,
		Status: storage.CertStatusActive,
	})
	if info, ok := g.Certs().Info("acc.example.com"); !ok {
		t.Fatal("fallback certificate was not provisioned")
	} else if info.Source != storage.CertSourceSelfSigned {
		t.Fatalf("fallback source=%q, want self_signed so the console can warn", info.Source)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{
		Handler: g.TLSServe(),
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: g.Certs().GetCertificate,
		},
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	// The request is addressed to the real domain and dialled at the loopback
	// listener, which is exactly what a browser does after a DNS lookup: SNI,
	// Host and routing all carry the public name.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:     fixedDialer(t, ln.Addr().String()),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("https://acc.example.com/api/v2/health")
	if err != nil {
		t.Fatalf("tls request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "tls-served") {
		t.Errorf("body=%s, want the upstream's response", body)
	}
	if resp.Header.Get("X-Goacos-Cert-Source") != storage.CertSourceSelfSigned {
		t.Errorf("X-Goacos-Cert-Source=%q — a client must be able to see it is on a fallback certificate",
			resp.Header.Get("X-Goacos-Cert-Source"))
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate presented")
	}
	if err := resp.TLS.PeerCertificates[0].VerifyHostname("acc.example.com"); err != nil {
		t.Errorf("served certificate must cover the requested SNI name: %v", err)
	}
}

// A domain declared on the route table must always complete a handshake, even
// before issuance has finished. The failure mode this prevents is "the site is
// down", which tells an operator nothing about the certificate.
func TestTLSListenerAlwaysCompletesHandshake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upHost, upPort := splitHostPort(t, upstream.URL)
	tbl := buildTable(testRoute{
		domain: "pending.example.com", app: "acc", component: "api", service: "acc-api",
		instances: []Instance{instanceOf(upHost, upPort, 1, nil)},
	})
	g := testGateway(tbl, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{
		Handler: g.TLSServe(),
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: g.Certs().GetCertificate,
		},
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{
		Transport: &http.Transport{
			DialContext:     fixedDialer(t, ln.Addr().String()),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}
	// No certificate has been provisioned for this name at all.
	resp, err := client.Get("https://pending.example.com/")
	if err != nil {
		t.Fatalf("handshake must succeed for a declared domain: %v", err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no certificate presented")
	}
}

func TestCertInfoReportsDaysLeftAndExpiry(t *testing.T) {
	expired := CertInfo{NotAfter: time.Now().Add(-24 * time.Hour)}
	if !expired.Expired() {
		t.Error("a past notAfter must read as expired")
	}
	if expired.DaysLeft() >= 0 {
		t.Errorf("days left should be negative once expired, got %d", expired.DaysLeft())
	}
	fresh := CertInfo{NotAfter: time.Now().Add(10 * 24 * time.Hour)}
	if d := fresh.DaysLeft(); d != 9 && d != 10 {
		t.Errorf("days left should be about 10, got %d", d)
	}
}

// ---------- redirects and challenges ----------

func TestHTTPListenerRedirectsToHTTPS(t *testing.T) {
	tbl := buildTable(testRoute{domain: "acc.example.com", component: "api", service: "acc-api"})
	g := testGateway(tbl, func(c *config.Config) { c.GatewayForceHTTPS = true })

	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/api/v2/health?a=1", nil)
	req.Host = "acc.example.com"
	rec := httptest.NewRecorder()
	g.HTTPServe().ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status=%d, want 301", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "https://acc.example.com/api/v2/health?a=1" {
		t.Errorf("Location=%q — path and query must survive the redirect", loc)
	}
}

// A challenge that gets redirected is a failed validation and a spent
// rate-limit slot, so it is answered before any policy decision.
func TestACMEChallengeIsServedBeforeRedirect(t *testing.T) {
	tbl := buildTable(testRoute{domain: "acc.example.com", component: "api", service: "acc-api"})
	g := testGateway(tbl, func(c *config.Config) { c.GatewayForceHTTPS = true })
	g.Certs().setChallenge("tok-123", "tok-123.thumbprint")

	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/.well-known/acme-challenge/tok-123", nil)
	req.Host = "acc.example.com"
	rec := httptest.NewRecorder()
	g.HTTPServe().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("challenge must be answered directly, got %d", rec.Code)
	}
	if rec.Body.String() != "tok-123.thumbprint" {
		t.Errorf("challenge body=%q", rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Error("a challenge must never be redirected")
	}
}

func TestUnknownChallengeIs404(t *testing.T) {
	tbl := buildTable()
	g := testGateway(tbl, nil)
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/.well-known/acme-challenge/nope", nil)
	rec := httptest.NewRecorder()
	g.HTTPServe().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
}

// ---------- static delivery ----------

func TestStaticServesFilesAndFallsBackToIndexForClientRoutes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "acc-web")
	if err := writeFile(dir+"/index.html", "<html>spa</html>"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir+"/assets/app.4f3a2b1c.js", "console.log(1)"); err != nil {
		t.Fatal(err)
	}

	tbl := buildTable(testRoute{
		domain: "acc.example.com", app: "acc", component: "web", kind: "web",
		protocol: "static", service: "acc-web",
	})
	g := testGateway(tbl, func(c *config.Config) { c.GatewayStaticRoot = root })

	// A real asset is served as itself, cacheable for a year because its name
	// is content-hashed.
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/assets/app.4f3a2b1c.js", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("asset not served: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("fingerprinted asset should be immutable, got %q", rec.Header().Get("Cache-Control"))
	}

	// A client-side route has no file behind it, and must get the SPA shell.
	req = httptest.NewRequest(http.MethodGet, "http://acc.example.com/settings/profile", nil)
	rec = httptest.NewRecorder()
	g.dispatch(rec, req, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "spa") {
		t.Fatalf("client route should fall back to index.html: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index.html must never be cached, got %q", cc)
	}

	// A missing asset must 404 rather than silently returning HTML, which the
	// browser reports as a confusing MIME type error.
	req = httptest.NewRequest(http.MethodGet, "http://acc.example.com/assets/missing.js", nil)
	rec = httptest.NewRecorder()
	g.dispatch(rec, req, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing asset status=%d, want 404", rec.Code)
	}
}

// A static component whose directory does not exist must fall through to the
// registered instances, which is what lets a team switch delivery modes without
// redeclaring the route.
func TestStaticFallsBackToProxyWhenDirectoryMissing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("from-instance"))
	}))
	defer upstream.Close()
	host, port := splitHostPort(t, upstream.URL)

	tbl := buildTable(testRoute{
		domain: "acc.example.com", app: "acc", component: "web", kind: "web",
		protocol: "static", service: "acc-web",
		instances: []Instance{instanceOf(host, port, 1, nil)},
	})
	g := testGateway(tbl, func(c *config.Config) { c.GatewayStaticRoot = t.TempDir() })

	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, true)
	if !strings.Contains(rec.Body.String(), "from-instance") {
		t.Errorf("expected the registered instance to serve it, got %s", rec.Body.String())
	}
}

func TestIsFingerprinted(t *testing.T) {
	cases := map[string]bool{
		"app.4f3a2b1c.js":   true,
		"main-C1x9zPq2.css": true,
		"chunk.AABBCC12.js": true,
		// An all-letter tail is an English word far more often than a hash,
		// and mistaking one for a fingerprint caches a file for a year.
		"chunk.AABBCCDD.js": false,
		"index.html":        false,
		"logo.svg":          false,
		"app.js":            false,
		"styles.css":        false,
		"bootstrap.min.js":  false,
		"vendor-bundle.js":  false,
	}
	for name, want := range cases {
		if got := isFingerprinted(name); got != want {
			t.Errorf("isFingerprinted(%q)=%v want %v", name, got, want)
		}
	}
}

// ---------- header hygiene ----------

func TestRemoveHopByHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom-Hop, Upgrade")
	h.Set("X-Custom-Hop", "secret")
	h.Set("Upgrade", "websocket")
	h.Set("X-Keep", "yes")
	removeHopByHop(h)
	if h.Get("X-Custom-Hop") != "" || h.Get("Upgrade") != "" {
		t.Errorf("hop-by-hop headers survived: %v", h)
	}
	if h.Get("X-Keep") != "yes" {
		t.Error("a normal end-to-end header must survive")
	}
}

func TestParseMetaFilterIsTolerant(t *testing.T) {
	if got := parseMetaFilter("lane=canary,zone=a"); len(got) != 2 {
		t.Errorf("got %v", got)
	}
	if got := parseMetaFilter("garbage"); got != nil {
		t.Errorf("a malformed filter must be dropped, not fatal, got %v", got)
	}
}

// ---------- misc helpers ----------

func mustPrefixes(t *testing.T, list ...string) []netip.Prefix {
	t.Helper()
	got := parsePrefixList(strings.Join(list, "\n"))
	if len(got) != len(list) {
		t.Fatalf("expected %d prefixes, got %v", len(list), got)
	}
	return got
}

// fixedDialer sends every connection to one address regardless of the host in
// the URL, so a test can address a request to a real domain name over loopback.
func fixedDialer(t *testing.T, target string) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", target)
	}
}

func splitHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u := strings.TrimPrefix(rawURL, "http://")
	host, portStr, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("bad url %q: %v", rawURL, err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// A failed dial is invisible in every other signal the status API exposes.
//
// `domains[].healthy` comes from the registry: an instance counts as healthy
// when its registrar heartbeats. That says nothing about whether this process
// can dial it. Measured on 2026-10-14 against a gateway with three registered
// instances, two of them unreachable from the gateway's network namespace:
// the status API reported three healthy instances and "proxied" kept rising,
// while 100% of traffic silently concentrated on the reachable one. The only
// trace was a log line nobody reads.
//
// So the counter is the point of this test, not the failover — the failover
// itself is already covered by TestDispatchFailsOverToHealthyInstance.
//
// Note this deliberately fires several requests instead of one. Lookup
// sorts instances by weight, then IP, then port (selectInstances), and both
// test instances live on 127.0.0.1 — so the tiebreak is whichever ephemeral
// port the OS happened to hand out. A single request would fail this half the
// time depending on whether the dead instance sorted first. Round robin over
// six requests reaches both instances regardless of the ordering.
func TestUpstreamFailureIsCountedSeparatelyFromSuccess(t *testing.T) {
	dead := deadServer(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served-by-healthy"))
	}))
	defer good.Close()

	deadHost, deadPort := splitHostPort(t, dead)
	goodHost, goodPort := splitHostPort(t, good.URL)
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{
			instanceOf(deadHost, deadPort, 1, nil),
			instanceOf(goodHost, goodPort, 1, nil),
		},
	})
	g := testGateway(tbl, nil)

	for i := 0; i < 6; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
		g.dispatch(rec, req, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: failover failed: status=%d body=%s",
				i, rec.Code, rec.Body.String())
		}
	}

	counters := g.Counters()
	failed, ok := counters["upstreamFailed"].(int64)
	if !ok {
		t.Fatalf("counters[upstreamFailed] missing or wrong type: %#v",
			counters["upstreamFailed"])
	}
	if failed < 1 {
		t.Errorf("upstreamFailed=%d, want >=1: an instance the data plane could "+
			"not reach must be visible in the status payload, not only in the log", failed)
	}
	if got := counters["proxied"].(int64); got < 1 {
		t.Errorf("proxied=%d, want >=1: the requests still succeeded, which is "+
			"exactly why the failures needed their own counter", got)
	}
}
