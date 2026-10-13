package gateway

// Real-database tests for the seam between the registry and the data plane.
//
// Every other test in this package builds its routing table by hand:
//
//	tbl := buildTable(testRoute{... instances: []Instance{instanceOf(...)}})
//
// That is fine for testing dispatch, load balancing and header rewriting —
// but it means Table.Refresh, the one function that actually reads instances
// out of goacos's naming storage, has never been executed by any test. If its
// query were wrong — wrong namespace, wrong group, a cluster filter that never
// matches — the gateway would resolve every domain to "no healthy instance",
// and the suite would still be green, because the hand-built table bypasses the
// very code that broke.
//
// This file closes that. It writes through the storage API exactly as the SDK
// does on a service's behalf, calls Refresh, and asserts the table routes to
// what was actually written.
//
// It needs a real database: goacos's storage speaks MySQL and PostgreSQL and has
// no in-memory dialect, so there is nothing to fake here that would not be
// another hand-built thing. Skipped unless the DSN is set:
//
//	GOACOS_TEST_PG_DSN='postgres://user:pass@127.0.0.1:5432/goacostest?sslmode=disable' \
//	  go test ./internal/gateway/ -run TestRefresh -v
//
// Apply the schema first:
//
//	psql -d goacostest -f internal/storage/schema/schema.postgres.sql
//	psql -d goacostest -f internal/storage/schema/migrations/002_application.postgres.sql
//	psql -d goacostest -f internal/storage/schema/migrations/003_gateway.postgres.sql

import (
	"context"
	"os"
	"testing"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

const (
	gwTestNamespace = "public"
	gwTestApp       = "acc"
	gwTestDomain    = "acc.example.com"
)

// openPGStore connects to the test database and truncates the tables this test
// writes, so repeated runs do not inherit each other's rows.
func openPGStore(t *testing.T) *storage.Store {
	t.Helper()
	dsn := os.Getenv("GOACOS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GOACOS_TEST_PG_DSN is not configured")
	}
	cfg := &config.Config{DBType: "postgres", DBDSN: dsn}
	store, err := storage.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.DB.ExecContext(context.Background(),
		`TRUNCATE naming_instance, naming_service, route_binding, gateway_vhost,
		 application_component, application`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store
}

// declare wires the application model the way an operator would: an
// application, a component that names the registry service, and a domain route.
func declare(t *testing.T, store *storage.Store, group, service string) {
	t.Helper()
	ctx := context.Background()
	ns := gwTestNamespace

	if _, err := store.UpsertApplication(ctx, storage.Application{
		NamespaceId: ns, Name: gwTestApp, DisplayName: "ACC", Env: "test",
	}); err != nil {
		t.Fatalf("upsert application: %v", err)
	}
	if _, err := store.UpsertComponent(ctx, storage.ApplicationComponent{
		NamespaceId: ns, AppName: gwTestApp, Name: "api", Kind: "backend",
		Protocol: "http", ServiceName: service, GroupName: group,
		ClusterName: "DEFAULT", Weight: 1, LBAlgorithm: "round_robin",
	}); err != nil {
		t.Fatalf("upsert component: %v", err)
	}
	if err := store.UpsertRoute(ctx, storage.RouteBinding{
		NamespaceId: ns, Domain: gwTestDomain, PathPrefix: "/",
		AppName: gwTestApp, ComponentName: "api",
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}
	if _, err := store.UpsertVHost(ctx, storage.VHostPolicy{
		NamespaceId: ns, Domain: gwTestDomain,
	}); err != nil {
		t.Fatalf("upsert vhost: %v", err)
	}
}

// TestRefreshRoutesToWhatTheRegistryActuallyHolds is the end-to-end promise of
// this whole subsystem: a service registers an instance, and the gateway routes
// traffic to it. Everything in between is exercised for real.
func TestRefreshRoutesToWhatTheRegistryActuallyHolds(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declare(t, store, "DEFAULT_GROUP", "acc-api")

	created, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
		ServiceName: "acc-api", ClusterName: "DEFAULT",
		IP: "100.106.175.168", Port: 8080, Weight: 1, Enabled: true, Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("register instance: %v", err)
	}
	if !created {
		t.Fatal("first registration should report created=true")
	}

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	target := tbl.Lookup(gwTestDomain, "/api/v2/health", nil)
	if target == nil {
		t.Fatalf("Refresh produced no target for %s; the route resolved to nothing "+
			"even though the instance is in the registry", gwTestDomain)
	}
	if got := sortedAddrs(target.Instances); len(got) != 1 || got[0] != "100.106.175.168:8080" {
		t.Errorf("routed to %v, want the registered 100.106.175.168:8080", got)
	}
	if target.ServiceName != "acc-api" || target.GroupName != "DEFAULT_GROUP" {
		t.Errorf("component lost its registry identity: service=%q group=%q",
			target.ServiceName, target.GroupName)
	}
	if !tbl.KnownDomain(gwTestDomain) {
		t.Error("the declared domain should be known after Refresh")
	}
}

// TestRefreshFollowsTheRegistryRatherThanASnapshot is the reason a route table
// exists at all: an instance that stops beating must disappear from routing, and
// one that appears must start being routed to. A gateway that caches forever
// serves dead hosts until someone restarts it.
func TestRefreshFollowsTheRegistryRatherThanASnapshot(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declare(t, store, "DEFAULT_GROUP", "acc-api")

	upsert := func(ip string, port uint32) {
		t.Helper()
		if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
			NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
			ServiceName: "acc-api", ClusterName: "DEFAULT",
			IP: ip, Port: port, Weight: 1, Enabled: true, Ephemeral: true,
		}); err != nil {
			t.Fatalf("register %s:%d: %v", ip, port, err)
		}
	}
	remove := func(ip string, port uint32) {
		t.Helper()
		if _, err := store.DeleteInstance(ctx, gwTestNamespace, "DEFAULT_GROUP",
			"acc-api", "DEFAULT", ip, port); err != nil {
			t.Fatalf("deregister %s:%d: %v", ip, port, err)
		}
	}

	tbl := NewTable()

	// Empty registry: the domain is declared, but there is nowhere to send it.
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh empty: %v", err)
	}
	// A declared domain with nothing behind it resolves to a target with no
	// instances — which the data plane turns into 503, not 404. That
	// distinction is the point: "we know this domain but it has nowhere to
	// send traffic" and "nobody ever declared this domain" are different
	// answers, and conflating them turns a capacity problem into a routing
	// bug report.
	if target := tbl.Lookup(gwTestDomain, "/x", nil); target == nil {
		t.Fatal("a declared domain must still resolve with no instances; " +
			"returning nil here would make a capacity problem look like a 404")
	} else if n := len(target.Instances); n != 0 {
		t.Fatalf("resolved to %v with no instances registered", sortedAddrs(target.Instances))
	}

	upsert("100.106.175.168", 8080)
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh after register: %v", err)
	}
	if target := tbl.Lookup(gwTestDomain, "/x", nil); target == nil ||
		len(target.Instances) != 1 {
		t.Fatalf("a freshly registered instance is not routable after Refresh: %v", target)
	}

	// The interesting direction: the instance leaves and the table must notice.
	remove("100.106.175.168", 8080)
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh after deregister: %v", err)
	}
	if target := tbl.Lookup(gwTestDomain, "/x", nil); target != nil &&
		len(target.Instances) != 0 {
		t.Fatalf("still routing to %v after the instance deregistered; "+
			"that is how a gateway ends up sending traffic to a host that is gone",
			sortedAddrs(target.Instances))
	}
}

