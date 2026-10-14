package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// liveInstance 把一个 httptest server 变成一条「活的」注册实例。
func liveInstance(t *testing.T, srv *httptest.Server) Instance {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return Instance{IP: host, Port: uint32(port), Healthy: true, Enabled: true, Weight: 1}
}

// 这一份判据盯的是「同域名按路径拆前端与后端」这个用法。
//
// 为什么单独盯：252 现在的 nginx 就是这么拆的——117 个 server 块里 42 个有
// 多个 location，8 个带 /api/ 这类前缀（memora.kxpms.cn、pocket.kxpms.cn、
// openpocket-web.kxpms.cn 都是「/ + /api/」的组合）。所以从 nginx 切到 goacos，
// 同一个域名的「/」与「/api/」会落到不同组件上。
//
// 仓库的 applications.itestu.json 目前一个这种声明都没有：15 条路由的
// pathPrefix 全是「/」，前端后端一律用**子域名**分开。这是声明没跟上能力，
// 不是能力没有——网关的最长前缀匹配本来就能做（同见
// application_pg_test.go 里同域的 / 与 /api 两条路由）。
//
// 但「能做」和「做错了不报错」是两回事。下面的判据盯的就是后者。

// 场景 A：前端是活的 upstream，后端 /api 全挂 —— 必须 503，绝不能返回前端页面。
//
// 这是 252 那种「/ + /api/」拆法下最容易出的事：
// 如果路由匹配在前端那条上继续找，用户拿到的不是 503，而是整个前端应用。
// 前端把返回的 HTML 当成接口响应，解析失败会被报成 schema 错误，几层之下
// 没人查得到真正的原因是后端挂了。
//
// 注意这里的形状很要紧：前端组件**必须**注册一个活实例。第一版判据把它写成
// protocol=static 且零实例，结果「跳过零实例路由」的变异只会拿到 404，
// 压根触不到最危险的形态——判据看着绿，其实没测到点上。
func TestDispatchDeadApiRouteDoesNotFallThroughToLiveFrontend(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html id=app>frontend-shell</html>"))
	}))
	defer live.Close()

	tbl := buildTable(
		// 前端：活的 upstream，落在「/」
		testRoute{domain: "shop.example.com", prefix: "/", app: "shop",
			component: "web", kind: "web", protocol: "http", service: "shop-web",
			instances: []Instance{liveInstance(t, live)}},
		// 后端：落在「/api」，**零实例**（故障中）
		testRoute{domain: "shop.example.com", prefix: "/api", app: "shop",
			component: "api", kind: "api", protocol: "http", service: "shop-api"},
	)
	g := testGateway(tbl, nil)

	req := httptest.NewRequest(http.MethodGet, "http://shop.example.com/api/orders", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503；前端 upstream 是活的，绝不能拿前端页面顶替 API 故障", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "frontend-shell") {
		t.Fatalf("API 故障被前端页面顶替了：%s", body)
	}
	if !strings.Contains(rec.Body.String(), "shop-api") {
		t.Errorf("body should name the dead service: %s", rec.Body.String())
	}
}

