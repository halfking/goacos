package api

// Gateway management API.
//
// # What lives here
//
// The gateway's data plane is driven entirely by data — routes, components,
// certificates and per-domain policy. This file is the console's and curl's
// view of that data. It contains no traffic-handling logic on purpose: the
// only thing the management surface does is change what the database says, and
// the data plane converges on it within one refresh interval.
//
// # Why certificates never return a private key
//
// Certificate listings report source, issuer, validity and expiry. The key
// material is write-only. Returning it would make a read endpoint a credential
// exfiltration target, and no caller needs it: the gateway already holds the
// key it needs in memory.
//
// # Why a status endpoint is worth building carefully
//
// "Is my site up" is answered here rather than by curling the site. It reports
// the routing snapshot's age, each domain's certificate source and days left,
// ACME backoff state, and which upstreams are live — the four things an
// operator checks in that order during an incident.

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/gateway"
	"github.com/halfking/goacos/internal/storage"
)

// ---------- status ----------

// gatewayStatus answers "is the gateway doing its job, and what is it doing".
func (s *Server) gatewayStatus(w http.ResponseWriter, r *http.Request) {
	g := s.Gateway
	if g == nil {
		writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
			"enabled": false,
			"reason":  "gateway data plane is disabled; set GOACOS_GATEWAY_ENABLED=true",
		}})
		return
	}
	loadedAt, refreshErr := g.Table().Snapshot()

	domains := make([]map[string]any, 0)
	for _, domain := range g.Table().Domains {
		entry := map[string]any{"domain": domain}
		// Every declared prefix for the domain is reported, because a domain
		// whose /ui route points at a component with no instances is a
		// different problem from a domain with no route at all.
		for _, t := range g.TargetsFor(domain) {
			entry[t.PathPrefix] = map[string]any{
				"app":       t.AppName,
				"component": t.ComponentName,
				"kind":      t.Kind,
				"protocol":  t.Protocol,
				"service":   t.ServiceName,
				"group":     t.GroupName,
				"lb":        t.LBAlgorithm,
				"instances": sortedAddresses(t.Instances),
				"healthy":   len(t.Instances),
			}
		}
		if info, ok := g.Certs().Info(domain); ok {
			entry["tls"] = info.Source
			entry["tlsIssuer"] = info.Issuer
			entry["tlsExpiry"] = info.NotAfter.Format(time.RFC3339)
			entry["tlsDaysLeft"] = info.DaysLeft()
			if info.FilePath != "" {
				// A file-sourced certificate is managed outside this server.
				// Saying where it came from is what tells an operator to go
				// fix the other tool rather than re-issue here.
				entry["tlsPath"] = info.FilePath
				entry["tlsNote"] = "managed outside goacos; it is never auto-renewed here"
			}
			if info.Source == storage.CertSourceSelfSigned {
				entry["tlsWarning"] = "running on a self-signed certificate; browsers will not trust this domain"
			}
			if info.Expired() {
				entry["tlsWarning"] = "certificate has expired"
			}
		} else {
			entry["tls"] = "none"
			entry["tlsWarning"] = "no certificate yet; issuance pending or not eligible"
		}
		domains = append(domains, entry)
	}

	certs := make([]map[string]any, 0)
	for _, info := range g.Certs().ListInfo() {
		certs = append(certs, map[string]any{
			"domain":    info.Domain,
			"source":    info.Source,
			"issuer":    info.Issuer,
			"notBefore": info.NotBefore.Format(time.RFC3339),
			"notAfter":  info.NotAfter.Format(time.RFC3339),
			"daysLeft":  info.DaysLeft(),
			"expired":   info.Expired(),
			"status":    info.Status,
			"lastError": info.LastError,
		})
	}

	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"enabled":   true,
		"namespace": s.Cfg.GatewayNamespace,
		"listeners": map[string]string{
			"http":  s.Cfg.GatewayHTTPAddr,
			"https": s.Cfg.GatewayHTTPSAddr,
		},
		"policyDefaults": map[string]any{
			"forceHttps":        s.Cfg.GatewayForceHTTPS,
			"hstsMaxAge":        s.Cfg.GatewayHSTSMaxAge,
			"maxBodyBytes":      s.Cfg.GatewayClientMaxBodyBytes,
			"connectTimeoutMs":  s.Cfg.GatewayConnectTimeoutMS,
			"readTimeoutMs":     s.Cfg.GatewayReadTimeoutMS,
			"sendTimeoutMs":     s.Cfg.GatewaySendTimeoutMS,
			"websocket":         s.Cfg.GatewayWebsocket,
			"trustForwardedFor": s.Cfg.GatewayTrustForwardedFor,
			"staticRoot":        s.Cfg.GatewayStaticRoot,
			"staticFallback":    s.Cfg.GatewayStaticFallback,
			"refreshSeconds":    s.Cfg.GatewayCertRefreshSeconds,
			"certDir":           s.Cfg.GatewayCertDir,
			"certWriteBack":     s.Cfg.GatewayCertWriteBack,
			"certScanSeconds":   s.Cfg.GatewayCertScanSeconds,
		},
		"routing": map[string]any{
			"loadedAt":    loadedAt.Format(time.RFC3339),
			"ageSeconds":  int(time.Since(loadedAt).Seconds()),
			"domainCount": len(g.Table().Domains),
			"error":       errText(refreshErr),
		},
		"acme": g.Certs().Status(time.Now()),
		"certDir": map[string]any{
			"path":          s.Cfg.GatewayCertDir,
			"writeBack":     s.Cfg.GatewayCertWriteBack,
			"scanSeconds":   s.Cfg.GatewayCertScanSeconds,
			"servedFromDir": g.Certs().CertDirSources(),
			"readable":      certDirReadable(s.Cfg.GatewayCertDir),
		},
		"domains":       domains,
		"certificates":  certs,
		"counters":      s.gatewayCounters(),
		"uptimeSeconds": int(time.Since(s.Started).Seconds()),
	}})
}

