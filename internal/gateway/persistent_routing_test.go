package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// 这一条把 persistent 退出轮转这件事，从「数据库里一个标志位」一路验到
// 「网关不再把流量发给它」。
//
// 为什么非要端到端：2026-10-14 在 211 上实测到那条注册
// `accgw-api -> 172.18.0.18:4100 healthy=true ephemeral=false` 是幽灵
// （IP 是容器 smm 的，端口是容器 acc-nginx 的，拼起来谁都不服务）。
// 修法是让停更的 persistent 实例退出轮转。但这条修复是否真的有用，
// 取决于网关选实例时会不会跳过 unhealthy 的那条——
// 而 `selectInstances` 本身**只看 Enabled，不看 Healthy**。
// 整条链是这样接上的：
//
//	MarkStalePersistentUnhealthy  ->  naming_instance.healthy = 0
//	  -> ListInstances(HealthyOnly: true)   （routetable.go:206）
//	    -> t.services 里根本没有它
//	      -> Lookup 返回的 Target.Instances 不含它
//
// 中间任何一环断掉，这次修复就是**完全无效**的：标志位改了，
// 流量照旧发给幽灵。所以这里钉的是最后一环。

// 一条停更的 persistent 实例必须从网关的选实例集合里消失。
func TestStalePersistentInstanceLeavesGatewayRouting(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	const svc = "fleet-service"

	// 先声明好路由，让 Refresh 会去加载这个服务的实例。
	declare(t, store, appTestGroup, svc)
	registerAppInstance(t, store, svc, "10.0.0.1", 8080)

	// 一条健康的 persistent，和一条停更 10 分钟的 persistent。
	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: appTestGroup, ServiceName: svc,
		ClusterName: appTestCluster, IP: "10.0.0.2", Port: 9090,
		Weight: 1, Enabled: true, Ephemeral: false,
	}); err != nil {
		t.Fatalf("register healthy persistent: %v", err)
	}
	ageInstance(t, store, gwTestNamespace, appTestGroup, svc, "10.0.0.3", 7070, 10*time.Minute)
	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: appTestGroup, ServiceName: svc,
		ClusterName: appTestCluster, IP: "10.0.0.3", Port: 7070,
		Weight: 1, Enabled: true, Ephemeral: false,
	}); err != nil {
		t.Fatalf("register stale persistent: %v", err)
	}
	ageInstance(t, store, gwTestNamespace, appTestGroup, svc, "10.0.0.3", 7070, 10*time.Minute)

	// 自护栅：三条都必须真的进库，否则下面会因为「集合本来就空」而空转。
	if got := countInstanceRows(t, store, gwTestNamespace, appTestGroup, svc); got != 3 {
		t.Fatalf("夹具只写入了 %d 条实例，want 3", got)
	}

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	target := tbl.Lookup(gwTestDomain, "/", nil)
	if target == nil {
		t.Fatal("Lookup 没有解析出路由，夹具无效")
	}
	// 扫之前，它**仍然在**轮转里。
	//
	// 这不是缺陷，是这套机制的固有窗口：网关按 healthy=1 过滤，而
	// last_heartbeat_ms 过期本身不改 healthy——要等 sweeper 跑那一拍。
	// 所以修复的效果是「把暴露窗口从无限缩到 ~PersistentHeartbeatTimeoutMs」，
	// 不是「停更即刻摘除」。把它写成断言，免得后人以为自己测的是后者。
	if !containsAddr(target.Instances, "10.0.0.3:7070") {
		t.Error("扫之前停更的实例就已经不在了——那说明判据在测 sweeper 之外的机制，" +
			"与本文件要钉的「退出轮转接不接得上数据面」不是一回事")
	}

	// 真正的动作：标记退出轮转之后，刷新一次它就该消失。
	if _, err := store.MarkStalePersistentUnhealthy(ctx, 90_000); err != nil {
		t.Fatalf("MarkStalePersistentUnhealthy: %v", err)
	}
	tbl2 := NewTable()
	if err := tbl2.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh after sweep: %v", err)
	}
	after := tbl2.Lookup(gwTestDomain, "/", nil)
	if after == nil {
		t.Fatal("sweep 之后 Lookup 反而解析不出来了")
	}
	if containsAddr(after.Instances, "10.0.0.3:7070") {
		t.Error("标记不健康并刷新后，它仍在选实例集合里 —— 健康位没有真正影响数据面")
	}
	// 正向对照：其余实例必须都还在。只断幽灵、不伤其余，才算修对了。
	for _, want := range []string{"10.0.0.1:8080", "10.0.0.2:9090"} {
		if !containsAddr(after.Instances, want) {
			t.Errorf("%s 被误伤了；判据可能只是碰巧通过", want)
		}
	}
}

