package gateway

// End-to-end balancing: the registry's instances → the route table → the
// balancer.
//
// ## Why this file exists
//
// The balancer is already tested in `gateway_test.go`
// (`TestWeightedRoundRobinHonoursWeights`, `TestIPHashPinsAClientToOneInstance`,
// `TestLeastConnPrefersTheIdleInstance`) — but every one of those builds the
// instance list **by hand**.
//
// A hand-built list proves the **algorithm**. It does not prove the
// **wiring**: that the weight a registering client wrote into
// `naming_instance.weight` reaches the balancer, that the algorithm the
// operator declared on the component reaches it, and that the instance set the
// balancer sees is the set the registry actually holds.
//
// That wiring is where the requirement lives or dies. 「不能固定死所有服务的
// 实例的 ip，需要根据当前可用来进行均衡调度」 is a statement about **the
// registry's currently available instances** — not about an algorithm in
// isolation. An algorithm that is handed the right set by hand proves nothing
// about a gateway that has to fetch that set from a database.
//
// ## The gap this closes
//
// Before it, **every** DB-backed criterion registered `Weight: 1` and declared
// `LBAlgorithm: "round_robin"`. ⇒ The whole "declared weight" and "declared
// algorithm" story had **no executed proof at all** — the same
// «green because it never ran» failure mode as the runtime-path criteria
// (`evidence/A-TEST-DEFINED-OUTSIDE-A-TESTCASE-CLASS-NEVER-RUNS-2026-10-24.md`).
//
// ## The two engines
//
// These run against MySQL (the engine 252 runs) and PostgreSQL (the engine
// 211 runs) through the shared fixture in `dbtest_test.go`.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// registerWeighted registers one instance of a service with an explicit
// weight, the way a registering client does.
func registerWeighted(t *testing.T, store *storage.Store, service, addr string, weight float64) {
	t.Helper()
	host, port := splitAddr(t, addr)
	_, err := store.RegisterInstance(context.Background(), storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: "DEFAULT_GROUP",
		ServiceName: service, ClusterName: "DEFAULT",
		IP: host, Port: port, Weight: weight, Enabled: true, Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("register %s (%s): %v", addr, service, err)
	}
}

// splitAddr splits "10.0.0.1:80" into host and port.
func splitAddr(t *testing.T, addr string) (string, uint32) {
	t.Helper()
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		t.Fatalf("地址 %q 里没有端口", addr)
	}
	host := addr[:colon]
	port, err := strconv.Atoi(addr[colon+1:])
	if err != nil {
		t.Fatalf("地址 %q 的端口解析失败：%v", addr, err)
	}
	return host, uint32(port)
}

// TestRegisteredInstancesShareTheTraffic is the plainest reading of
// 「按当前可用来进行均衡调度」: the instances the registry holds are the
// instances the requests are spread across.
//
// Equal weights, three instances, thirty picks ⇒ ten each. The number is
// exact rather than approximate because smooth weighted round robin is
// deterministic — and a deterministic expectation is what makes a failure
// mean something.
//
// The picks go through the **same call the data plane makes**
// (`proxy.go`: `picker.Pick(targetKey(target), target.LBAlgorithm, sticky,
// target.Instances)`), with the instance list the gateway actually built.
func TestRegisteredInstancesShareTheTraffic(t *testing.T) {
	forEachEngine(t, func(t *testing.T, store *storage.Store) {
		ctx := context.Background()
		const svc = "balanced-service"
		declareWithAlgorithm(t, store, "DEFAULT_GROUP", svc, "round_robin")

		want := []string{"10.0.0.1:80", "10.0.0.2:80", "10.0.0.3:80"}
		for _, a := range want {
			registerWeighted(t, store, svc, a, 1)
		}

		tbl := NewTable()
		if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		target := tbl.Lookup(gwTestDomain, "/", nil)
		if target == nil {
			t.Fatal("Lookup 没有解析出路由，夹具无效")
		}
		if got := sortedAddrs(target.Instances); len(got) != len(want) {
			t.Fatalf("路由表里的实例集是 %v，应为 %v", got, want)
		}

		picker := NewPicker()
		key := targetKey(target)
		counts := map[string]int{}
		for i := 0; i < 30; i++ {
			got := picker.Pick(key, target.LBAlgorithm, "", target.Instances)
			if got == nil {
				t.Fatal("picker 没有选出实例")
			}
			counts[addr(*got)]++
		}
		for _, w := range want {
			if counts[w] != 10 {
				t.Errorf("实例 %s 拿到 %d 次，应为 10 次；分布是 %v —— 均衡调度退化了",
					w, counts[w], counts)
			}
		}
	})
}

