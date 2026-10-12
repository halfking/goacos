package gateway

// Instance selection.
//
// # Why weighted round robin and not random
//
// nginx's smooth weighted round robin is the algorithm most operators already
// have intuition for, and its two properties are the ones that matter here: a
// weight of 5 against 1 really does get five requests, and the distribution is
// even rather than clumped. Plain random selection produces visible bursts —
// five consecutive hits on one instance during a health blip is exactly the
// moment that instance has no capacity to absorb them.

import (
	"hash/fnv"
	"sync"
)

// lbState is the mutable part of selection for one target. It is keyed by the
// component identity, never by address: two components may share an instance,
// and their counters must not interfere.
type lbState struct {
	mu       sync.Mutex
	current  map[string]float64
	inflight map[string]int
	rr       uint64
}

func newLBState() *lbState {
	return &lbState{current: map[string]float64{}, inflight: map[string]int{}}
}

// Picker holds per-target selection state. It is safe for concurrent use and
// is shared by every connection the gateway handles.
type Picker struct {
	mu    sync.Mutex
	stats map[string]*lbState
}

func NewPicker() *Picker { return &Picker{stats: map[string]*lbState{}} }

func (p *Picker) state(key string) *lbState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.stats[key]
	if !ok {
		s = newLBState()
		p.stats[key] = s
	}
	return s
}

// Forget drops the state of a target that no longer resolves, so a long-running
// gateway does not accumulate one counter set per component ever declared.
func (p *Picker) Forget(live map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.stats {
		if !live[k] {
			delete(p.stats, k)
		}
	}
}

// Pick chooses one instance for a target.
//
// alg is the component's load-balancing hint: round_robin (default),
// least_conn, ip_hash, cookie_sticky. key is the sticky key — the client IP
// for ip_hash, the session cookie value for cookie_sticky — and is ignored by
// the others. An unknown algorithm falls back to round robin.
func (p *Picker) Pick(key, alg, stickyKey string, instances []Instance) *Instance {
	if len(instances) == 0 {
		return nil
	}
	if len(instances) == 1 {
		return &instances[0]
	}
	s := p.state(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	switch alg {
	case "ip_hash":
		// Hashing the client keeps one client on one instance for as long as
		// the instance set is unchanged. No fallback to round robin: a client
		// that silently migrates mid-session is exactly what this algorithm
		// exists to prevent.
		idx := hashIndex(stickyKey, len(instances))
		return &instances[idx]

	case "least_conn":
		best, bestConns := 0, -1
		for i, inst := range instances {
			conns := s.inflight[addr(inst)]
			if bestConns < 0 || conns < bestConns ||
				(conns == bestConns && weightOf(inst) > weightOf(instances[best])) {
				best, bestConns = i, conns
			}
		}
		return &instances[best]

	case "cookie_sticky":
		// Same pinning as ip_hash, but keyed by an explicit session value.
		// The proxy resolves the cookie before calling here, so a missing or
		// foreign cookie degrades to ordinary round robin rather than
		// sending every cookie-less request to the same instance.
		if stickyKey == "" {
			return p.pickRoundRobin(s, instances)
		}
		idx := hashIndex(stickyKey, len(instances))
		return &instances[idx]

	default:
		return p.pickRoundRobin(s, instances)
	}
}

// pickRoundRobin implements nginx's smooth weighted round robin: every round
// each instance gains its weight, the highest wins, and the winner immediately
// gives its weight back. The result is an even interleave instead of the
// clumps a naive "repeat the instance by its weight" list produces.
func (p *Picker) pickRoundRobin(s *lbState, instances []Instance) *Instance {
	total := 0.0
	best, bestCurrent := -1, -1.0
	for i, inst := range instances {
		w := weightOf(inst)
		total += w
		s.current[addr(inst)] += w
		if cur := s.current[addr(inst)]; cur > bestCurrent || best < 0 {
			best, bestCurrent = i, cur
		}
	}
	if best < 0 {
		return &instances[0]
	}
	s.current[addr(instances[best])] -= total
	return &instances[best]
}

// Acquire records that a connection to an instance has started, for least_conn.
func (p *Picker) Acquire(key, addr string) {
	s := p.state(key)
	s.mu.Lock()
	s.inflight[addr]++
	s.mu.Unlock()
}

// Release records that a connection to an instance has finished. A decrement
// that would go negative is clamped: an unbalanced release would make
// least_conn prefer an instance nobody is actually connected to.
func (p *Picker) Release(key, addr string) {
	s := p.state(key)
	s.mu.Lock()
	if s.inflight[addr] > 0 {
		s.inflight[addr]--
	}
	s.mu.Unlock()
}

// weightOf reads an instance's declared weight, treating a missing or
// non-positive weight as 1. A zero weight would remove an instance from
// selection entirely, which is almost never what a registry client means by
// registering with weight=0 — it means "no opinion".
func weightOf(i Instance) float64 {
	if i.Weight <= 0 {
		return 1
	}
	return i.Weight
}

// hashIndex maps an arbitrary string onto an index with FNV-1a. FNV rather
// than the built-in map hash because the input is a client IP or cookie, and a
// randomised seed would move clients between instances on every restart.
func hashIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}
