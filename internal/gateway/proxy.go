package gateway

// The reverse proxy: turning a resolved target into a request to a live
// instance.
//
// # Why one transport per instance address
//
// Keying connection pools by upstream address rather than by component means a
// deregistered instance's pooled connections disappear with its entry instead
// of lingering until idle timeout. It also makes the least_conn counter
// meaningful: the number of open connections to an instance is exactly the
// number of pooled connections the gateway holds against it.
//
// # Why the rewrite lives in a hook rather than before dispatch
//
// net/http's ReverseProxy refuses to run without either a Director or a
// Rewrite, and in Rewrite mode it strips every client-supplied X-Forwarded-*
// header before calling the hook. So the forwarding headers cannot be set
// before ServeHTTP: they would simply be deleted. They are set inside the
// hook, which is also where the scheme, host and prefix handling belong.
//
// # Why failover only happens before a byte is written
//
// This is nginx's proxy_next_upstream, and it is what makes a rolling restart
// invisible. But it is only safe while the response is still unwritten: once
// the client has the first byte of a 500, replaying the request elsewhere
// produces two answers for one request and can double-charge a payment. So the
// proxy handler records the failure instead of writing it, and the failover
// decision happens one level up, where the writer's state is known.

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// maxAttempts bounds failover. Two attempts covers the normal case (one dead
// instance); more than three in a row usually means every instance is down, at
// which point failing fast beats holding the client's connection open through a
// retry storm.
const maxAttempts = 3

// stickyCookieName carries the instance a client was pinned to, so
// cookie_sticky keeps the client on it without re-deriving anything.
const stickyCookieName = "GOACOS_INSTANCE"

// rewritePlanType is the context key carrying the routing decision from
// Forward into the shared ReverseProxy's Rewrite hook. One ReverseProxy serves
// every concurrent request to an instance, so the decision travels with the
// request rather than being captured in a closure.
type rewritePlanType struct{}

// rewritePlan is everything the hook needs that is not already on the request.
type rewritePlan struct {
	target       *Target
	upstream     string
	trustXFF     bool
	clientIP     netip.Addr
	inboundXFF   string
	originalHost string
}

// proxyEntry is one instance's transport plus the reverse proxy that uses it.
type proxyEntry struct {
	transport *http.Transport
	reverse   *httputil.ReverseProxy
}

// transportPool caches per-upstream connection pools.
type transportPool struct {
	mu      sync.RWMutex
	entries map[string]*proxyEntry
}

func newTransportPool() *transportPool {
	return &transportPool{entries: map[string]*proxyEntry{}}
}

