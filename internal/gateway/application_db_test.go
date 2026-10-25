package gateway

// Full-application routing: one application, a frontend component and a backend
// component, on one domain.
//
// Why this file exists separately from routetable_pg_test.go
//
// That file proves a *service* reaches the data plane. The goal this subsystem
// actually has is bigger — "不只是支持 nacos 类型的服务，还要支持前端后端的
// 完整应用" — and nothing asserted that. A full application is not a bigger
// version of the same thing: it is two kinds of component sharing a domain, and
// the interesting failures are about the boundary between them.
//
// The boundary that matters
//
// `/` is the frontend and `/api` is the backend. When the backend has no healthy
// instance, the request to `/api/...` must fail closed. The tempting wrong
// behaviour is to fall back to the shorter prefix and hand the caller the SPA's
// index.html with a 200 — which turns an API outage into "the frontend is
// broken but the docs say to clear your cache".
//
// Lookup returns the first matching entry's target even when that target has
// zero instances, so the fail-closed property is real today. It is also exactly
// the kind of property that a future "be more forgiving" change would quietly
// remove, and no existing test would notice. So it is asserted here by name.
//
// What this file does NOT claim
//
// `kind` is carried through the model and into the API response, but the data
// path never branches on it. Declaring a component `kind='static'` does not make
// goacos serve any files: a static component still resolves through its
// registry service name, so it has instances or it has nothing. Frontend
// delivery therefore still requires the frontend to be a real, registered,
// health-checked endpoint (an nginx serving the built assets registers fine).
// TestAGatewayFailsClosedRatherThanServingAnUnreachableFrontend pins that
// boundary so nobody discovers it by getting 503s in production.

