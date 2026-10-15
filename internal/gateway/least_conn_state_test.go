package gateway

import (
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// 这组测试钉住 refresh() 喂给 Picker.Forget 的集合必须是**组件键**而不是域名。
//
// Forget(live) 会删掉键不在 live 里的全部计数集合。Picker 的键来自
// targetKey()（proxy.go:323）= AppName + "/" + ComponentName + "/" + ServiceName。
// 而 refresh() 曾经把 g.table.Domains 的元素塞进 live —— 那是**域名**。
// 域名永远不等于 "app/comp/svc"，于是**每一次路由刷新都清空全部 lbState**，
// 包括 least_conn 依赖的 inflight。
//
// 后果不是「少了一点优化」：清零之后每一次 Pick 看到的都是平局，
// 而 least_conn 的平局分支恒定选列表第一个实例（lb.go:95-98），
// 于是它在并发突发下退化成「永远选第一个」——
// 恰好把「避开忙的那个」这件事做成了「反复压榨同一个」。
//
// 2026-10-15 实测（本机实验台，两个后端一个 3s 一个 0s，并发 12）：
//
//	slow 端口更小 → 12/12 走 slow
//	slow 端口更大 → 12/12 走 fast      ← 流量跟着「列表第一个」走，与忙闲无关
//	对照组 round_robin → 6/6 均分
func TestTargetKeysAreComponentKeysNotDomains(t *testing.T) {
	tbl := NewTable()
	tbl.domains["lc.itestu.cn"] = []routeEntry{{
		RouteBinding: storage.RouteBinding{Domain: "lc.itestu.cn", PathPrefix: "/"},
		comp: &storage.ApplicationComponent{
			AppName: "lcapp", Name: "api", ServiceName: "lc-probe-svc",
		},
	}}

	got := tbl.TargetKeys()
	// 键形如 targetKey() 的输出。注意 ServiceName 是**裸服务名**，
	// 不是 allTargets 里拼 services map 用的 "group@@service" —— 第一版把
	// 预期写成后者，被这条判据当场抓住：判据比人可靠。
	want := "lcapp/api/lc-probe-svc"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("TargetKeys() = %v，want [%q]", got, want)
	}
	// 域名绝不能出现在结果里：它正是导致 Forget 清空一切的那种键。
	for _, k := range got {
		if k == "lc.itestu.cn" {
			t.Fatal("TargetKeys 返回了域名 —— refresh 会再次把 Forget 的 live 集合喂错")
		}
	}
}

// 同一个组件可以被多个域、多条路径前缀解析到，而它们**共用一套计数**
// （lb.go:18-21 的设计意图）。TargetKeys 必须去重，否则 Forget 只是多传几个
// 无害的键；但反过来漏掉任何一个就是致命的。
func TestTargetKeysDedupesSharedComponents(t *testing.T) {
	tbl := NewTable()
	comp := &storage.ApplicationComponent{AppName: "app", Name: "api", ServiceName: "svc"}
	tbl.domains["a.example.com"] = []routeEntry{
		{RouteBinding: storage.RouteBinding{Domain: "a.example.com", PathPrefix: "/api"}, comp: comp},
		{RouteBinding: storage.RouteBinding{Domain: "a.example.com", PathPrefix: "/v1"}, comp: comp},
	}
	tbl.domains["b.example.com"] = []routeEntry{
		{RouteBinding: storage.RouteBinding{Domain: "b.example.com", PathPrefix: "/"}, comp: comp},
	}

	got := tbl.TargetKeys()
	if len(got) != 1 {
		t.Fatalf("三个入口指向同一组件，应只产出一个键，实际 %v", got)
	}
}

// Forget 的语义本身没坏：live 形状对了，活着的留、死掉的收。
// 这条是上面那条的反向对照 —— 没有它，TargetKeys 恒返回空也能让上面那条绿。
func TestForgetWithComponentKeysKeepsLiveAndReclaimsDead(t *testing.T) {
	p := NewPicker()
	liveKey := "app/api/DEFAULT_GROUP@@svc"
	deadKey := "app/web/DEFAULT_GROUP@@gone"

	p.Acquire(liveKey, "10.0.0.1:9000")
	p.Acquire(deadKey, "10.0.0.2:9000")

	p.Forget(map[string]bool{liveKey: true})

	if p.stats[liveKey] == nil {
		t.Error("仍在用的组件状态被清掉了")
	}
	if got := p.stats[liveKey].inflight["10.0.0.1:9000"]; got != 1 {
		t.Errorf("inflight = %d，want 1", got)
	}
	if p.stats[deadKey] != nil {
		t.Error("已不用的组件状态没有被回收 —— Forget 失去意义")
	}
}

// 自护栅：TargetKeys 必须真的产出键。返回空表会让上面两条在空集上通过。
func TestTargetKeysFindsSeededRoutes(t *testing.T) {
	tbl := NewTable()
	if got := tbl.TargetKeys(); len(got) != 0 {
		t.Fatalf("空表应产出空集合，实际 %v", got)
	}
	tbl.domains["x.example.com"] = []routeEntry{{comp: &storage.ApplicationComponent{
		AppName: "app", Name: "api", ServiceName: "svc",
	}}}
	if len(tbl.TargetKeys()) != 1 {
		t.Fatal("塞了一条路由却没产出键 —— 判据本身失效")
	}
}