// 场景 B：同一个域名在只有前端那条路由时，前端必须照常服务。
//
// 这是反向对照：如果上面的判据把问题写反（把整个域名都掐了），
// 这条会红。两条一起才说明「后端挂了只影响后端」。
func TestDispatchLiveFrontendSurvivesSiblingApiOutage(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html id=app>frontend-shell</html>"))
	}))
	defer live.Close()

	tbl := buildTable(
		testRoute{domain: "shop.example.com", prefix: "/", app: "shop",
			component: "web", kind: "web", protocol: "http", service: "shop-web",
			instances: []Instance{liveInstance(t, live)}},
		testRoute{domain: "shop.example.com", prefix: "/api", app: "shop",
			component: "api", kind: "api", protocol: "http", service: "shop-api"},
	)
	g := testGateway(tbl, nil)

	req := httptest.NewRequest(http.MethodGet, "http://shop.example.com/", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "frontend-shell") {
		t.Fatalf("前端应照常服务，status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 场景 C：static 前端 + 零实例后端。
//
// static 组件从磁盘服务、可以没有注册实例，所以这是另一种常见形状。
// 结果同样必须是 503，且不能吐出 index.html。
func TestDispatchDeadApiRouteDoesNotFallThroughToStaticFrontend(t *testing.T) {
	root := t.TempDir()
	webDir := filepath.Join(root, "shop-web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		t.Fatalf("mkdir web static root: %v", err)
	}
	// index.html 真的存在，否则 servesStatic 会提前返回 false，这条判据空转。
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html id=app>static-shell</html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	tbl := buildTable(
		testRoute{domain: "shop.example.com", prefix: "/", app: "shop",
			component: "web", kind: "web", protocol: "static", service: "shop-web"},
		testRoute{domain: "shop.example.com", prefix: "/api", app: "shop",
			component: "api", kind: "api", protocol: "http", service: "shop-api"},
	)
	g := testGateway(tbl, nil)
	g.cfg.GatewayStaticRoot = root

	req := httptest.NewRequest(http.MethodGet, "http://shop.example.com/api/orders", nil)
	rec := httptest.NewRecorder()
	g.dispatch(rec, req, false)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "static-shell") {
		t.Fatalf("API 故障被静态站 index.html 顶替了：%s", body)
	}

	// 反向：前端自己照常
	req2 := httptest.NewRequest(http.MethodGet, "http://shop.example.com/", nil)
	rec2 := httptest.NewRecorder()
	g.dispatch(rec2, req2, false)
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "static-shell") {
		t.Fatalf("静态前端应照常服务，status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

// 最长前缀优先，直接钉**生产那份**排序函数。
//
// 上一条 TestLookupPrefersApiRouteOverRootOnSameDomain 走的是测试夹具里的
// buildTable，而那份夹具当时自己抄了一份排序规则。把生产里的比较符翻成
// 「最短前缀优先」，那条判据照样全绿——它测的是抄件，不是网关的决策。
// 判据能测到的东西，以它测的那个函数为准。
func TestSortRouteEntriesPrefersLongestPrefix(t *testing.T) {
	entries := []routeEntry{
		{RouteBinding: storage.RouteBinding{PathPrefix: "/", ComponentName: "web", Priority: 100}},
		{RouteBinding: storage.RouteBinding{PathPrefix: "/api", ComponentName: "api", Priority: 100}},
		{RouteBinding: storage.RouteBinding{PathPrefix: "/api/v2", ComponentName: "api2", Priority: 100}},
		// 同样的长度，靠 priority 分先后
		{RouteBinding: storage.RouteBinding{PathPrefix: "/ws", ComponentName: "ws-late", Priority: 200}},
		{RouteBinding: storage.RouteBinding{PathPrefix: "/ws", ComponentName: "ws-early", Priority: 50}},
	}
	sortRouteEntries(entries)

	// 期望顺序：最长前缀优先（/api/v2 → /api → /ws → /），同长度按 priority
	// 小的在前（ws-early 50 在 ws-late 200 前），最后是 /。
	want := []string{"/api/v2/api2", "/api/api", "/ws/ws-early", "/ws/ws-late", "//web"}
	var got []string
	for _, e := range entries {
		got = append(got, e.PathPrefix+"/"+e.ComponentName)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("排序结果\n got %v\nwant %v", got, want)
	}
}

// 最长前缀优先，经由 Lookup 的表现来看。
func TestLookupPrefersApiRouteOverRootOnSameDomain(t *testing.T) {
	tbl := buildTable(
		testRoute{domain: "shop.example.com", prefix: "/", app: "shop",
			component: "web", kind: "web", protocol: "http", service: "shop-web"},
		testRoute{domain: "shop.example.com", prefix: "/api", app: "shop",
			component: "api", kind: "api", protocol: "http", service: "shop-api"},
	)

	for _, tc := range []struct{ path, want string }{
		{"/api/orders", "shop/api"},
		{"/api", "shop/api"},
		{"/", "shop/web"},
		{"/dashboard", "shop/web"},
	} {
		got := tbl.Lookup("shop.example.com", tc.path, nil)
		if got == nil {
			t.Fatalf("%s 没有匹配到任何路由", tc.path)
		}
		if name := got.AppName + "/" + got.ComponentName; name != tc.want {
			t.Errorf("Lookup(%q) 选中 %s, want %s", tc.path, name, tc.want)
		}
	}
}