// TestRefreshKeepsInstancesApartByNamespace pins the scoping the whole
// multi-stack design rests on: kxpms and itestu are two namespaces of one
// registry, and neither may ever be handed the other's instances.
func TestRefreshKeepsInstancesApartByNamespace(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declare(t, store, "DEFAULT_GROUP", "acc-api")

	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
		ServiceName: "acc-api", ClusterName: "DEFAULT",
		IP: "100.106.175.168", Port: 8080, Weight: 1, Enabled: true, Ephemeral: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The same service name in another namespace must not leak in.
	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: "itestu", GroupName: "DEFAULT_GROUP",
		ServiceName: "acc-api", ClusterName: "DEFAULT",
		IP: "100.106.177.3", Port: 8080, Weight: 1, Enabled: true, Ephemeral: true,
	}); err != nil {
		t.Fatalf("register other namespace: %v", err)
	}

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	target := tbl.Lookup(gwTestDomain, "/x", nil)
	if target == nil {
		t.Fatal("no target in the namespace that does have an instance")
	}
	for _, a := range sortedAddrs(target.Instances) {
		if a == "100.106.177.3:8080" {
			t.Error("the gateway picked an instance belonging to a different namespace; " +
				"one stack's traffic would be sent into the other")
			break
		}
	}
}

// TestRefreshDropsRoutesForDeclarationsThatNoLongerExist covers the silent case
// that looks most like success: a route whose application was deleted keeps
// resolving, and every log says the gateway is configured.
func TestRefreshDropsRoutesForDeclarationsThatNoLongerExist(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	declare(t, store, "DEFAULT_GROUP", "acc-api")
	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
		ServiceName: "acc-api", ClusterName: "DEFAULT",
		IP: "100.106.175.168", Port: 8080, Weight: 1, Enabled: true, Ephemeral: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tbl.Lookup(gwTestDomain, "/x", nil) == nil {
		t.Fatal("expected the intact declaration to route")
	}

	if err := store.DeleteComponent(ctx, gwTestNamespace, gwTestApp, "api"); err != nil {
		t.Fatalf("delete component: %v", err)
	}
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh after delete: %v", err)
	}
	if tbl.KnownDomain(gwTestDomain) {
		t.Error("a route whose component no longer exists must be dropped, " +
			"not kept as a domain that resolves to nothing")
	}
}

// TestRefreshIsNotHandedAWorkspaceTooSmallToBehave is a guard on this file
// itself: if the database silently accepts writes but returns no rows, the tests
// above would "pass" for entirely the wrong reason — every one of them asserts
// on absence after truncate, and absence is what a broken fixture produces.
func TestRefreshIsNotHandedAWorkspaceTooSmallToBehave(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()

	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
		ServiceName: "probe", ClusterName: "DEFAULT",
		IP: "10.0.0.1", Port: 1, Weight: 1, Enabled: true, Ephemeral: true,
	}); err != nil {
		t.Fatalf("probe registration failed, so the fixture is unusable: %v", err)
	}
	got, err := store.ListInstances(ctx, storage.InstanceListQuery{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP", ServiceName: "probe",
	})
	if err != nil {
		t.Fatalf("probe read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("wrote one instance and read back %d; the fixture is not behaving "+
			"and every assertion in this file would pass for the wrong reason", len(got))
	}
	if got[0].IP != "10.0.0.1" || got[0].Port != 1 {
		t.Errorf("round-trip returned %s:%d, want 10.0.0.1:1", got[0].IP, got[0].Port)
	}
}