// certDirReadable reports whether the mounted directory can actually be read.
//
// This is a separate signal from "is it configured" because the usual failure
// is a permission problem: the image runs as uid 10001, so a host directory
// owned by root with restrictive modes looks configured and serves nothing.
func certDirReadable(dir string) any {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	return true
}

func (s *Server) gatewayCounters() map[string]any {
	g := s.Gateway
	if g == nil {
		return map[string]any{}
	}
	return g.Counters()
}

// gatewayRefresh forces an immediate routing reload.
//
// It exists for the moment right after a route change when an operator does
// not want to wait out the refresh interval to confirm it took effect.
func (s *Server) gatewayRefresh(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		appError(w, http.StatusBadRequest, 400, "gateway is not enabled")
		return
	}
	s.Gateway.Refresh(r.Context())
	loadedAt, err := s.Gateway.Table().Snapshot()
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"reloadedAt": loadedAt.Format(time.RFC3339),
		"error":      errText(err),
	}})
}

// ---------- certificates ----------

func (s *Server) certList(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	rows, err := s.ST.ListCertificates(r.Context(), ns)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"namespaceId":   row.NamespaceId,
			"domain":        storage.NormalizeDomain(row.Domain),
			"source":        row.Source,
			"issuer":        row.Issuer,
			"notBefore":     row.NotBefore.Format(time.RFC3339),
			"notAfter":      row.NotAfter.Format(time.RFC3339),
			"daysLeft":      gateway.DaysLeft(row.NotAfter),
			"status":        row.Status,
			"lastError":     row.LastError,
			"autoRenew":     row.Source == storage.CertSourceACME,
			"sourceWarning": certWarning(row),
		})
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: out})
}

// certWarning states the one thing an operator must not miss about a
// certificate. Empty means nothing needs saying.
func certWarning(row storage.Certificate) string {
	switch {
	case row.Status == storage.CertStatusFailed:
		return "issuance failed: " + row.LastError
	case row.Source == storage.CertSourceSelfSigned:
		return "self-signed: browsers will not trust this domain until ACME issues or an operator uploads a certificate"
	case gateway.DaysLeft(row.NotAfter) < 0:
		return "certificate has expired"
	case gateway.DaysLeft(row.NotAfter) <= storage.DefaultRenewBeforeDays && row.Source == storage.CertSourceACME:
		return "inside the renewal window"
	}
	return ""
}

// certWrite uploads a certificate for one domain.
//
// Manual certificates deliberately opt out of every automatic path. An
// operator uploading from an internal CA knows something this gateway cannot
// infer, and replacing it with a public-CA certificate would be worse than
// doing nothing.
func (s *Server) certWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NamespaceId string `json:"namespaceId"`
		Domain      string `json:"domain"`
		CertPem     string `json:"certPem"`
		KeyPem      string `json:"keyPem"`
	}
	if err := jsonDecode(r, &body); err != nil {
		f := formOrJSON(r)
		body.NamespaceId = f["namespaceId"]
		body.Domain = f["domain"]
		body.CertPem = f["certPem"]
		body.KeyPem = f["keyPem"]
	}
	if body.Domain == "" {
		appError(w, http.StatusBadRequest, 400, "domain is required")
		return
	}
	if body.CertPem == "" || body.KeyPem == "" {
		appError(w, http.StatusBadRequest, 400, "certPem and keyPem are both required")
		return
	}
	ns := defaultNamespace(body.NamespaceId)

	var info gateway.CertInfo
	var err error
	if s.Gateway != nil {
		info, err = s.Gateway.Certs().StoreManual(r.Context(), body.Domain, body.CertPem, body.KeyPem)
	} else {
		err = s.storeManualWithoutGateway(r, ns, body.Domain, body.CertPem, body.KeyPem)
	}
	if err != nil {
		// A certificate that does not cover its own domain, or a key that
		// does not match the certificate, is refused here rather than served.
		appError(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"domain":   info.Domain,
		"source":   storage.CertSourceManual,
		"notAfter": info.NotAfter.Format(time.RFC3339),
		"daysLeft": info.DaysLeft(),
		"note":     "manual certificates are never auto-renewed; upload a replacement before expiry",
	}})
}

