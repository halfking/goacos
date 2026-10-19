package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// 这一条把 `!`（台账里的兜底前缀）在**网关数据面**上的语义钉住。
//
// 为什么非钉不可：台账 `service-map.conf` 用 `!` 表达「这条是兜底」，
// reconcile 据此给实例写 `metadata.role=backup`（goacos-reconcile.sh:176）。
// nginx 的 `backup` 语义是「平时不参与轮转，本组全挂才顶上」。
// 而网关的 `selectInstances` **只看 ClusterName 与 Enabled，完全不看 role**，
// primary 与 backup 注册时 weight 又都是 1.0 —— 两者排序完全平权。
//
// 于是：一旦第 5、6 步让 goacos 网关成为数据面，台账里那 4 个带兜底的行
// （acc / redclaw / reclaw / openpocket-web，兜底全是 loopback:127.0.0.1）
// 会**各拿到约一半流量**，而网关容器打自己的 127.0.0.1 一律 502 ——
// 注册中心侧它们还显示 healthy，是最难排查的那一类。
//
// 这里钉三件事，一件都不能少：
//  1. 有 primary 时，backup 不得进入转发集合（这是本次修复的行为）；
//  2. **全网都是 backup 时仍然要转发** —— 宁可错、不可黑，一个被打错标签的
//     服务整体不答应，比用兜底顶上坏得多；
//  3. 运维列表仍然要**看得到** backup —— 否则出问题时运维界面上它就不存在了。

func instanceWithRole(ip string, port uint32, weight float64, role string) storage.Instance {
	inst := storage.Instance{
		IP:        ip,
		Port:      port,
		Weight:    weight,
		Healthy:   true,
		Enabled:   true,
		Ephemeral: true,
	}
	if role != "" {
		b, err := json.Marshal(map[string]string{"role": role})
		if err != nil {
			panic(err)
		}
		inst.MetadataJSON = string(b)
	}
	return inst
}

func addrSet(insts []storage.Instance) map[string]bool {
	out := map[string]bool{}
	for _, i := range insts {
		out[i.IP] = true
	}
	return out
}

// 1. 有 primary 时，backup 不得进入数据面转发集合。
func TestBackupInstanceStaysOutOfForwardingWhilePrimaryExists(t *testing.T) {
	in := []storage.Instance{
		instanceWithRole("10.0.0.1", 4100, 1, "primary"),
		instanceWithRole("10.0.0.2", 4100, 1, "backup"),
	}
	got := addrSet(selectForwardable(in, "", ""))
	if got["10.0.0.2"] {
		t.Fatalf("backup 实例进入了转发集合：%v", got)
	}
	if !got["10.0.0.1"] {
		t.Fatalf("primary 不在转发集合里：%v", got)
	}
}

// 1b. 兜底标签来自 metadata，不是来自一个独立字段 —— 不带 metadata 的实例
//
//	必须仍然被当成普通实例转发（否则未标注的老实例会集体消失）。
func TestUntaggedInstancesAreForwarded(t *testing.T) {
	in := []storage.Instance{
		instanceWithRole("10.0.0.1", 4100, 1, ""),
		instanceWithRole("10.0.0.2", 4100, 1, ""),
	}
	got := addrSet(selectForwardable(in, "", ""))
	if len(got) != 2 {
		t.Fatalf("未标注 role 的实例应全部可转发，实际 %v", got)
	}
}

// 2. 全网都是 backup 时仍然转发（fail open，不是 fail dark）。
func TestAllBackupInstancesStillServeWhenNothingElseIsLeft(t *testing.T) {
	in := []storage.Instance{
		instanceWithRole("10.0.0.1", 4100, 1, "backup"),
		instanceWithRole("10.0.0.2", 4100, 1, "backup"),
	}
	got := addrSet(selectForwardable(in, "", ""))
	if len(got) != 2 {
		t.Fatalf("全部为 backup 时应照常服务（宁可错不可黑），实际只转发 %v", got)
	}
}

// 3. 运维列表（selectInstances）仍然要返回 backup —— 过滤只发生在数据面。
func TestOperatorListingStillShowsBackupInstances(t *testing.T) {
	in := []storage.Instance{
		instanceWithRole("10.0.0.1", 4100, 1, "primary"),
		instanceWithRole("10.0.0.2", 4100, 1, "backup"),
	}
	got := addrSet(selectInstances(in, "", ""))
	if len(got) != 2 {
		t.Fatalf("运维列表必须仍然显示 backup 实例，实际 %v", got)
	}
}

// 4. **端到端**：真发 HTTP 请求穿过 dispatch，证明过滤没有被中间任何一环绕过。
//
// 上面三条都是对纯函数的断言 —— 它们证明 `selectForwardable` 算对了，
// 但没证明**转发路径真的用的是它**。这一条把整条链走完：
//
//	route table → Lookup → Target.Instances → Picker → 真正的 HTTP 转发
//
// ⚠ **第一版把兜底节点写成「没人监听的端口」，结果这条用例抓不到洞**：
// 网关本来就有故障转移（见 TestDispatchFailsOverToHealthyInstance），
// 打到打不通的节点会**换一台重试**，最后仍然 200、由 primary 应答 ——
// 于是修复与否都绿。**「兜底不可达」测不出「兜底该不该被选中」**，
// 因为不可达这件事本身已经被另一层兜住了。
//
// 正确的做法是让兜底节点**活着并应答**，只是它不该被选中：
// 修复前 round robin 会真的轮到它、请求由它应答（body 不同，可区分）；
// 修复后 20 次请求必须全部由 primary 应答。
func TestBackupNodeNeverReceivesForwardedTrafficEndToEnd(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served-by-primary"))
	}))
	defer primary.Close()

	// 兜底节点是**活的**，只是它本不该收到正常流量。
	// 生产里的等价物是网关自己 loopback 上恰好有东西应答的那一刻。
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("served-by-backup"))
	}))
	defer backup.Close()

	primaryHost, primaryPort := splitHostPort(t, primary.URL)
	backupHost, backupPort := splitHostPort(t, backup.URL)

	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{
			instanceWithRole(primaryHost, uint32(primaryPort), 1, "primary"),
			instanceWithRole(backupHost, uint32(backupPort), 1, "backup"),
		},
	})
	g := testGateway(tbl, nil)

	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
		g.dispatch(rec, req, false)
		if got := rec.Body.String(); got != "served-by-primary" {
			t.Fatalf("第 %d 次请求由**兜底节点**应答了（%q，状态 %d）—— "+
				"role=backup 仍然在吃正常流量。", i, got, rec.Code)
		}
	}
}
