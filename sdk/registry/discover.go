package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Source labels describe where a Candidate came from. They are purely
// descriptive — the ones a caller's config layer uses get relabelled by that
// caller, and SourceEnv exists so a config file read can claim it without this
// package ever touching os.Getenv itself.
const (
	// SourceConfig marks an address supplied explicitly in Config.
	SourceConfig = "config"
	// SourceEnv marks an address that the caller's config layer read from the
	// environment and passed in.
	SourceEnv = "env"
	// SourceDefault marks the package's built-in fallback address.
	SourceDefault = "default"
	// SourceLoopback marks 127.0.0.1 on the registry's default port.
	SourceLoopback = "loopback"
	// SourceWellKnown is a prefix; the DNS name is appended, e.g.
	// "wellknown:goacos".
	SourceWellKnown = "wellknown:"
)

// DefaultDiscoverTimeout is the overall budget for one Discover call when the
// caller does not supply one.
const DefaultDiscoverTimeout = 10 * time.Second

const (
	// perCandidateTimeout bounds a single probe. It is deliberately much
	// smaller than the overall budget: three dead DNS names must not eat the
	// whole window while a live fourth candidate waits its turn.
	perCandidateTimeout = 2 * time.Second

	// dialTimeout covers name resolution AND the TCP handshake. This is the
	// single most important number in this file: net.Dialer.Timeout includes
	// DNS, so an unresolvable name fails fast instead of stalling discovery
	// until the caller's context dies.
	dialTimeout = 800 * time.Millisecond

	// maxProbeBodyBytes is all we need from a health endpoint.
	maxProbeBodyBytes = 4096
)

// wellKnownNames are the registry DNS names this workspace actually uses on
// its shared docker network, in preference order. "goacos" is our own server,
// "nacos" is what a stock compose file calls it, and host.docker.internal
// reaches a registry running on the host from inside a container. The order
// encodes preference, not correctness: any 200 wins.
var wellKnownNames = []string{"goacos", "nacos", "host.docker.internal"}

// Candidate is one address to try, plus where it came from. Source is what
// makes the 3am error message readable.
type Candidate struct {
	// Addr is the registry base address, e.g. "goacos:8848" or
	// "http://goacos:8848". Both forms work.
	Addr string

	// Source is a human-readable origin label, e.g. "config" or
	// "wellknown:goacos".
	Source string
}

// probeFunc checks one candidate. It is a function type so tests can simulate
// "first candidate times out, second succeeds" without real sockets.
type probeFunc func(ctx context.Context, addr string) error

// Candidates returns the ordered, de-duplicated list of addresses to probe.
//
// Order is preference order:
//
//  1. Config.ServerAddr, when set. An operator who told us where the registry
//     is gets no second-guessing.
//  2. The well-known docker-network names, in order.
//  3. Loopback on the default port.
//
// De-duplication is by normalized address, so "goacos:8848" and
// "http://goacos:8848/" collapse to one entry — otherwise a compose file that
// spells the registry both ways would burn two probe slots on the same server.
func Candidates(cfg Config) []Candidate {
	var out []Candidate
	seen := make(map[string]bool, 4)

	add := func(addr, source string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		key := candidateKey(addr)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Candidate{Addr: addr, Source: source})
	}

	if cfg.ServerAddr != "" {
		add(cfg.ServerAddr, SourceConfig)
	}
	for _, name := range wellKnownNames {
		add(fmt.Sprintf("%s:%d", name, DefaultPort), SourceWellKnown+name)
	}
	add(fmt.Sprintf("127.0.0.1:%d", DefaultPort), SourceLoopback)

	return out
}

// candidateKey normalizes an address for de-duplication: lowercase, no scheme,
// no trailing slash.
func candidateKey(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	addr = strings.TrimSuffix(addr, "/")
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	return addr
}

// Discover probes the candidates in order and returns the first address whose
// health endpoint answers 200.
//
// The classic bug this exists to avoid: a candidate whose DNS name does not
// resolve must fail fast, because a client that waits out the full timeout per
// candidate turns a three-line config into a thirty-second boot. Hence the
// per-candidate timeout AND a dial timeout that covers name resolution.
//
// When nothing answers, the returned error lists every candidate with its
// source and the reason it failed, because that message is the only thing the
// operator will see.
func Discover(ctx context.Context, candidates []Candidate, timeout time.Duration) (string, error) {
	return discoverWith(ctx, candidates, timeout, defaultProbe)
}