import (
	"context"
	"os"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

const (
	appTestDomain = "shop.example.com"
	appTestApp    = "shop"

	// The component names below are chosen so that the FRONTEND's name sorts
	// before the BACKEND's ("app" < "backend").
	//
	// That is not cosmetic. The route table sorts by longest prefix first and
	// only then by component name, so a table that had lost the length ordering
	// would still resolve correctly whenever the names happen to agree with
	// path specificity — which they did at first ("frontend" < "backend" is
	// *false*, but "api" < "web" is *true*, and that naming passed anyway).
	// A mutation removing the length sort was caught by nothing in this package.
	//
	// With these names, a name-ordered table puts the catch-all "/" first, so a
	// request to /api/orders matches the frontend — which is exactly the
	// cross-contamination these tests exist to rule out.
	appTestFrontendCmp = "app"
	appTestBackendCmp  = "backend"

	appTestGroup   = "DEFAULT_GROUP"
	appTestCluster = "DEFAULT"
)

// declareFullApplication wires the model an operator writes when one product
// has a frontend and an API behind it: one application, two components, two
// route prefixes on the same domain.
//
// Note that this deliberately does not reuse declare() from the sibling file:
// that helper hardcodes a single `/` route and one component, which is the
// "one nacos service" case, not this one.
func declareFullApplication(t *testing.T, store *storage.Store) {
	t.Helper()
	ctx := context.Background()
	ns := gwTestNamespace

	if _, err := store.UpsertApplication(ctx, storage.Application{
		NamespaceId: ns, Name: appTestApp, DisplayName: "Shop", Env: "test",
	}); err != nil {
		t.Fatalf("upsert application: %v", err)
	}

	components := []storage.ApplicationComponent{
		{
			NamespaceId: ns, AppName: appTestApp, Name: appTestFrontendCmp,
			Kind: "static", Protocol: "http", ServiceName: "shop-web",
			GroupName: appTestGroup, ClusterName: appTestCluster,
			Weight: 1, LBAlgorithm: "round_robin",
		},
		{
			NamespaceId: ns, AppName: appTestApp, Name: appTestBackendCmp,
			Kind: "backend", Protocol: "http", ServiceName: "shop-api",
			GroupName: appTestGroup, ClusterName: appTestCluster,
			Weight: 1, LBAlgorithm: "round_robin",
		},
	}
	for _, c := range components {
		if _, err := store.UpsertComponent(ctx, c); err != nil {
			t.Fatalf("upsert component %s: %v", c.Name, err)
		}
	}

	// Order below is the order an operator would plausibly type them: the
	// catch-all first. It must not matter — the route table sorts by prefix
	// length — and declaring it this way is what makes that a real assertion
	// rather than a restatement of the sort implementation.
	routes := []storage.RouteBinding{
		{NamespaceId: ns, Domain: appTestDomain, PathPrefix: "/",
			AppName: appTestApp, ComponentName: appTestFrontendCmp},
		{NamespaceId: ns, Domain: appTestDomain, PathPrefix: "/api",
			AppName: appTestApp, ComponentName: appTestBackendCmp},
	}
	for _, r := range routes {
		if err := store.UpsertRoute(ctx, r); err != nil {
			t.Fatalf("upsert route %s: %v", r.PathPrefix, err)
		}
	}

	if _, err := store.UpsertVHost(ctx, storage.VHostPolicy{
		NamespaceId: ns, Domain: appTestDomain,
	}); err != nil {
		t.Fatalf("upsert vhost: %v", err)
	}
}

func registerAppInstance(t *testing.T, store *storage.Store, service, ip string, port uint32) {
	t.Helper()
	_, err := store.RegisterInstance(context.Background(), storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: appTestGroup,
		ServiceName: service, ClusterName: appTestCluster,
		IP: ip, Port: port, Weight: 1, Enabled: true, Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("register %s at %s:%d: %v", service, ip, port, err)
	}
}

// TestFullApplicationRoutesEachPrefixToItsOwnComponent is the positive case:
// the frontend and the backend are both live, and each prefix reaches its own
// component's instances and nobody else's.
//
// The cross-contamination assertion is the point. Both components live under
// one application and one domain; if Lookup ever resolved by application rather
// than by prefix, the API would be served by the web host and every assertion
// here would still be "passing" on the frontend half alone.
func TestFullApplicationRoutesEachPrefixToItsOwnComponent(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declareFullApplication(t, store)

	registerAppInstance(t, store, "shop-web", "100.106.175.10", 80)
	registerAppInstance(t, store, "shop-web", "100.106.175.11", 80)
	registerAppInstance(t, store, "shop-api", "100.106.175.20", 8080)
	registerAppInstance(t, store, "shop-api", "100.106.175.21", 8080)

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	front := tbl.Lookup(appTestDomain, "/index.html", nil)
	if front == nil {
		t.Fatalf("no target for the frontend prefix; domain %s is declared and its instances are registered",
			appTestDomain)
	}
	if front.ComponentName != appTestFrontendCmp {
		t.Errorf("/ resolved to component %q, want %q", front.ComponentName, appTestFrontendCmp)
	}
	if got := sortedAddrs(front.Instances); len(got) != 2 {
		t.Errorf("frontend routed to %v, want both web instances — a full application must balance across them", got)
	}

	back := tbl.Lookup(appTestDomain, "/api/orders", nil)
	if back == nil {
		t.Fatalf("no target for /api; its component and instances are both registered")
	}
	if back.ComponentName != appTestBackendCmp {
		t.Errorf("/api resolved to component %q, want %q", back.ComponentName, appTestBackendCmp)
	}
	if got := sortedAddrs(back.Instances); len(got) != 2 {
		t.Errorf("/api routed to %v, want both api instances", got)
	}
	for _, addr := range sortedAddrs(back.Instances) {
		if addr == "100.106.175.10:80" || addr == "100.106.175.11:80" {
			t.Errorf("/api was served by a frontend instance %s", addr)
		}
	}

	// A deeper prefix under the backend must stay on the backend, not climb
	// back up to the shorter /api rule or down to /.
	if deep := tbl.Lookup(appTestDomain, "/api/v2/orders/42", nil); deep == nil || deep.ComponentName != appTestBackendCmp {
		t.Errorf("/api/v2/orders/42 resolved to %v, want the backend component", deep)
	}
}

// TestAGatewayFailsClosedRatherThanServingAnUnreachableFrontend is the boundary
// assertion: when the backend has nothing to route to, /api fails closed and
// does NOT fall back to the frontend prefix.
//
// "Fails closed" here means Lookup still returns a target — with zero instances —
// rather than nil. That distinction is load-bearing for the data plane: nil is a
// 404 ("no such route"), while an empty instance list is a 503 ("the route
// exists and nothing is behind it"). Those are different pages of the runbook.
func TestAGatewayFailsClosedRatherThanServingAnUnreachableFrontend(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declareFullApplication(t, store)

	// Only the frontend is up. The backend component is declared and routed,
	// with nothing registered behind it — an ordinary deployment accident.
	registerAppInstance(t, store, "shop-web", "100.106.175.10", 80)
	registerAppInstance(t, store, "shop-web", "100.106.175.11", 80)

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	back := tbl.Lookup(appTestDomain, "/api/orders", nil)
	if back == nil {
		t.Fatalf("/api returned nil; the route IS declared, so the data plane must see an " +
			"empty target and answer 503, not 404")
	}
	if back.ComponentName != appTestBackendCmp {
		t.Fatalf("/api fell through to component %q — an API outage must not be served "+
			"the frontend's index.html with a 200", back.ComponentName)
	}
	if len(back.Instances) != 0 {
		t.Errorf("/api has %d instances (%v), want none", len(back.Instances), sortedAddrs(back.Instances))
	}

	// The frontend must be unaffected: one component being down is not a
	// reason to take the whole application offline.
	front := tbl.Lookup(appTestDomain, "/index.html", nil)
	if front == nil || front.ComponentName != appTestFrontendCmp {
		t.Fatalf("frontend broke when the backend was empty: %v", front)
	}
	if got := sortedAddrs(front.Instances); len(got) != 2 {
		t.Errorf("frontend routed to %v, want both instances — the backend outage must not shrink the frontend pool", got)
	}
}

// TestAnUnreachableFrontendIsAlsoJustAnEmptyTarget pins the other half of the
// boundary: `kind` is metadata, not behaviour.
//
// A component declared kind='static' with nothing registered behind it must not
// make goacos serve files, and must not 404 either — it has to fail the same way
// any other unreachable component does. Written as its own case so that the day
// someone adds real static serving, this assertion is the line they have to
// consciously cross rather than something that quietly stops holding.
func TestAnUnreachableFrontendIsAlsoJustAnEmptyTarget(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declareFullApplication(t, store)

	// Nothing at all registered: neither shop-web nor shop-api.
	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	front := tbl.Lookup(appTestDomain, "/", nil)
	if front == nil {
		t.Fatal("/ returned nil with no instances; declared routes must resolve to an empty target")
	}
	if front.Kind != "static" {
		t.Errorf("component kind = %q, want it carried through as %q", front.Kind, "static")
	}
	if len(front.Instances) != 0 {
		t.Errorf("a static component with nothing registered resolved to %v — goacos does not serve files today",
			sortedAddrs(front.Instances))
	}
}

// TestFullApplicationIsNotAStubFixture keeps the other three cases honest: they
// pass on a table that resolved nothing. Every assertion above is a positive
// claim about which component and which instances, so a fixture that registered
// too little would turn them all green for the wrong reason.
func TestFullApplicationIsNotAStubFixture(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declareFullApplication(t, store)

	if os.Getenv("GOACOS_TEST_PG_DSN") == "" {
		t.Fatal("fixture requires a real database; openPGStore should have skipped instead")
	}

	registerAppInstance(t, store, "shop-web", "100.106.175.10", 80)
	registerAppInstance(t, store, "shop-api", "100.106.175.20", 8080)

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	front := tbl.Lookup(appTestDomain, "/", nil)
	back := tbl.Lookup(appTestDomain, "/api", nil)
	if front == nil || back == nil {
		t.Fatalf("fixture produced no targets at all (front=%v back=%v)", front, back)
	}
	if len(front.Instances) != 1 || len(back.Instances) != 1 {
		t.Fatalf("fixture registered %d frontend and %d backend instances, want 1 and 1",
			len(front.Instances), len(back.Instances))
	}
	if front.ComponentName == back.ComponentName {
		t.Fatalf("both prefixes resolved to component %q — the fixture cannot tell the two apart",
			front.ComponentName)
	}
}
