package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// Gateway is the running data plane.
type Gateway struct {
	cfg    *config.Config
	st     *storage.Store
	ns     string
	table  *Table
	certs  *CertManager
	picker *Picker
	pool   *transportPool

	httpServer  *http.Server
	httpsServer *http.Server

	// stats are exposed for the console. They are counters, not metrics: the
	// question an operator asks is "is traffic flowing and where", not "what
	// is my p99".
	requests   atomic.Int64
	proxied    atomic.Int64
	staticHits atomic.Int64
	redirects  atomic.Int64
	rejected   atomic.Int64
	noRoute    atomic.Int64

	wg sync.WaitGroup
}

// New builds a gateway against an already-open store.
//
// The certificate manager is built here because a gateway that cannot answer a
// handshake must not start listening on 443: failing at construction turns a
// TLS misconfiguration into a refused start rather than a running process
// serving errors.
func New(ctx context.Context, cfg *config.Config, st *storage.Store) (*Gateway, error) {
	ns := cfg.GatewayNamespace
	if ns == "" {
		ns = "public"
	}
	certs, err := NewCertManager(ctx, cfg, st, ns)
	if err != nil {
		return nil, fmt.Errorf("certificate manager: %w", err)
	}
	g := &Gateway{
		cfg:    cfg,
		st:     st,
		ns:     ns,
		table:  NewTable(),
		certs:  certs,
		picker: NewPicker(),
		pool:   newTransportPool(),
	}
	if err := g.table.Refresh(ctx, st, ns); err != nil {
		// A gateway starting against an unreachable database would serve
		// 404 for everything, which looks like a routing mistake rather than
		// a dependency failure. Refuse to start instead.
		return nil, fmt.Errorf("load routing table: %w", err)
	}
	return g, nil
}

// Table exposes the routing snapshot for status reporting.
func (g *Gateway) Table() *Table { return g.table }

// Certs exposes the certificate manager for the management API.
func (g *Gateway) Certs() *CertManager { return g.certs }

// Run starts the listeners and blocks until the context is cancelled.
//
// Binding :80 and :443 usually needs privileges. When binding fails the
// gateway logs it and continues with whatever did bind, because half a gateway
// that answers on 443 is more useful than none, and the operator needs to see
// which listener is missing rather than a crash loop.
func (g *Gateway) Run(ctx context.Context) error {
	if g.cfg.GatewayHTTPAddr != "" {
		g.startHTTP(ctx)
	}
	if g.cfg.GatewayHTTPSAddr != "" {
		g.startHTTPS(ctx)
	}

	interval := time.Duration(g.cfg.GatewayCertRefreshSeconds) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go g.certs.Run(ctx, g.table)
	go g.refreshLoop(ctx, interval)

	<-ctx.Done()
	return g.shutdown()
}

func (g *Gateway) startHTTP(ctx context.Context) {
	srv := &http.Server{
		Addr:              g.cfg.GatewayHTTPAddr,
		Handler:           g.HTTPServe(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	g.httpServer = srv
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[gateway] HTTP listener stopped: %v", err)
		}
	}()
	log.Printf("[gateway] http listening on %s (force_https=%v)", g.cfg.GatewayHTTPAddr, g.cfg.GatewayForceHTTPS)
	_ = ctx
}

func (g *Gateway) startHTTPS(ctx context.Context) {
	srv := &http.Server{
		Addr:              g.cfg.GatewayHTTPSAddr,
		Handler:           g.TLSServe(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: a websocket or a long-poll would be cut off at a
		// fixed wall-clock deadline regardless of activity. Upstream
		// liveness is bounded per request by the transport timeouts instead.
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: g.certs.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
			// Session tickets are disabled so that a certificate replacement
			// takes effect for returning clients instead of being masked by
			// a resumed session built on the old key.
			SessionTicketsDisabled: true,
		},
	}
	g.httpsServer = srv
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[gateway] HTTPS listener stopped: %v", err)
		}
	}()
	log.Printf("[gateway] https listening on %s", g.cfg.GatewayHTTPSAddr)
	_ = ctx
}