// storeManualWithoutGateway persists an upload on an instance where the data
// plane is disabled, so certificate configuration can be staged ahead of
// turning the listeners on.
func (s *Server) storeManualWithoutGateway(r *http.Request, ns, domain, certPEM, keyPEM string) error {
	_, err := s.ST.UpsertCertificate(r.Context(), storage.Certificate{
		NamespaceId: ns,
		Domain:      storage.NormalizeDomain(domain),
		Source:      storage.CertSourceManual,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	})
	return err
}

func (s *Server) certDelete(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	domain := storage.NormalizeDomain(r.PathValue("domain"))
	if err := s.ST.DeleteCertificate(r.Context(), ns, domain); err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	// Whether the domain is currently served from the shared directory has to
	// be established before the cache entry is dropped: afterwards the gateway
	// has no way to know, and the operator would read a successful delete as a
	// revocation while a certificate keeps being served from disk.
	fromFile := false
	if s.Gateway != nil {
		if info, ok := s.Gateway.Certs().Info(domain); ok {
			fromFile = info.Source == storage.CertSourceFile || info.FilePath != ""
		}
		s.Gateway.Certs().Forget(domain)
	}

	note := "any serving domain will fall back to a self-signed certificate until issuance succeeds"
	if fromFile {
		note = "database copy deleted, but " + domain + " is still served from " +
			s.Cfg.GatewayCertDir + " — remove the file there to complete the revocation"
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"deleted": domain,
		"note":    note,
	}})
}

// ---------- vhost policy ----------

func (s *Server) vhostList(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	rows, err := s.ST.ListVHosts(r.Context(), ns)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, v := range rows {
		out = append(out, vhostJSON(v))
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: out})
}

func vhostJSON(v storage.VHostPolicy) map[string]any {
	return map[string]any{
		"namespaceId":        v.NamespaceId,
		"domain":             storage.NormalizeDomain(v.Domain),
		"forceHttps":         v.ForceHTTPS,
		"hstsMaxAge":         v.HSTSMaxAge,
		"allowIps":           v.AllowIPs,
		"denyIps":            v.DenyIPs,
		"clientMaxBodyBytes": v.ClientMaxBodyBytes,
		"connectTimeoutMs":   v.ConnectTimeoutMS,
		"readTimeoutMs":      v.ReadTimeoutMS,
		"sendTimeoutMs":      v.SendTimeoutMS,
		"websocket":          v.Websocket,
		"metadata":           v.Metadata,
	}
}

