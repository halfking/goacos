package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
)

// 这一组判据钉的是需求第 4 条「按当前可用来进行均衡调度」的前半句：
// 注册中心必须知道什么叫「当前不可用」。
//
// 2026-10-14 在 211 上实测：itestu 生产注册表唯一那条注册是
// `accgw-api -> 172.18.0.18:4100 healthy=true ephemeral=false`，
// 而 172.18.0.18 是容器 smm、4100 是容器 acc-nginx 的端口，拼起来谁都不服务。
// 它能一直报 healthy，是因为全仓只有 SweepEphemeral 会把 healthy 置 0，
// 条件写死 ephemeral=1 —— persistent 实例没有任何过期机制。
//
// 修法：MarkStalePersistentUnhealthy —— 超时标不健康，**不删除**。

// openNamingStore follows the same convention as the gateway's PG tests:
// no DSN, no test. The self-guard below is what keeps that honest.
func openNamingStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("GOACOS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GOACOS_TEST_PG_DSN is not configured")
	}
	store, err := Open(context.Background(), &config.Config{DBType: "postgres", DBDSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.DB.ExecContext(context.Background(),
		`TRUNCATE naming_instance, naming_service`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store
}

// staleInstance writes a row with a deliberately old heartbeat. RegisterInstance
// always stamps NowMS(), so the aging has to happen through SQL — which is
// fine, because the sweeper under test is itself SQL.
func staleInstance(t *testing.T, st *Store, svc, ip string, port uint32, ephemeral bool, age time.Duration) {
	t.Helper()
	// 经 store.Exec 而不是 DB.ExecContext：后者不走 dialect.Rebind，
	// "?" 占位符在 postgres 下原样送出，直接语法错。
	_, err := st.Exec(context.Background(), `
		INSERT INTO naming_instance
			(namespace_id, group_name, service_name, cluster_name, ip, port, weight,
			 healthy, enabled, ephemeral, metadata, last_heartbeat_ms)
		VALUES (?,?,?,?,?,?,1,1,1,?,'',?)`,
		"testns", "DEFAULT_GROUP", svc, "DEFAULT", ip, port, boolInt(ephemeral),
		NowMS()-int64(age/time.Millisecond))
	if err != nil {
		t.Fatalf("insert stale instance %s:%d: %v", ip, port, err)
	}
}

func liveInstance(t *testing.T, st *Store, svc, ip string, port uint32, ephemeral bool) {
	t.Helper()
	if _, err := st.RegisterInstance(context.Background(), InstanceUpsert{
		NamespaceId: "testns", GroupName: "DEFAULT_GROUP", ServiceName: svc,
		ClusterName: "DEFAULT", IP: ip, Port: port,
		Enabled: true, Ephemeral: ephemeral, Weight: 1,
	}); err != nil {
		t.Fatalf("register instance %s:%d: %v", ip, port, err)
	}
}

func isHealthy(t *testing.T, st *Store, svc, ip string, port uint32) bool {
	t.Helper()
	var healthy int
	err := st.QueryRow(context.Background(),
		`SELECT healthy FROM naming_instance WHERE service_name=? AND ip=? AND port=?`,
		svc, ip, port).Scan(&healthy)
	if err != nil {
		t.Fatalf("read back %s:%d: %v", ip, port, err)
	}
	return healthy == 1
}

func countInstances(t *testing.T, st *Store, svc string) int {
	t.Helper()
	var n int
	if err := st.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM naming_instance WHERE service_name=?`, svc).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", svc, err)
	}
	return n
}

// 停止刷新的 persistent 实例必须退出轮转，而刚刷新的那个必须毫发无损。
func TestMarkStalePersistentUnhealthy_StopsSelectingGhosts(t *testing.T) {
	st := openNamingStore(t)
	svc := "ghost-service"

	staleInstance(t, st, svc, "172.18.0.18", 4100, false, 10*time.Minute)
	liveInstance(t, st, svc, "172.18.0.30", 80, false)

	n, err := st.MarkStalePersistentUnhealthy(context.Background(), 90_000)
	if err != nil {
		t.Fatalf("MarkStalePersistentUnhealthy: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d, want 1（只有那条停更 10 分钟的该退轮转）", n)
	}
	if isHealthy(t, st, svc, "172.18.0.18", 4100) {
		t.Error("停更的 persistent 实例仍是 healthy —— 它会继续被选中，正是要修的故障")
	}
	if !isHealthy(t, st, svc, "172.18.0.30", 80) {
		t.Error("刚刷新的 persistent 实例被误标不健康 —— 超时阈值必须远大于生产者的刷新间隔")
	}
}

// 最关键的安全性质：persistent 的语义是「不因沉默而消失」。
// 破了这条，reconcile 一停，整个 fleet 的注册记录会被清空。
func TestMarkStalePersistentUnhealthy_NeverDeletes(t *testing.T) {
	st := openNamingStore(t)
	svc := "persistent-keepalive"
	staleInstance(t, st, svc, "10.0.0.9", 8080, false, 24*time.Hour)

	if _, err := st.MarkStalePersistentUnhealthy(context.Background(), 1_000); err != nil {
		t.Fatalf("MarkStalePersistentUnhealthy: %v", err)
	}
	if got := countInstances(t, st, svc); got != 1 {
		t.Fatalf("实例还剩 %d 行，want 1 —— persistent 只能退出轮转，不能被删除", got)
	}
}

// ephemeral 归 SweepEphemeral 管（超时直接删）。两条判定混在一起会重复劳动，
// 所以边界要钉住。
func TestMarkStalePersistentUnhealthy_IgnoresEphemeral(t *testing.T) {
	st := openNamingStore(t)
	svc := "ephemeral-service"
	staleInstance(t, st, svc, "10.0.0.5", 1234, true, 10*time.Minute)

	n, err := st.MarkStalePersistentUnhealthy(context.Background(), 90_000)
	if err != nil {
		t.Fatalf("MarkStalePersistentUnhealthy: %v", err)
	}
	if n != 0 {
		t.Fatalf("marked %d ephemeral instances, want 0（它们由 SweepEphemeral 负责）", n)
	}
	if !isHealthy(t, st, svc, "10.0.0.5", 1234) {
		t.Error("本函数不应改动 ephemeral 实例的健康位")
	}
}

// 超时配成 0 = 明确关闭这条检查。没有这个开关，运维只能靠调小超时来「关」，
// 而那会误伤 ephemeral。
func TestMarkStalePersistentUnhealthy_ZeroTimeoutDisables(t *testing.T) {
	st := openNamingStore(t)
	svc := "explicitly-off"
	staleInstance(t, st, svc, "10.0.0.7", 9999, false, 10*time.Minute)

	n, err := st.MarkStalePersistentUnhealthy(context.Background(), 0)
	if err != nil {
		t.Fatalf("timeout=0 不该报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("timeout=0 时标记了 %d 条，want 0（0 表示关闭）", n)
	}
	if !isHealthy(t, st, svc, "10.0.0.7", 9999) {
		t.Error("timeout=0 时不应改动任何实例")
	}
}

// 自护栅：夹具必须真的写进去东西。
//
// 这四条判据全是「标记了几条」「健康位是什么」的断言。夹具若因表结构、方言、
// 字段名变化而一行没写进去，它们会因为 0 == 0 而全绿——那正是本仓库
// provenance 判据加过同款守卫的原因。这里不靠 skip 蒙混：DSN 没配就整组
// skip，配了就必须真的建出数据。
func TestPersistentLivenessFixtureIsNotAStub(t *testing.T) {
	st := openNamingStore(t)
	svc := "fixture-check"
	staleInstance(t, st, svc, "10.1.0.1", 1111, false, time.Hour)
	liveInstance(t, st, svc, "10.1.0.2", 2222, false)

	if got := countInstances(t, st, svc); got != 2 {
		t.Fatalf("夹具只写入了 %d 行，want 2 —— 下面的判据会在空集上空转", got)
	}
	if !isHealthy(t, st, svc, "10.1.0.1", 1111) {
		t.Error("夹具里那条「陈旧」实例一开始就不健康，判据将无意义")
	}
	if _, err := st.MarkStalePersistentUnhealthy(context.Background(), 90_000); err != nil {
		t.Fatalf("MarkStalePersistentUnhealthy: %v", err)
	}
	if isHealthy(t, st, svc, "10.1.0.1", 1111) {
		t.Error("超时 90s、陈旧 1h，却没有退出轮转 —— 判据测不到东西")
	}
}