// poolKey is "address|websocket".
//
// The websocket flag is in the key because a websocket-enabled transport keeps
// no response-header timeout, and a target must not silently inherit that
// behaviour from another target that happened to share its address.
func poolKey(instance Instance, pol Policy) string {
	return addr(instance) + "|" + boolFlag(pol.Websocket)
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// get returns a pooled transport and reverse proxy for one instance, creating
// them on first use.
func (p *transportPool) get(instance Instance, pol Policy, timeout time.Duration) *proxyEntry {
	key := poolKey(instance, pol)
	if entry, ok := p.lookup(key); ok {
		return entry
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: time.Duration(pol.ConnectTimeoutMS) * time.Millisecond,
			// Keep-alives are the whole reason this pool exists. A generous
			// idle window is safe because a deregistered instance's entry is
			// pruned on the next refresh rather than waiting to expire.
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	// The response-header timeout is what actually bounds a hung upstream. It
	// is disabled for upgraded connections, which are long-lived by design and
	// would otherwise be cut off mid-session.
	if !pol.Websocket {
		transport.ResponseHeaderTimeout = timeout
	}

	entry := &proxyEntry{transport: transport}
	entry.reverse = &httputil.ReverseProxy{
		Transport: transport,
		// Negative flush interval disables buffering entirely, so streaming
		// responses reach the client as they are produced.
		FlushInterval: -1,
		// Rewrite rather than Director: it is the current API, it runs after
		// the client-supplied forwarding headers have been stripped, and it
		// leaves the Host header alone so this gateway can decide that.
		Rewrite: func(pr *httputil.ProxyRequest) {
			plan, _ := pr.Out.Context().Value(rewritePlanType{}).(*rewritePlan)
			if plan == nil {
				return
			}
			applyRewrite(pr.Out, plan)
		},
		// The error handler deliberately writes nothing. Forward decides
		// whether this failure can be retried against another instance, and
		// that decision is only safe while nothing has been sent.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if rec, ok := w.(*responseRecorder); ok {
				rec.failed = err
			}
		},
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.entries[key]; ok {
		// Another request won the race; keep theirs and discard ours so the
		// pool never holds two pools for one upstream.
		transport.CloseIdleConnections()
		return existing
	}
	p.entries[key] = entry
	return entry
}

func (p *transportPool) lookup(key string) (*proxyEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.entries[key]
	return entry, ok
}

// prune drops pools for instances that are no longer routable, closing their
// idle connections so a deregistered instance is released immediately rather
// than kept warm for the idle timeout.
func (p *transportPool) prune(liveAddrs map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entry := range p.entries {
		upstream := key
		if i := strings.IndexByte(key, '|'); i >= 0 {
			upstream = key[:i]
		}
		if !liveAddrs[upstream] {
			entry.transport.CloseIdleConnections()
			delete(p.entries, key)
		}
	}
}

// size reports how many upstream pools are currently held.
func (p *transportPool) size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// closeAll releases every pooled connection, for shutdown.
func (p *transportPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entry := range p.entries {
		entry.transport.CloseIdleConnections()
		delete(p.entries, key)
	}
}

// responseRecorder tracks whether anything was written and whether the proxy
// failed, so failover can be decided safely.
type responseRecorder struct {
	http.ResponseWriter
	written bool
	failed  error
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

// Hijack hands the underlying connection to ReverseProxy so a protocol upgrade
// can switch to bidirectional relaying.
//
// Without this method the upgrade fails at the very last step, with Go's
// "can't switch protocols using non-Hijacker ResponseWriter" — the request
// would reach the upstream correctly and the response would come back
// correctly, and the connection still could not be taken over. It is the
// second half of the same bug the outbound Upgrade header was: both had to be
// fixed before /ws worked at all.
//
// written is set because the connection is gone from here on: failover must
// not retry a request whose socket another layer now owns.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("gateway: response writer %T cannot be hijacked",
			r.ResponseWriter)
	}
	r.written = true
	return h.Hijack()
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (SSE, large downloads) unbuffered.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Forward proxies one request to a target, failing over between instances
// while it is still safe to do so.
func (g *Gateway) Forward(w http.ResponseWriter, r *http.Request, t *Target, pol Policy) {
	if len(t.Instances) == 0 {
		writeGatewayError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("no healthy instance for service %q (component %s/%s)", t.ServiceName, t.AppName, t.ComponentName))
		return
	}

	sticky := g.stickyKey(r, t)
	key := targetKey(t)
	timeout := time.Duration(pol.ReadTimeoutMS) * time.Millisecond
	clientIP := ClientAddr(r, g.cfg.GatewayTrustForwardedFor)

	var lastErr error
	tried := map[string]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		instance := g.picker.Pick(key, t.LBAlgorithm, sticky, t.Instances)
		if instance == nil {
			break
		}
		upstream := addr(*instance)
		if tried[upstream] && len(t.Instances) > 1 {
			// Everything reachable has already been tried: a further attempt
			// would only re-hit a known-dead instance.
			break
		}
		tried[upstream] = true

		if isWebSocketUpgrade(r) && !pol.Websocket {
			writeGatewayError(w, http.StatusForbidden, "websocket upgrades are disabled for this domain")
			return
		}

		plan := &rewritePlan{
			target:       t,
			upstream:     upstream,
			trustXFF:     g.cfg.GatewayTrustForwardedFor,
			clientIP:     clientIP,
			inboundXFF:   r.Header.Get("X-Forwarded-For"),
			originalHost: r.Host,
		}
		outbound := r.Clone(context.WithValue(r.Context(), rewritePlanType{}, plan))
		if outbound.Header == nil {
			outbound.Header = http.Header{}
		}
		outbound.Header.Del("X-Goacos-Instance-Pin")

		entry := g.pool.get(*instance, pol, timeout)
		g.picker.Acquire(key, upstream)

		rec := &responseRecorder{ResponseWriter: w}
		entry.reverse.ServeHTTP(rec, outbound)

		g.picker.Release(key, upstream)

		if rec.failed == nil {
			// A response was produced, or the upstream said something itself.
			// Either way this request is finished and belongs to this instance.
			return
		}
		lastErr = fmt.Errorf("upstream %s: %w", upstream, rec.failed)
		if rec.written {
			// Bytes already reached the client; there is nothing to retry.
			return
		}
		g.upstreamFailed.Add(1)
		log.Printf("[gateway] upstream %s failed, trying another instance: %v", upstream, rec.failed)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no instance available for service %q", t.ServiceName)
	}
	log.Printf("[gateway] host=%s path=%s could not be served: %v", r.Host, r.URL.Path, lastErr)
	writeGatewayError(w, http.StatusBadGateway, lastErr.Error())
}

// targetKey identifies a component for load-balancing bookkeeping. Counters
// belong to the component, not to a route binding: two domains pointing at one
// component must see one consistent instance set.
func targetKey(t *Target) string {
	return t.AppName + "/" + t.ComponentName + "/" + t.ServiceName
}

// stickyKey extracts the value that should pin this request to an instance.
//
// ip_hash uses the client address, cookie_sticky uses the session cookie. The
// cookie is only read when the component asks for it, keeping the common case
// — round robin — free of per-request cookie work.
func (g *Gateway) stickyKey(r *http.Request, t *Target) string {
	switch t.LBAlgorithm {
	case "ip_hash":
		return ClientAddr(r, g.cfg.GatewayTrustForwardedFor).String()
	case "cookie_sticky":
		if c, err := r.Cookie(stickyCookieName); err == nil {
			return c.Value
		}
		return ""
	}
	return ""
}

