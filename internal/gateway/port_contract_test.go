package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// 端到端证明「组件端口契约」真的在转发路径上起作用。
//
// 背景：一个服务名下面挂着不同端口的实例，是「两个服务共用了同一个名字」的
// 签名。今天这种服务既没有健康检查能发现它（实例都活着），也没有日志能发现
// 它（轮询照常成功），只有被路由到错的那个端口的请求会出事。
//
// 这里钉住三件事：
//  1. 组件声明了 servicePort ⇒ 转发集合只剩那个端口；
//  2. 没声明 ⇒ 转发集合不变（命名模型按实例自带端口拨号），但冲突被记下来；
//  3. 冲突能被运维面看到 —— 否则「一个名字跨了多个端口」永远不会被发现。

// 1. 声明了 servicePort ⇒ 只转发该端口的实例。
func TestDeclaredServicePortKeepsOtherPortsOutOfForwarding(t *testing.T) {
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		metadata: map[string]string{storage.ServicePortMetaKey: "8090"},
		instances: []Instance{
			{IP: "10.0.0.1", Port: 4175, Weight: 1, Healthy: true, Enabled: true},
			{IP: "10.0.0.2", Port: 8090, Weight: 1, Healthy: true, Enabled: true},
		},
	})

	target := tbl.Lookup("acc.example.com", "/", nil)
	if target == nil {
		t.Fatal("路由未命中")
	}
	if len(target.Instances) != 1 {
		t.Fatalf("声明端口后转发集合应只剩 1 个实例，实际 %d：%v", len(target.Instances), target.Instances)
	}
	if target.Instances[0].Port != 8090 {
		t.Fatalf("转发集合里出现了别的端口：%d", target.Instances[0].Port)
	}
	if target.PortConflict == nil {
		t.Fatal("被排除的实例必须留痕，否则这次排除没有理由")
	}
	if target.PortConflict.Excluded != 1 {
		t.Fatalf("排除计数不对：%d", target.PortConflict.Excluded)
	}
}

// 2. 没声明 ⇒ 转发集合不变，但冲突被记下来。
func TestUndeclaredPortSpreadIsForwardedButRecorded(t *testing.T) {
	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		instances: []Instance{
			{IP: "10.0.0.1", Port: 4175, Weight: 1, Healthy: true, Enabled: true},
			{IP: "10.0.0.2", Port: 8090, Weight: 1, Healthy: true, Enabled: true},
		},
	})

	target := tbl.Lookup("acc.example.com", "/", nil)
	if target == nil {
		t.Fatal("路由未命中")
	}
	if len(target.Instances) != 2 {
		t.Fatalf("未声明端口时不应丢实例，实际 %d", len(target.Instances))
	}
	if target.PortConflict == nil {
		t.Fatal("端口跨布必须留痕")
	}
	if len(target.PortConflict.Ports) != 2 {
		t.Fatalf("冲突必须列出两个端口：%v", target.PortConflict.Ports)
	}
}

// 3. 声明端口一个实例都没有 ⇒ 没有可用落点，且原因说得出来。
//
// 这里刻意不写「no healthy instance」：实例是健康的，只是没有一个在声明的
// 端口上。把这两件事说成同一句，运维就会去查注册，而注册恰恰是好的。
func TestDeclaredPortWithNoMatchingInstanceFailsWithAReason(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	tbl := buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		metadata: map[string]string{storage.ServicePortMetaKey: "4175"},
		instances: []Instance{
			{IP: "10.0.0.2", Port: 8090, Weight: 1, Healthy: true, Enabled: true},
		},
	})
	g := testGateway(tbl, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("声明端口没有落点时应答 503，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "servicePort") {
		t.Fatalf("503 必须说清楚是端口契约落空，而不是泛泛的「无健康实例」：%s", body)
	}
}
