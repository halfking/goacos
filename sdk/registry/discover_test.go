package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCandidatesOrderAndWellKnownNames(t *testing.T) {
	got := Candidates(Config{})
	if len(got) != 4 {
		t.Fatalf("len(Candidates(Config{})) = %d, want 4: %+v", len(got), got)
	}

	wantAddrs := []string{"goacos:8848", "nacos:8848", "host.docker.internal:8848", "127.0.0.1:8848"}
	for i, want := range wantAddrs {
		if got[i].Addr != want {
			t.Errorf("candidate %d addr = %q, want %q", i, got[i].Addr, want)
		}
	}

	wantSources := []string{
		SourceWellKnown + "goacos",
		SourceWellKnown + "nacos",
		SourceWellKnown + "host.docker.internal",
		SourceLoopback,
	}
	for i, want := range wantSources {
		if got[i].Source != want {
			t.Errorf("candidate %d source = %q, want %q", i, got[i].Source, want)
		}
	}
}

func TestCandidatesPutsExplicitAddressFirst(t *testing.T) {
	got := Candidates(Config{ServerAddr: "http://goacos:9999"})
	if len(got) == 0 {
		t.Fatal("Candidates returned nothing")
	}
	if got[0].Addr != "http://goacos:9999" || got[0].Source != SourceConfig {
		t.Errorf("candidate 0 = %+v, want the explicit address tagged %q", got[0], SourceConfig)
	}
}

func TestCandidatesDeduplicates(t *testing.T) {
	// The same registry spelled three ways must collapse to one entry, or a
	// compose file that mixes spellings would burn extra probe slots.
	got := Candidates(Config{ServerAddr: "http://goacos:8848"})

	seen := map[string]int{}
	for i, c := range got {
		key := candidateKey(c.Addr)
		if prev, dup := seen[key]; dup {
			t.Errorf("candidate %d (%s) duplicates candidate %d", i, c.Addr, prev)
		}
		seen[key] = i
	}

	if len(got) != 4 {
		t.Errorf("len = %d, want 4 after de-duplication: %+v", len(got), got)
	}
	if got[0].Source != SourceConfig {
		t.Errorf("candidate 0 source = %q, want %q (the explicit spelling wins)", got[0].Source, SourceConfig)
	}
}

func TestCandidateKeyNormalization(t *testing.T) {
	same := [][2]string{
		{"goacos:8848", "http://goacos:8848"},
		{"http://goacos:8848/", "http://goacos:8848"},
		{"GOACOS:8848", "http://goacos:8848"},
		{"  http://goacos:8848 ", "goacos:8848"},
	}
	for _, pair := range same {
		if candidateKey(pair[0]) != candidateKey(pair[1]) {
			t.Errorf("candidateKey(%q) != candidateKey(%q), want equal", pair[0], pair[1])
		}
	}
	if candidateKey("goacos:8848") == candidateKey("nacos:8848") {
		t.Error("different hosts must not collapse to the same key")
	}
}

func TestDiscoverReturnsFirstSuccess(t *testing.T) {
	var probed []string
	probe := func(ctx context.Context, addr string) error {
		probed = append(probed, addr)
		if addr != "second:8848" {
			return errors.New("connection refused")
		}
		return nil
	}

	cands := []Candidate{
		{Addr: "first:8848", Source: SourceConfig},
		{Addr: "second:8848", Source: SourceLoopback},
		{Addr: "third:8848", Source: SourceLoopback},
	}
	got, err := discoverWith(context.Background(), cands, 2*time.Second, probe)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != "second:8848" {
		t.Errorf("addr = %q, want second:8848", got)
	}
	if len(probed) != 2 {
		t.Errorf("probed %v, want discovery to stop at the first success", probed)
	}
}

func TestDiscoverSkipsDeadThenSucceeds(t *testing.T) {
	// The classic case: a candidate that hangs (an unresolvable DNS name in
	// production) must not consume the whole budget. The probe here blocks
	// until its per-candidate context expires, so if discovery did not apply a
	// per-candidate timeout the second candidate would never be reached.
	probe := func(ctx context.Context, addr string) error {
		if addr == "dead:8848" {
			<-ctx.Done()
			return fmt.Errorf("dial %s: %w", addr, ctx.Err())
		}
		return nil
	}

	cands := []Candidate{
		{Addr: "dead:8848", Source: SourceWellKnown + "goacos"},
		{Addr: "alive:8848", Source: SourceWellKnown + "nacos"},
	}

	start := time.Now()
	got, err := discoverWith(context.Background(), cands, 10*time.Second, probe)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != "alive:8848" {
		t.Errorf("addr = %q, want alive:8848", got)
	}
	if elapsed > 4*time.Second {
		t.Errorf("discovery took %s; a dead candidate must be bounded per-candidate, not by the whole budget", elapsed)
	}
}

