package gateway

// Path-level traffic controls: the half of a fronting nginx's job that an
// IP-level Allow/Deny cannot express.
//
// The gateway already knows how to say "who may connect" (Policy.Allow/Deny).
// It had no answer for "once connected, what may they touch". That gap is not
// theoretical: 42 production domains are covered by
// 00-security-hardening.snippet, which supplies four security response headers,
// a scanner-user-agent block and a sensitive-path block. Moving any of those
// domains onto this gateway dropped all of it — silently, because nothing in
// the request path ever failed.
//
// Failure status codes follow nginx's snippet on purpose, so a scanner sees the
// same thing either side of a cutover:
//   · scanner user agent → 403
//   · sensitive path     → 404 (not 403: probing /.git/config should learn
//     nothing about whether the path exists)
//
// A malformed entry is skipped rather than failing the whole domain, matching
// parsePrefixList's trade-off: an operator mid-edit who fat-fingers one line
// must not take every request to that domain off the air. The cost is that a
// typo in one rule fails open for that one rule.

import (
	"net/http"
	"regexp"
	"strings"
)

// SecurityHeader is one "Name: value" response header applied to every gateway
// response, mirroring nginx's `add_header ... always`.
type SecurityHeader struct {
	Name  string
	Value string
}

// parseSecurityHeaders reads "Name: value" lines.
//
// The split is on the FIRST colon so a value may contain colons (a CSP
// contains several). A line with no colon, or an empty name, is skipped.
func parseSecurityHeaders(raw []string) []SecurityHeader {
	var out []SecurityHeader
	for _, line := range raw {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if name == "" {
			continue
		}
		out = append(out, SecurityHeader{Name: name, Value: value})
	}
	return out
}

// compilePatterns compiles each pattern, dropping the ones that do not compile.
//
// Returning the valid subset is the same fail-open choice as above: one bad
// regexp must not disable the other rules, and must certainly not disable
// serving.
func compilePatterns(raw []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, pat := range raw {
		if strings.TrimSpace(pat) == "" {
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// applyHeaders writes the domain's security headers.
//
// It runs before anything writes a status, because a header set after
// WriteHeader is discarded — which is exactly the failure this replaces, where
// a response left nginx without nosniff at all.
func (p Policy) applyHeaders(w http.ResponseWriter) {
	for _, h := range p.SecurityHeaders {
		w.Header().Set(h.Name, h.Value)
	}
}

// rejectSensitiveRequest applies the scanner-UA and sensitive-path rules.
//
// Both run before route lookup: a request that would be refused must not
// consume a route resolution, and more importantly must not be forwarded even
// if some component later matches it. The ACME prefix is already answered
// earlier in Serve, so it never reaches here.
func (p Policy) rejectSensitiveRequest(w http.ResponseWriter, r *http.Request) bool {
	if len(p.BlockUserAgents) > 0 && matchesAny(p.BlockUserAgents, r.UserAgent()) {
		writeGatewayError(w, http.StatusForbidden, "user agent is not permitted for this domain")
		return true
	}
	if len(p.BlockPaths) > 0 && matchesAny(p.BlockPaths, r.URL.Path) {
		writeGatewayError(w, http.StatusNotFound, "not found")
		return true
	}
	return false
}
