package gateway

// Per-domain traffic policy: how a request is admitted and how it is carried.
//
// The gateway-wide configuration supplies defaults; a gateway_vhost row
// overrides them for one domain. The merge is deliberately field-by-field
// rather than whole-row replacement, because an operator who wants to add HSTS
// to a single domain should not have to restate every timeout they were already
// getting from the defaults.

import (
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// Policy is the effective rule set for one request.
type Policy struct {
	ForceHTTPS         bool
	HSTSMaxAge         int
	Allow              []netip.Prefix
	Deny               []netip.Prefix
	ClientMaxBodyBytes int64
	ConnectTimeoutMS   int
	ReadTimeoutMS      int
	SendTimeoutMS      int
	Websocket          bool
	// Path-level controls. See security.go for why they exist and why a
	// malformed entry is dropped rather than fatal.
	SecurityHeaders   []SecurityHeader
	BlockUserAgents   []*regexp.Regexp
	BlockPaths        []*regexp.Regexp
}

// resolvePolicy merges the gateway defaults with a domain's row, if any.
func resolvePolicy(cfg *config.Config, v *storage.VHostPolicy) Policy {
	p := Policy{
		ForceHTTPS:         cfg.GatewayForceHTTPS,
		HSTSMaxAge:         cfg.GatewayHSTSMaxAge,
		ClientMaxBodyBytes: cfg.GatewayClientMaxBodyBytes,
		ConnectTimeoutMS:   cfg.GatewayConnectTimeoutMS,
		ReadTimeoutMS:      cfg.GatewayReadTimeoutMS,
		SendTimeoutMS:      cfg.GatewaySendTimeoutMS,
		Websocket:          cfg.GatewayWebsocket,
		SecurityHeaders:    parseSecurityHeaders(cfg.GatewaySecurityHeaders),
		BlockUserAgents:    compilePatterns(cfg.GatewayBlockUserAgents),
		BlockPaths:         compilePatterns(cfg.GatewayBlockPaths),
	}
	if v != nil {
		p.ForceHTTPS = v.ForceHTTPS
		if v.HSTSMaxAge > 0 {
			p.HSTSMaxAge = v.HSTSMaxAge
		}
		p.Allow = parsePrefixList(v.AllowIPs)
		p.Deny = parsePrefixList(v.DenyIPs)
		if v.ClientMaxBodyBytes > 0 {
			p.ClientMaxBodyBytes = v.ClientMaxBodyBytes
		}
		if v.ConnectTimeoutMS > 0 {
			p.ConnectTimeoutMS = v.ConnectTimeoutMS
		}
		if v.ReadTimeoutMS > 0 {
			p.ReadTimeoutMS = v.ReadTimeoutMS
		}
		if v.SendTimeoutMS > 0 {
			p.SendTimeoutMS = v.SendTimeoutMS
		}
		p.Websocket = v.Websocket
	}
	return p
}

// parsePrefixList reads newline-separated CIDRs or bare addresses.
//
// A malformed entry is skipped rather than failing the whole domain: an
// operator mid-edit who fat-fingers one line must not take every request to
// that domain off the air. The trade-off is that a typo in a deny rule fails
// open for that one entry, which is why ParsePrefixErrors is reported to the
// status endpoint rather than swallowed.
func parsePrefixList(raw string) []netip.Prefix {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []netip.Prefix
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' }) {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		p, err := parsePrefix(s)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// parsePrefix accepts both "10.0.0.0/8" and a bare "10.0.0.1", which is how
// operators actually write these lists.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Allowed reports whether a client address may reach the domain.
//
// Deny is evaluated first and wins. With both lists populated the alternative
// — allow-first — makes a deny rule unusable for every client the allow list
// already covers, which is the common case for an admin blocklist.
func (p Policy) Allowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		// An unparseable client address cannot be matched against any rule.
		// Refusing is the correct direction: this is the gateway's outermost
		// access control, and "I do not know who you are" is not consent.
		return len(p.Allow) == 0
	}
	for _, prefix := range p.Deny {
		if prefix.Contains(addr) {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return true
	}
	for _, prefix := range p.Allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientAddr returns the address access rules should be evaluated against.
//
// X-Forwarded-For is honoured only when the operator has explicitly enabled
// trust for it. It is client-supplied: believing it while directly exposed lets
// anyone spoof a source address and walk straight through an allow list.
func ClientAddr(r *http.Request, trustForwardedFor bool) netip.Addr {
	if trustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Left-most entry is the original client as recorded by the
			// closest proxy that saw it.
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if addr, err := netip.ParseAddr(first); err == nil {
				return addr.Unmap()
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if addr, err := netip.ParseAddr(xr); err == nil {
				return addr.Unmap()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return addr.Unmap()
	}
	return netip.Addr{}
}

// parseMetaFilter reads "k=v,k2=v2" and selects a canary or lane by instance
// metadata.
//
// A malformed pair is dropped rather than failing the whole request: a
// partially applied filter is still a narrower, well-defined answer, whereas an
// error would take a live route offline because someone typed a comma wrong.
func parseMetaFilter(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rejectBody records why a body was refused. It exists so the response tells an
// operator which limit fired instead of a bare 413, which is indistinguishable
// from a 413 raised by the upstream.
func rejectBody(limit int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"code":413,"message":"request body exceeds this domain's limit of ` +
			itoa(int(limit)) + ` bytes","data":null}`))
	}
}
