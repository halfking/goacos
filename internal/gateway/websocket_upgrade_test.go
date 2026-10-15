package gateway

// WebSocket upgrade support: two things had to be true, and both were false.
//
// 2026-10-16 端到端实测（myvpn/scripts/goacos-websocket-probe.py，
// 证据 myvpn/evidence/GOACOS-WEBSOCKET-UPGRADE-BROKEN-2026-10-16.md）：
// 一个**合规**的 WebSocket 后端经网关时回 `426 Upgrade Required`；
// 一个**宽松**的后端让 ReverseProxy 报
// `backend tried to switch protocol "websocket" when "" was requested`。
//
// 那个 `""` 指向根因一：`applyRewrite` 把 `Connection` / `Upgrade`
// 当成普通逐跳头从**出站请求**上删了。RFC 7230 §6.1 要求代理把这两个头
// 转发给下一跳，ReverseProxy 也正是靠它们认出「这是一次升级」。
//
// 根因二在删完之后才会撞上：`responseRecorder` 没有实现 `http.Hijacker`，
// 于是即便头是对的，连接也交不出去。
//
// 只修一个都不算修好 —— 所以下面两组判据都必须绿，
// 而端到端那条（真握手 + 真帧中继）才是最终判据。

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func wsPlan(t *testing.T) (*http.Request, *rewritePlan) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/ws", nil)
	return req, &rewritePlan{
		target: &Target{
			AppName:       "app",
			ComponentName: "api",
			ServiceName:   "svc",
			Protocol:      "http",
			UpstreamName:  "up",
			PathPrefix:    "/",
		},
		upstream:     "10.0.0.1:8080",
		trustXFF:     false,
		clientIP:     netip.MustParseAddr("10.1.2.3"),
		originalHost: "app.example.com",
	}
}

// 一、升级的两个头必须活到上游。
func TestApplyRewriteKeepsUpgradeHeaders(t *testing.T) {
	req, plan := wsPlan(t)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")

	applyRewrite(req, plan)

	if got := req.Header.Get("Upgrade"); got != "websocket" {
		t.Fatalf("Upgrade 被 rewrite 删掉了（实际 %q）—— 上游收不到升级请求，"+
			"只会当成普通 GET，然后回 426 或 200，客户端永远拿不到 101", got)
	}
	if got := req.Header.Get("Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
		t.Fatalf("Connection: Upgrade 被 rewrite 删掉了（实际 %q）—— "+
			"ReverseProxy 靠它认出这是一次升级", got)
	}
}

// 反向对照：放行升级**不等于**放行整张 Connection 表。
// Connection 里点名的其他逐跳头仍然必须被清掉，否则就成了 header 注入面。
func TestApplyRewriteStillStripsNamedHopByHopOnUpgrade(t *testing.T) {
	req, plan := wsPlan(t)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade, X-Internal-Token")
	req.Header.Set("X-Internal-Token", "secret")

	applyRewrite(req, plan)

	if got := req.Header.Get("X-Internal-Token"); got != "" {
		t.Fatalf("Connection 里点名的 X-Internal-Token 被原样转发到上游了（%q）—— "+
			"这是一个逐跳头，转发等于把内部凭据交给后端", got)
	}
	if req.Header.Get("Upgrade") != "websocket" {
		t.Fatal("放行 Upgrade 的同时把 Upgrade 自己弄丢了")
	}
}

// 对照组：**不是**升级的请求，行为不能变。
func TestApplyRewriteStripsHopByHopOnOrdinaryRequest(t *testing.T) {
	req, plan := wsPlan(t)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade", "h2c") // 没有 upgrade token ⇒ 不是 WS 升级
	req.Header.Set("Keep-Alive", "timeout=5")

	applyRewrite(req, plan)

	if req.Header.Get("Connection") != "" {
		t.Errorf("普通请求的 Connection 应被清掉，实际 %q", req.Header.Get("Connection"))
	}
	if req.Header.Get("Upgrade") != "" {
		t.Errorf("非升级请求的 Upgrade 应被清掉，实际 %q", req.Header.Get("Upgrade"))
	}
}

// 二、连接必须交得出去。
var _ http.Hijacker = (*responseRecorder)(nil)

type fakeHijacker struct{ called bool }

func (f *fakeHijacker) Header() http.Header       { return http.Header{} }
func (f *fakeHijacker) Write([]byte) (int, error) { return 0, nil }
func (f *fakeHijacker) WriteHeader(int)           {}
func (f *fakeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.called = true
	return nil, nil, nil
}

func TestResponseRecorderHijacks(t *testing.T) {
	inner := &fakeHijacker{}
	rec := &responseRecorder{ResponseWriter: inner}

	if _, _, err := rec.Hijack(); err != nil {
		t.Fatalf("Hijack 失败: %v", err)
	}
	if !inner.called {
		t.Fatal("Hijack 没有委托给底层 writer")
	}
	if !rec.written {
		t.Error("hijack 之后 written 必须为 true —— 连接已经不在这一层了，" +
			"failover 不能再重试一个别人已经接管的请求")
	}
}

// 底层不支持 hijack 时要给出**指名**的错误，而不是静默继续。
func TestResponseRecorderHijackUnsupported(t *testing.T) {
	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
	conn, rw, err := rec.Hijack()
	if err == nil {
		t.Fatal("底层不支持 hijack 时应当报错")
	}
	if conn != nil || rw != nil {
		t.Errorf("失败时不该返回 conn/rw：%v %v", conn, rw)
	}
	if !strings.Contains(err.Error(), "cannot be hijacked") {
		t.Errorf("错误信息应当指名 writer 类型，实际 %q", err)
	}
}