// 健康位是网关唯一的可用性依据，所以 selectInstances 不看 Healthy 这件事
// 必须被显式钉住：它完全依赖调用方传了 HealthyOnly:true。
//
// 这不是假设的风险——routetable.go:206 确实是这么写的，但这个耦合没有任何
// 判据守着。哪天有人为了「让 unhealthy 的实例也参与」，把 HealthyOnly 改成
// false（或者给别的地方补一条不走 ListInstances 的加载路径），幽灵就会
// 重新回到轮转里，而上面那条判据仍然全绿。
//
// 所以这里断言的是一个**当前的不变式**：出现在 Target.Instances 里的实例
// 一定是 healthy 的。将来若真要让网关自行过滤，这里会红，届时应当
// 显式改判据，而不是悄悄放行。
func TestRoutingTableNeverCarriesUnhealthyInstances(t *testing.T) {
	store := openPGStore(t)
	ctx := context.Background()
	const svc = "health-filter-service"

	declare(t, store, appTestGroup, svc)
	registerAppInstance(t, store, svc, "10.1.0.1", 8080)
	if _, err := store.RegisterInstance(ctx, storage.InstanceUpsert{
		NamespaceId: gwTestNamespace, GroupName: appTestGroup, ServiceName: svc,
		ClusterName: appTestCluster, IP: "10.1.0.2", Port: 8080,
		Weight: 1, Enabled: true, Ephemeral: false,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 手工把它标成不健康，绕过任何 sweeper。
	if _, err := store.Exec(ctx,
		`UPDATE naming_instance SET healthy=0 WHERE ip='10.1.0.2'`); err != nil {
		t.Fatalf("mark unhealthy: %v", err)
	}

	tbl := NewTable()
	if err := tbl.Refresh(ctx, store, gwTestNamespace); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	target := tbl.Lookup(gwTestDomain, "/", nil)
	if target == nil {
		t.Fatal("Lookup 没有解析出路由，夹具无效")
	}
	for _, inst := range target.Instances {
		if inst.IP == "10.1.0.2" {
			t.Fatal("不健康的实例出现在 Target.Instances 里 —— 网关正在把流量发给它")
		}
	}
}

// ageInstance rewrites last_heartbeat_ms so the row looks like it stopped
// refreshing. RegisterInstance always stamps NowMS(), so aging has to go
// through SQL — which is fine, the sweeper under test is SQL too.
func ageInstance(t *testing.T, st *storage.Store, ns, group, svc, ip string, port uint32, age time.Duration) {
	t.Helper()
	// store.Exec 走 dialect.Rebind；DB.ExecContext 不走，"?" 会被原样送进 postgres。
	_, err := st.Exec(context.Background(),
		`UPDATE naming_instance SET last_heartbeat_ms = last_heartbeat_ms - ?
		 WHERE namespace_id=? AND group_name=? AND service_name=? AND ip=? AND port=?`,
		int64(age/time.Millisecond), ns, group, svc, ip, port)
	if err != nil {
		t.Fatalf("age %s:%d: %v", ip, port, err)
	}
}

func countInstanceRows(t *testing.T, st *storage.Store, ns, group, svc string) int {
	t.Helper()
	var n int
	if err := st.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM naming_instance WHERE namespace_id=? AND group_name=? AND service_name=?`,
		ns, group, svc).Scan(&n); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	return n
}

func containsAddr(list []storage.Instance, addr string) bool {
	for _, i := range list {
		if i.IP+":"+itoa(int(i.Port)) == addr {
			return true
		}
	}
	return false
}