// HTTPServe is the plain-HTTP handler: ACME challenges, then either a redirect
// to HTTPS or the same dispatch the TLS listener performs.
func (g *Gateway) HTTPServe() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Challenges are answered before any policy decision. A challenge that
		// is redirected is a failed validation, and a failed validation costs
		// a rate-limit slot that the operator cannot see.
		if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
			if g.certs.ServeChallenge(w, token) {
				return
			}
			http.NotFound(w, r)
			return
		}

		host := r.Host
		pol := resolvePolicy(g.cfg, g.table.Policy(host))
		if !g.admit(w, r, pol) {
			return
		}
		if pol.ForceHTTPS {
			g.redirects.Add(1)
			g.redirectToHTTPS(w, r)
			return
		}
		g.dispatch(w, r, false)
	})
}

// TLSServe is the HTTPS handler.
func (g *Gateway) TLSServe() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pol := resolvePolicy(g.cfg, g.table.Policy(r.Host)); pol.HSTSMaxAge > 0 {
			w.Header().Set("Strict-Transport-Security",
				fmt.Sprintf("max-age=%d; includeSubDomains", pol.HSTSMaxAge))
		}
		g.dispatch(w, r, true)
	})
}

// dispatch resolves a route and hands the request to static delivery or the
// proxy. secure is whether this request arrived over TLS.
func (g *Gateway) dispatch(w http.ResponseWriter, r *http.Request, secure bool) {
	g.requests.Add(1)

	pol := resolvePolicy(g.cfg, g.table.Policy(r.Host))
	if !g.admit(w, r, pol) {
		return
	}

	target := g.table.Lookup(r.Host, r.URL.Path, metaFiltersFrom(r))
	if target == nil {
		g.noRoute.Add(1)
		writeGatewayError(w, http.StatusNotFound,
			fmt.Sprintf("no route declared for %s%s", r.Host, r.URL.Path))
		return
	}

	// The certificate in use is echoed back so an operator troubleshooting a
	// browser warning can see which material actually served them, without
	// having to open the certificate viewer.
	if info, ok := g.certs.Info(r.Host); ok {
		w.Header().Set("X-Goacos-Cert-Source", info.Source)
		w.Header().Set("X-Goacos-Cert-Expiry", info.NotAfter.Format(time.RFC3339))
	}
	w.Header().Set("X-Goacos-Component", target.AppName+"/"+target.ComponentName)

	if !secure && pol.ForceHTTPS {
		// Reached when force-HTTPS is off for this domain but on by default;
		// kept so a policy change mid-request cannot serve content in clear.
		g.redirects.Add(1)
		g.redirectToHTTPS(w, r)
		return
	}

	if pol.ClientMaxBodyBytes > 0 && r.ContentLength > pol.ClientMaxBodyBytes {
		g.rejected.Add(1)
		rejectBody(pol.ClientMaxBodyBytes).ServeHTTP(w, r)
		return
	}
	if pol.ClientMaxBodyBytes > 0 {
		// A chunked request has no Content-Length, so the limit can only be
		// enforced while reading.
		r.Body = http.MaxBytesReader(w, r.Body, pol.ClientMaxBodyBytes)
	}

	if g.servesStatic(target) {
		if g.ServeStatic(w, r, target) {
			g.staticHits.Add(1)
			return
		}
	}
	g.proxied.Add(1)
	g.Forward(w, r, target, pol)
}

// admit applies the domain's access rules. It returns false once it has
// written a rejection.
func (g *Gateway) admit(w http.ResponseWriter, r *http.Request, pol Policy) bool {
	if len(pol.Allow) == 0 && len(pol.Deny) == 0 {
		return true
	}
	if pol.Allowed(ClientAddr(r, g.cfg.GatewayTrustForwardedFor)) {
		return true
	}
	g.rejected.Add(1)
	writeGatewayError(w, http.StatusForbidden, "client address is not permitted for this domain")
	return false
}