func TestDiscoverAllCandidatesFail(t *testing.T) {
	probe := func(ctx context.Context, addr string) error {
		return fmt.Errorf("dial tcp: lookup %s: no such host", hostOf(addr))
	}

	cands := []Candidate{
		{Addr: "goacos:8848", Source: SourceWellKnown + "goacos"},
		{Addr: "nacos:8848", Source: SourceWellKnown + "nacos"},
		{Addr: "127.0.0.1:8848", Source: SourceLoopback},
	}
	_, err := discoverWith(context.Background(), cands, 2*time.Second, probe)
	if err == nil {
		t.Fatal("want an error when nothing answers")
	}

	msg := err.Error()
	// Every candidate, its source and the reason must be in the message:
	// this string is what an operator reads.
	for _, want := range []string{
		"goacos:8848", SourceWellKnown + "goacos",
		"nacos:8848", SourceWellKnown + "nacos",
		"127.0.0.1:8848", SourceLoopback,
		"no such host",
		"probed 3 candidate(s)",
		"Config.ServerAddr",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("discovery error is missing %q:\n%s", want, msg)
		}
	}
}

func TestDiscoverRespectsOverallDeadline(t *testing.T) {
	// Every candidate hangs; discovery must give up on the overall budget.
	probe := func(ctx context.Context, addr string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cands := make([]Candidate, 10)
	for i := range cands {
		cands[i] = Candidate{Addr: fmt.Sprintf("host%d:8848", i), Source: SourceLoopback}
	}

	start := time.Now()
	_, err := discoverWith(context.Background(), cands, 300*time.Millisecond, probe)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want an error when the overall deadline expires")
	}
	if elapsed > 2*time.Second {
		t.Errorf("discovery ran for %s, want it bounded by the 300ms overall deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error should mention the deadline:\n%s", err)
	}
}

func TestDiscoverNoCandidates(t *testing.T) {
	if _, err := Discover(context.Background(), nil, time.Second); err == nil {
		t.Fatal("want an error for an empty candidate list")
	}
}

func TestDiscoverNilContext(t *testing.T) {
	//lint:ignore SA1012 deliberately passing a nil context to assert the guard.
	if _, err := discoverWith(nil, []Candidate{{Addr: "x:8848"}}, time.Second, nil); err == nil {
		t.Fatal("want an error for a nil context")
	}
}

// TestDiscoverAgainstRealServer exercises the real probe end to end: a live
// httptest registry on loopback must be found, and a closed port must be
// rejected quickly rather than hanging.
func TestDiscoverAgainstRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathPrefix+pathHealth {
			t.Errorf("probe hit %s, want %s", r.URL.Path, pathPrefix+pathHealth)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"UP","components":{"nacos":{"status":"UP"}}}`))
	}))
	defer srv.Close()

	// A closed port on loopback: connection refused, immediately.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := dead.URL
	dead.Close()

	cands := []Candidate{
		{Addr: deadAddr, Source: SourceConfig},
		{Addr: srv.URL, Source: SourceLoopback},
	}

	start := time.Now()
	got, err := Discover(context.Background(), cands, 5*time.Second)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != srv.URL {
		t.Errorf("addr = %q, want %q", got, srv.URL)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("discovery took %s; a refused connection must fail fast", elapsed)
	}
}

func TestDefaultProbeRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry still starting", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := defaultProbe(context.Background(), srv.URL); err == nil {
		t.Fatal("want an error for a non-200 health response")
	} else if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should carry the status, got: %v", err)
	}
}

func TestDefaultProbeRejectsNonUPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"DOWN"}`))
	}))
	defer srv.Close()

	err := defaultProbe(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "DOWN") {
		t.Fatalf("want a DOWN health error, got: %v", err)
	}
}

func TestDefaultProbeFailsFastOnEmptyAddress(t *testing.T) {
	if err := defaultProbe(context.Background(), "  "); err == nil {
		t.Fatal("want an error for an empty address")
	}
}

func TestDefaultProbeTimesOutOnBlackHole(t *testing.T) {
	// A listener that accepts but never answers must not hold the probe past
	// its own timeout. Bound by the overall context to keep the test honest.
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	// One defer, close-then-wait: the accept loop only returns once the
	// listener is closed, so the order inside matters.
	defer func() {
		_ = ln.Close()
		wg.Wait()
	}()
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and stay silent.
			go func() {
				<-time.After(10 * time.Second)
				_ = conn.Close()
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = defaultProbe(ctx, "http://"+ln.Addr().String())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want a timeout error from a silent listener")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("probe ran for %s; it must respect its own timeout", elapsed)
	}
}

// hostOf strips a port from an address for error-message realism.
func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