// probeFailure records why one candidate was rejected.
type probeFailure struct {
	addr    string
	source  string
	elapsed time.Duration
	err     error
}

// discoverWith is Discover with an injectable probe.
func discoverWith(ctx context.Context, candidates []Candidate, timeout time.Duration, probe probeFunc) (string, error) {
	if probe == nil {
		probe = defaultProbe
	}
	if len(candidates) == 0 {
		return "", errors.New("nacos: no discovery candidates to probe")
	}
	if ctx == nil {
		return "", errors.New("nacos: nil context")
	}
	if timeout <= 0 {
		timeout = DefaultDiscoverTimeout
	}

	// Overall deadline on top of the per-candidate ones, so a pathological
	// probe implementation cannot outlive the caller's budget either.
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var failures []probeFailure
	deadline := time.Now().Add(timeout)

	for _, candidate := range candidates {
		if err := deadlineCtx.Err(); err != nil {
			failures = append(failures, probeFailure{
				addr:    "(not probed)",
				source:  candidate.Source,
				elapsed: 0,
				err:     fmt.Errorf("overall discovery deadline of %s reached: %w", timeout, err),
			})
			break
		}

		// Never let one candidate outlive the overall budget.
		budget := perCandidateTimeout
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
		if budget <= 0 {
			break
		}

		attemptCtx, cancelAttempt := context.WithTimeout(deadlineCtx, budget)
		started := time.Now()
		err := probe(attemptCtx, candidate.Addr)
		cancelAttempt()
		elapsed := time.Since(started).Round(time.Millisecond)

		if err == nil {
			return candidate.Addr, nil
		}
		failures = append(failures, probeFailure{
			addr:    candidate.Addr,
			source:  candidate.Source,
			elapsed: elapsed,
			err:     err,
		})
	}

	return "", discoveryError(candidates, timeout, failures)
}

// discoveryError renders every candidate and why it failed. The format is
// intentionally multi-line: it is meant to be pasted into a ticket.
func discoveryError(candidates []Candidate, timeout time.Duration, failures []probeFailure) error {
	var b strings.Builder
	fmt.Fprintf(&b, "nacos: no registry answered the health probe within %s\n", timeout)
	fmt.Fprintf(&b, "nacos: probed %d candidate(s):\n", len(candidates))
	for _, f := range failures {
		if f.elapsed > 0 {
			fmt.Fprintf(&b, "  - %s [%s] failed after %s: %v\n", f.addr, f.source, f.elapsed, f.err)
		} else {
			fmt.Fprintf(&b, "  - %s [%s] failed: %v\n", f.addr, f.source, f.err)
		}
	}
	b.WriteString("nacos: check that the registry is running and reachable from this host; " +
		"a 'no such host' failure means the docker-network name did not resolve, " +
		"and a 'connection refused' failure means nothing is listening on that port. " +
		"Set Config.ServerAddr to skip discovery entirely.")
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// defaultProbe is the real health probe used by Discover.
//
// Every layer has its own short timeout: DialContext (including DNS),
// TLS handshake, response headers, and the client as a whole. DisableKeepAlives
// is set because discovery probes run a handful of times per process; reusing
// a connection to a registry that may have died is not worth the idle socket.
func defaultProbe(ctx context.Context, addr string) error {
	base := normalizeBaseURL(addr)
	if base == "" {
		return errors.New("empty address")
	}
	url := base + pathPrefix + pathHealth

	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: dialTimeout,
		DisableKeepAlives:     true,
		MaxIdleConns:          1,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   perCandidateTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect off the registry is a misconfiguration, not a
			// discovery result. Stop rather than follow it somewhere random.
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBodyBytes))
		_ = resp.Body.Close()
	}()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes))
	if readErr != nil {
		return fmt.Errorf("read response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, summarizeBody(body))
	}
	// A 200 with an explicit non-UP status is still a failure.
	var health healthData
	if json.Unmarshal(body, &health) == nil && health.Status != "" &&
		!strings.EqualFold(health.Status, "UP") {
		return fmt.Errorf("health status %q", health.Status)
	}
	return nil
}