// metaFiltersFrom reads a canary or lane selector from the request.
//
// The key is namespaced so a normal query parameter cannot select a metadata
// lane by accident; only the gateway's own convention does.
func metaFiltersFrom(r *http.Request) map[string]string {
	raw := r.URL.Query().Get("metaFilter")
	if raw == "" {
		raw = r.Header.Get("X-Goacos-Meta-Filter")
	}
	return parseMetaFilter(raw)
}

// redirectToHTTPS sends a 301 that preserves the full path and query.
//
// The port comes from this gateway's own TLS listener, not from the request.
// A client cannot know that the HTTPS side moved to 8443 while HTTP stayed on
// 80, so a redirect built from the request's port would send every browser to
// 443 — where nothing is listening — which presents as "the site redirects and
// then fails" with nothing in the logs to explain it.
//
// 308 is deliberately not used: browsers cache a 308 permanently per entry, so
// an operator who turns force-HTTPS off afterwards still sees redirects until
// the cache ages out.
func (g *Gateway) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := storage.NormalizeDomain(hostOnly(r.Host))
	target := "https://" + host + g.httpsPortSuffix() + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// httpsPortSuffix renders the TLS listener's port when it is not the default.
//
// A gateway deliberately bound to a non-standard port exists in development
// and in side-by-side migrations, and in both cases a portless redirect is
// simply wrong.
func (g *Gateway) httpsPortSuffix() string {
	_, port, err := net.SplitHostPort(g.cfg.GatewayHTTPSAddr)
	if err != nil || port == "" || port == "443" {
		return ""
	}
	return ":" + port
}

// refreshLoop rebuilds the routing snapshot on a timer.
//
// The routing table is deliberately a timer-driven cache rather than something
// rebuilt per request: the whole reason it exists is to keep the database off
// the hot path of inbound traffic.
func (g *Gateway) refreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.refresh(ctx)
		}
	}
}

// Refresh rebuilds the routing table immediately.
func (g *Gateway) Refresh(ctx context.Context) { g.refresh(ctx) }

func (g *Gateway) refresh(ctx context.Context) {
	if err := g.table.Refresh(ctx, g.st, g.ns); err != nil {
		// The previous snapshot stays in place. A transient database failure
		// must not empty the routing table and take every domain offline.
		log.Printf("[gateway] routing table refresh failed, keeping previous snapshot: %v", err)
		return
	}
	g.pool.prune(liveAddresses(g.table))
	live := map[string]bool{}
	for _, d := range g.table.Domains {
		live[d] = true
	}
	g.picker.Forget(live)
}

// shutdown stops both listeners and releases every pooled connection.
func (g *Gateway) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.pool.closeAll()

	var firstErr error
	if g.httpsServer != nil {
		if err := g.httpsServer.Shutdown(ctx); err != nil {
			firstErr = err
		}
	}
	if g.httpServer != nil {
		if err := g.httpServer.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	return firstErr
}

// TargetsFor returns every declared target for one domain, most specific path
// prefix first. The console uses it to show the whole picture for a domain
// rather than whichever single prefix happens to be probed.
func (g *Gateway) TargetsFor(domain string) []*Target { return g.table.allTargets(domain) }

// Counters returns the traffic counters for the status endpoint.
func (g *Gateway) Counters() map[string]any {
	return map[string]any{
		"requests":        g.requests.Load(),
		"proxied":         g.proxied.Load(),
		"static":          g.staticHits.Load(),
		"redirects":       g.redirects.Load(),
		"rejected":        g.rejected.Load(),
		"noRoute":         g.noRoute.Load(),
		"pooledUpstreams": g.pool.size(),
	}
}

// DaysLeft reports whole days until t, negative once t has passed. It is
// exported because the management API reports the same number the gateway
// renews on, and two different calculations would eventually disagree.
func DaysLeft(t time.Time) int { return int(time.Until(t).Hours() / 24) }

// writeGatewayError renders a gateway failure in the same envelope the rest of
// goacos uses, so a client parsing errors does not need to special-case the
// data plane.
func writeGatewayError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%d,"message":%q,"data":null}`, status, msg)
}