// applyRewrite prepares the outbound request, from inside the proxy hook.
//
// The full original path is forwarded by default. Stripping the matched prefix
// is opt-in through component metadata, because a frontend calling
// /api/v2/health against a backend that also serves /healthz cannot tell which
// convention the gateway chose — and a silent choice here is how a gateway
// takes down an otherwise healthy application.
func applyRewrite(out *http.Request, plan *rewritePlan) {
	t := plan.target

	// The component's declared protocol decides how the upstream is dialled —
	// not how the client arrived. The gateway terminates TLS at its own edge
	// and re-originates in plain text unless the component says otherwise, so
	// an internal service on port 8080 does not have to hold a certificate for
	// traffic that never leaves the host.
	scheme := "http"
	if t.Protocol == storage.ProtocolHTTPS {
		scheme = "https"
	}

	out.URL.Scheme = scheme
	out.URL.Host = plan.upstream

	if t.StripPrefix {
		prefix := strings.TrimSuffix(t.PathPrefix, "/")
		if prefix != "" && (out.URL.Path == prefix || strings.HasPrefix(out.URL.Path, prefix+"/")) {
			trimmed := strings.TrimPrefix(out.URL.Path, prefix)
			if trimmed == "" {
				trimmed = "/"
			}
			out.URL.Path = trimmed
			out.URL.RawPath = ""
		}
	}

	// Hop-by-hop headers belong to a single connection and must not be
	// forwarded. Connection-listed headers are named per request, so they have
	// to be removed explicitly rather than by a fixed list alone.
	//
	// **协议升级是这套规则唯一的例外，而这里原来是漏的。**
	// `Connection: Upgrade` 与 `Upgrade: websocket` 看着也是逐跳的，但
	// RFC 7230 §6.1 要求代理把它们转发给下一跳 —— 删掉它们，升级永远协商不成。
	// net/http 的 ReverseProxy 正是靠这两个头认出「这是一次升级」并切换到
	// 双向中继；头被删之后，上游只会把它当成普通请求。
	//
	// 端到端实测（myvpn/scripts/goacos-websocket-probe.py，2026-10-16）：
	// 删掉时，一个**合规**的 WS 后端回 `426 Upgrade Required`；
	// 一个**宽松**的后端（不检查 Upgrade 就回 101）会让 ReverseProxy 报
	// `backend tried to switch protocol "websocket" when "" was requested`
	// —— 那个 `""` 就是被删掉的上游 Upgrade。两种症状下客户端都**拿不到 101**。
	upgrade := isWebSocketUpgrade(out)
	savedConn, savedUpgrade := out.Header.Get("Connection"), out.Header.Get("Upgrade")
	removeHopByHop(out.Header)
	if upgrade {
		// 只把这两个放回去，`Connection` 里点名的其他逐跳头仍然被清掉 ——
		// 放行升级不等于放行整张 Connection 表。
		out.Header.Set("Connection", savedConn)
		out.Header.Set("Upgrade", savedUpgrade)
	}

	// The forwarded chain is rebuilt rather than trusted. A client-supplied
	// prefix survives only when the operator has declared that something
	// trustworthy sits in front of the gateway; otherwise the forged part is
	// dropped and the observed peer stands alone.
	if plan.inboundXFF != "" && plan.trustXFF {
		out.Header.Set("X-Forwarded-For", plan.inboundXFF+", "+plan.clientIP.String())
	} else {
		out.Header.Set("X-Forwarded-For", plan.clientIP.String())
	}
	out.Header.Set("X-Real-IP", plan.clientIP.String())
	out.Header.Set("X-Forwarded-Proto", scheme)
	out.Header.Set("X-Forwarded-Host", plan.originalHost)
	out.Header.Set("X-Forwarded-Port", portOf(plan.originalHost, scheme))
	out.Header.Set("X-Goacos-Upstream", plan.upstream)
	out.Header.Set("X-Goacos-Component", t.AppName+"/"+t.ComponentName)

	// Upstreams see the host they were actually reached on. Forwarding the
	// public name instead would break every upstream that builds absolute URLs
	// from it, and would also make the gateway's own vhost rules apply twice.
	out.Host = plan.upstream
}

func removeHopByHop(h http.Header) {
	// Connection is read before anything is deleted. Removing the fixed list
	// first would throw away the very header that names the per-request
	// hop-by-hop fields, so "Connection: X-Internal-Token" would silently
	// forward X-Internal-Token to the upstream.
	var named []string
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if name := strings.TrimSpace(token); name != "" {
				named = append(named, name)
			}
		}
	}
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade",
	} {
		h.Del(name)
	}
	for _, name := range named {
		h.Del(name)
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func portOf(host, scheme string) string {
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// liveAddresses collects every instance address currently routable, so the
// transport pool can be pruned on each refresh.
func liveAddresses(table *Table) map[string]bool {
	live := map[string]bool{}
	for _, domain := range table.Domains {
		for _, target := range table.allTargets(domain) {
			for _, inst := range target.Instances {
				live[addr(inst)] = true
			}
		}
	}
	return live
}

// sortedAddrs renders an instance set for status output.
func sortedAddrs(instances []Instance) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, addr(i))
	}
	return out
}