func (s *Server) vhostWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NamespaceId string `json:"namespaceId"`
		Domain      string `json:"domain"`
		ForceHTTPS  *bool  `json:"forceHttps"`
		HSTSMaxAge  int    `json:"hstsMaxAge"`
		// Pointer rather than string: an omitted key means "leave this rule
		// alone", while an explicit "" means "remove it". Without that
		// distinction a misconfigured deny list could never be lifted — the
		// operator would have to delete the whole vhost and lose every other
		// setting with it.
		AllowIPs           *string           `json:"allowIps"`
		DenyIPs            *string           `json:"denyIps"`
		ClientMaxBodyBytes int64             `json:"clientMaxBodyBytes"`
		ConnectTimeoutMS   int               `json:"connectTimeoutMs"`
		ReadTimeoutMS      int               `json:"readTimeoutMs"`
		SendTimeoutMS      int               `json:"sendTimeoutMs"`
		Websocket          *bool             `json:"websocket"`
		Metadata           map[string]string `json:"metadata"`
	}
	if err := jsonDecode(r, &body); err != nil {
		f := formOrJSON(r)
		body.NamespaceId = f["namespaceId"]
		body.Domain = f["domain"]
		body.HSTSMaxAge = atoiDefault(f["hstsMaxAge"], 0)
		if v, ok := f["allowIps"]; ok {
			body.AllowIPs = &v
		}
		if v, ok := f["denyIps"]; ok {
			body.DenyIPs = &v
		}
		body.ClientMaxBodyBytes = int64(atoiDefault(f["clientMaxBodyBytes"], 0))
		body.ConnectTimeoutMS = atoiDefault(f["connectTimeoutMs"], 0)
		body.ReadTimeoutMS = atoiDefault(f["readTimeoutMs"], 0)
		body.SendTimeoutMS = atoiDefault(f["sendTimeoutMs"], 0)
		if v, ok := parseBoolPtr(f["forceHttps"]); ok {
			body.ForceHTTPS = v
		}
		if v, ok := parseBoolPtr(f["websocket"]); ok {
			body.Websocket = v
		}
	}
	// The path segment wins when present, so PUT /vhosts/{domain} addresses
	// exactly what its URL says. Ignoring it and demanding the same value in
	// the body would mean two sources of truth for one identity — and the kind
	// of request that silently edits the wrong domain when they disagree.
	if fromPath := storage.NormalizeDomain(r.PathValue("domain")); fromPath != "" {
		body.Domain = fromPath
	}
	if body.Domain == "" {
		appError(w, http.StatusBadRequest, 400, "domain is required in the path or the body")
		return
	}
	ns := defaultNamespace(body.NamespaceId)

	// Start from the effective current policy so an omitted field keeps its
	// value. Replacing the whole row would silently reset every timeout the
	// operator had not mentioned in this particular request.
	current, err := s.ST.GetVHost(r.Context(), ns, body.Domain)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	policy := storage.VHostPolicy{
		NamespaceId:        ns,
		Domain:             storage.NormalizeDomain(body.Domain),
		ForceHTTPS:         s.Cfg.GatewayForceHTTPS,
		ConnectTimeoutMS:   s.Cfg.GatewayConnectTimeoutMS,
		ReadTimeoutMS:      s.Cfg.GatewayReadTimeoutMS,
		SendTimeoutMS:      s.Cfg.GatewaySendTimeoutMS,
		Websocket:          s.Cfg.GatewayWebsocket,
		ClientMaxBodyBytes: s.Cfg.GatewayClientMaxBodyBytes,
		Metadata:           body.Metadata,
	}
	if current != nil {
		policy.ForceHTTPS = current.ForceHTTPS
		policy.HSTSMaxAge = current.HSTSMaxAge
		policy.AllowIPs = current.AllowIPs
		policy.DenyIPs = current.DenyIPs
		policy.ClientMaxBodyBytes = current.ClientMaxBodyBytes
		policy.ConnectTimeoutMS = current.ConnectTimeoutMS
		policy.ReadTimeoutMS = current.ReadTimeoutMS
		policy.SendTimeoutMS = current.SendTimeoutMS
		policy.Websocket = current.Websocket
		if body.Metadata == nil {
			policy.Metadata = current.Metadata
		}
	}
	if body.ForceHTTPS != nil {
		policy.ForceHTTPS = *body.ForceHTTPS
	}
	if body.Websocket != nil {
		policy.Websocket = *body.Websocket
	}
	if body.HSTSMaxAge > 0 {
		policy.HSTSMaxAge = body.HSTSMaxAge
	}
	if body.AllowIPs != nil {
		policy.AllowIPs = *body.AllowIPs
	}
	if body.DenyIPs != nil {
		policy.DenyIPs = *body.DenyIPs
	}
	if body.ClientMaxBodyBytes > 0 {
		policy.ClientMaxBodyBytes = body.ClientMaxBodyBytes
	}
	if body.ConnectTimeoutMS > 0 {
		policy.ConnectTimeoutMS = body.ConnectTimeoutMS
	}
	if body.ReadTimeoutMS > 0 {
		policy.ReadTimeoutMS = body.ReadTimeoutMS
	}
	if body.SendTimeoutMS > 0 {
		policy.SendTimeoutMS = body.SendTimeoutMS
	}

	created, err := s.ST.UpsertVHost(r.Context(), policy)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if s.Gateway != nil {
		s.Gateway.Refresh(r.Context())
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"created": created,
		"policy":  vhostJSON(policy),
	}})
}

func (s *Server) vhostDelete(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	domain := storage.NormalizeDomain(r.PathValue("domain"))
	if err := s.ST.DeleteVHost(r.Context(), ns, domain); err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if s.Gateway != nil {
		s.Gateway.Refresh(r.Context())
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"deleted": domain,
		"note":    "the domain now inherits the gateway-wide defaults",
	}})
}

// ---------- helpers ----------

// sortedAddresses renders an instance list for status output.
func sortedAddresses(instances []gateway.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, i.IP+":"+strconv.Itoa(int(i.Port)))
	}
	return out
}

func errText(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func parseBoolPtr(s string) (*bool, bool) {
	if s == "" {
		return nil, false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return nil, false
	}
	return &v, true
}