// TestRegisteredWeightsReachTheBalancer proves the weight a client wrote at
// registration time arrives at the balancer.
//
// The expectation is the same 30/10 the hand-built criterion uses — and that
// is exactly the point: the hand-built one proves the **algorithm**, this one
// proves the weight survives the whole trip — `naming_instance.weight` →
// `scanInstance` → `weightOf` → the picker — with the instance list coming
// out of the database.
//
// ⚠ It is worth being precise about what this does and does not say: it proves
// the **plumbing**. It does not prove an operator can ask for a weight —
// no client in the fleet has a weight knob (see the decision sheet), so today
// every instance registers with weight 1 and this path is only reachable by
// writing the registry directly.
func TestRegisteredWeightsReachTheBalancer(t *testing.T) {
	forEachEngine(t, func(t *testing.T, store *storage.Store) {
		ctx := context.Background()
		const svc = "weighted-service"
		declareWithAlgorithm(t, store, "DEFAULT_GROUP", svc, "round_robin")

		registerWeighted(t, store, svc, "10.0.0.1:80", 3)
		registerWeighted(t, store, svc, "10.0.0.2:80", 1)

		tbl := NewTable()
		if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		target := tbl.Lookup(gwTestDomain, "/", nil)
		if target == nil {
			t.Fatal("Lookup 没有解析出路由，夹具无效")
		}
		if got := sortedAddrs(target.Instances); len(got) != 2 {
			t.Fatalf("路由表里的实例集是 %v，应有 2 个", got)
		}

		picker := NewPicker()
		key := targetKey(target)
		counts := map[string]int{}
		for i := 0; i < 40; i++ {
			got := picker.Pick(key, target.LBAlgorithm, "", target.Instances)
			if got == nil {
				t.Fatal("picker 没有选出实例")
			}
			counts[addr(*got)]++
		}
		if counts["10.0.0.1:80"] != 30 || counts["10.0.0.2:80"] != 10 {
			t.Errorf("权重 3:1 在 40 次里应分成 30/10，实际 %v —— 注册时写的权重没有到达调度器",
				counts)
		}
	})
}

// TestDeclaredAlgorithmReachesThePicker proves the algorithm the operator
// declared on the component is the one the balancer uses.
//
// If the declaration did not arrive, the picker would fall back to round
// robin — and then an ip_hash component would stop pinning a client to one
// instance. That is not a subtle degradation: a session that migrates between
// requests is the failure this algorithm exists to prevent.
func TestDeclaredAlgorithmReachesThePicker(t *testing.T) {
	forEachEngine(t, func(t *testing.T, store *storage.Store) {
		ctx := context.Background()
		const svc = "pinned-service"
		declareWithAlgorithm(t, store, "DEFAULT_GROUP", svc, "ip_hash")

		for _, a := range []string{"10.0.0.1:80", "10.0.0.2:80", "10.0.0.3:80"} {
			registerWeighted(t, store, svc, a, 1)
		}

		tbl := NewTable()
		if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		target := tbl.Lookup(gwTestDomain, "/", nil)
		if target == nil {
			t.Fatal("Lookup 没有解析出路由，夹具无效")
		}
		if target.LBAlgorithm != "ip_hash" {
			t.Fatalf("组件声明的算法没有到达路由目标：拿到 %q，应为 %q",
				target.LBAlgorithm, "ip_hash")
		}

		picker := NewPicker()
		key := targetKey(target)
		first := picker.Pick(key, target.LBAlgorithm, "203.0.113.7", target.Instances)
		if first == nil {
			t.Fatal("picker 没有选出实例")
		}
		for i := 0; i < 5; i++ {
			got := picker.Pick(key, target.LBAlgorithm, "203.0.113.7", target.Instances)
			if addr(*got) != addr(*first) {
				t.Fatalf("ip_hash 把同一个客户端钉到了不同实例：%s vs %s —— 声明的算法没生效",
					addr(*got), addr(*first))
			}
		}
		// 而且它得真的在实例之间散开，而不是永远返回第一个。
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			sticky := fmt.Sprintf("203.0.113.%d", i)
			got := picker.Pick(key, target.LBAlgorithm, sticky, target.Instances)
			if got == nil {
				t.Fatal("picker 没有选出实例")
			}
			seen[addr(*got)] = true
		}
		if len(seen) < 2 {
			t.Errorf("ip_hash 把 20 个客户端全放到了同一个实例 %v —— 它退化成了固定打第一个",
				seen)
		}
	})
}

// TestDeclaredLeastConnAvoidsTheBusyInstance is the second algorithm, and it
// fails differently.
//
// With the declaration missing, the picker would use round robin — and the
// first instance in the list is exactly the busy one, so the busy instance
// would get the request. With least_conn the idle instance must win.
//
// The check is deliberately the *first* pick: a round-robin fallback would
// pick the same instance on every first pick, which is what makes this
// assertion sharp.
func TestDeclaredLeastConnAvoidsTheBusyInstance(t *testing.T) {
	forEachEngine(t, func(t *testing.T, store *storage.Store) {
		ctx := context.Background()
		const svc = "busy-service"
		declareWithAlgorithm(t, store, "DEFAULT_GROUP", svc, "least_conn")

		registerWeighted(t, store, svc, "10.0.0.1:80", 1)
		registerWeighted(t, store, svc, "10.0.0.2:80", 1)

		tbl := NewTable()
		if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		target := tbl.Lookup(gwTestDomain, "/", nil)
		if target == nil {
			t.Fatal("Lookup 没有解析出路由，夹具无效")
		}
		if target.LBAlgorithm != "least_conn" {
			t.Fatalf("组件声明的算法没有到达路由目标：拿到 %q，应为 %q",
				target.LBAlgorithm, "least_conn")
		}

		picker := NewPicker()
		key := targetKey(target)
		// 让第一个实例上挂一条连接。
		picker.Acquire(key, "10.0.0.1:80")
		got := picker.Pick(key, target.LBAlgorithm, "", target.Instances)
		if got == nil {
			t.Fatal("picker 没有选出实例")
		}
		if addr(*got) == "10.0.0.1:80" {
			t.Errorf("least_conn 把请求交给了正忙着的那台 %s —— 声明的算法没生效",
				addr(*got))
		}
	})
}
