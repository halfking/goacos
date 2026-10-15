package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A dead upstream must not become a user-visible 502 while a healthy
// instance exists.
//
// This already had a test — TestDispatchFailsOverToHealthyInstance — and it
// was green. It was green for the wrong reason: `buildTable` defaults
// `lb` to round_robin, whose picker rotates, so the retry loop's second
// `Pick` lands on the other instance by accident.
//
// Every other algorithm can return the *same* instance twice in a row:
//
//   - least_conn keeps the first entry while inflight ties and weights tie
//     (lb.go: the `best` index is only replaced on a strictly better
//     candidate), and sequential requests always tie;
//   - ip_hash and cookie_sticky hash the client onto one fixed index.
//
// The retry loop asks `Pick` for another instance and then breaks out when
// `Pick` returns something already tried. So for those algorithms the very
// first failure ends the loop, and the client gets 502 while a healthy
// instance sits idle — even though the log line on that path claims
// "trying another instance".
//
// These cases pin the behaviour for every algorithm the gateway offers.

func failoverTable(t *testing.T, alg string, dead, good string) *Table {
	t.Helper()
	deadHost, deadPort := splitHostPort(t, dead)
	goodHost, goodPort := splitHostPort(t, good)
	// The dead instance goes first on purpose: least_conn and the sticky
	// algorithms are exactly the ones that prefer index 0 on a tie or a
	// fixed hash, so putting the dead one last would hide the bug.
	return buildTable(testRoute{
		domain: "acc.example.com", component: "api", service: "acc-api",
		lb: alg,
		instances: []Instance{
			instanceOf(deadHost, deadPort, 1, nil),
			instanceOf(goodHost, goodPort, 1, nil),
		},
	})
}

func TestFailsOverUnderEveryAlgorithm(t *testing.T) {
	for _, alg := range []string{"round_robin", "least_conn", "ip_hash", "cookie_sticky"} {
		t.Run(alg, func(t *testing.T) {
			dead := deadServer(t)
			var hits int32
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("served-by-healthy"))
			}))
			defer good.Close()

			tbl := failoverTable(t, alg, dead, good.URL)
			rec := httptest.NewRecorder()
			// The cookie makes cookie_sticky pin instead of degrading to
			// round robin, which is the branch that can return one index
			// forever.
			req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
			req.AddCookie(&http.Cookie{Name: "sid", Value: "sticky-1"})
			testGateway(tbl, nil).dispatch(rec, req, false)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s: failover failed: status=%d body=%s",
					alg, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if !strings.Contains(rec.Body.String(), "served-by-healthy") {
				t.Errorf("%s: body=%s", alg, rec.Body.String())
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("%s: healthy instance hit %d times, want exactly 1 "+
					"(the gateway must move on, not fan out)", alg, got)
			}
		})
	}
}

// A sticky client must not stay pinned to one dead index. The hash decides
// which index a given cookie maps to, so sweep enough cookies that both
// outcomes occur; every one of them has to be served by the survivor.
func TestFailsOverForEveryStickyClient(t *testing.T) {
	for _, alg := range []string{"ip_hash", "cookie_sticky"} {
		t.Run(alg, func(t *testing.T) {
			dead := deadServer(t)
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			}))
			defer good.Close()

			tbl := failoverTable(t, alg, dead, good.URL)
			for i := 0; i < 32; i++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
				req.RemoteAddr = fmt.Sprintf("10.0.0.%d:5000", i)
				req.AddCookie(&http.Cookie{Name: "sid", Value: fmt.Sprintf("sid-%d", i)})
				testGateway(tbl, nil).dispatch(rec, req, false)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s client %d: status=%d body=%s", alg, i,
						rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			}
		})
	}
}

// Fixing the retry must not turn "no healthy instance" into a silent hang or
// an empty upstream: every instance dead still has to be a 502.
func TestAllDeadStillIs502AfterRetryFix(t *testing.T) {
	first := deadServer(t)
	second := deadServer(t)
	host1, port1 := splitHostPort(t, first)
	host2, port2 := splitHostPort(t, second)
	for _, alg := range []string{"round_robin", "least_conn", "ip_hash", "cookie_sticky"} {
		tbl := buildTable(testRoute{
			domain: "acc.example.com", component: "api", service: "acc-api",
			lb: alg,
			instances: []Instance{
				instanceOf(host1, port1, 1, nil),
				instanceOf(host2, port2, 1, nil),
			},
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "sticky-1"})
		testGateway(tbl, nil).dispatch(rec, req, false)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status=%d, want 502", alg, rec.Code)
		}
	}
}

// With three instances and the first two dead, the request has to reach the
// third. A retry loop bounded to two tries would stop one short of it.
func TestFailsOverPastTwoDeadInstances(t *testing.T) {
	dead1 := deadServer(t)
	dead2 := deadServer(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("third"))
	}))
	defer good.Close()

	d1h, d1p := splitHostPort(t, dead1)
	d2h, d2p := splitHostPort(t, dead2)
	gh, gp := splitHostPort(t, good.URL)
	for _, alg := range []string{"round_robin", "least_conn", "ip_hash", "cookie_sticky"} {
		tbl := buildTable(testRoute{
			domain: "acc.example.com", component: "api", service: "acc-api",
			lb: alg,
			instances: []Instance{
				instanceOf(d1h, d1p, 1, nil),
				instanceOf(d2h, d2p, 1, nil),
				instanceOf(gh, gp, 1, nil),
			},
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://acc.example.com/", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: "sticky-1"})
		testGateway(tbl, nil).dispatch(rec, req, false)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status=%d body=%s", alg, rec.Code,
				strings.TrimSpace(rec.Body.String()))
		}
	}
}
