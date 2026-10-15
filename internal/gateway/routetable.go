package gateway

// The routing table: an in-memory snapshot of everything the data plane needs to
// turn a request into an upstream address, rebuilt from the database on a timer.
//
// # Why not query per request
//
// ResolveRoute is correct and it is the single source of truth, but it is a
// multi-query join. Running it on every request would put the database on the
// hot path of all inbound traffic, which is exactly how a service registry
// becomes the reason a site goes down. The table exists so that the cost of a
// request is a few map lookups and a read lock.
//
// # Why a timer and not change notification
//
// A timer makes staleness a bounded, obvious number: whatever the refresh
// interval is, no routing decision is ever older than that. Instance
// heartbeats already flow every 5s by default, so a refresh on the same order
// of magnitude sees a liveness change on schedule without the registry having
// to know that a gateway exists at all.
//
// The cost is that this file must NOT be the place policy is written. Policy
// lives in the database; this is a cache of it, rebuilt from scratch each time.
// If a value is ever only ever set here, it is unreachable by every other
// gateway instance and by the console, and that is the bug this comment exists
// to prevent.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// Target is one resolved destination: the component's policy plus the instances
// that can serve it.
type Target struct {
	Domain        string
	PathPrefix    string
	AppName       string
	ComponentName string
	Kind          string
	Protocol      string
	UpstreamName  string
	LBAlgorithm   string
	Sticky        string
	Weight        float64
	ServiceName   string
	GroupName     string
	ClusterName   string
	HealthPath    string
	// StripPrefix removes the matched route prefix before forwarding. It is
	// opt-in through component metadata because both conventions are common
	// and guessing wrong breaks the application, not just the gateway.
	StripPrefix bool
	// Metadata is the component's own metadata, carried through so delivery
	// overrides can be read on the request path without another lookup.
	Metadata  map[string]string
	Instances []storage.Instance
}

// Certificate source aliases. The gateway reasons about provenance in its own
// hot paths, so the storage vocabulary is imported once here rather than
// sprinkled through every comparison.
const (
	storageCertSourceManual     = storage.CertSourceManual
	storageCertSourceACME       = storage.CertSourceACME
	storageCertSourceSelfSigned = storage.CertSourceSelfSigned
	storageCertSourceFile       = storage.CertSourceFile
	storageCertStatusActive     = storage.CertStatusActive
)

// Instance is the naming registry's instance row. Aliased so the data plane
// reads as one vocabulary; the storage package keeps owning the schema.
type Instance = storage.Instance

// addr renders an instance as host:port, the only form the proxy needs.
func addr(i Instance) string { return i.IP + ":" + itoa(int(i.Port)) }

// Table is an immutable snapshot. Readers never hold the write lock, and every
// request reads exactly one of these, so a refresh can never be observed
// half-applied.
type Table struct {
	mu sync.RWMutex

	// domains maps a lowercased domain to its routes, longest prefix first.
	domains map[string][]routeEntry
	// services maps "group@@service" to the healthy instances behind it.
	services map[string][]storage.Instance
	// vhosts maps a lowercased domain to its traffic policy, when one exists.
	vhosts map[string]*storage.VHostPolicy

	// Domains is the sorted set of domains this table knows about. The
	// certificate manager uses it to decide which names to obtain
	// certificates for: only a name that is actually served is a name worth
	// spending a CA rate limit on.
	Domains []string

	LoadedAt time.Time
	Err      error
}

type routeEntry struct {
	storage.RouteBinding
	comp *storage.ApplicationComponent
}

// NewTable returns an empty table. An empty table is a valid, safe state: every
// lookup misses and the gateway answers 404, which is what a gateway with no
// declared routes should do.
func NewTable() *Table {
	return &Table{
		domains:  map[string][]routeEntry{},
		services: map[string][]storage.Instance{},
		vhosts:   map[string]*storage.VHostPolicy{},
	}
}

// serviceKey is the naming service identity. Group is part of the key because
// two services may legitimately share a name in different groups, and routing
// between them silently is the kind of bug that ships to production.
func serviceKey(group, service string) string {
	if group == "" {
		group = "DEFAULT_GROUP"
	}
	return group + "@@" + service
}

// Refresh rebuilds the snapshot from the database.
//
// A failure keeps the previous snapshot. An empty table would look identical to
// "every declared route was deleted", and a transient database blip must never
// be able to take a whole site offline — it can only delay seeing a change.
func (t *Table) Refresh(ctx context.Context, st *storage.Store, ns string) error {
	routes, err := st.ListRoutes(ctx, ns)
	if err != nil {
		return err
	}
	apps, err := st.ListApplications(ctx, ns)
	if err != nil {
		return err
	}
	vhosts, err := st.ListVHosts(ctx, ns)
	if err != nil {
		return err
	}

	comps := map[string]map[string]*storage.ApplicationComponent{}
	for _, a := range apps {
		list, err := st.ListComponents(ctx, ns, a.Name)
		if err != nil {
			return err
		}
		m := make(map[string]*storage.ApplicationComponent, len(list))
		for i := range list {
			c := list[i]
			m[c.Name] = &c
		}
		comps[a.Name] = m
	}

	domains := map[string][]routeEntry{}
	seenDomains := map[string]bool{}
	for _, r := range routes {
		d := storage.NormalizeDomain(r.Domain)
		byApp, ok := comps[r.AppName]
		if !ok {
			// A route for an application that no longer exists is a broken
			// declaration. It is dropped rather than served: a route that
			// resolves to nothing is worse than one that 404s, because it
			// looks configured.
			continue
		}
		comp, ok := byApp[r.ComponentName]
		if !ok {
			continue
		}
		domains[d] = append(domains[d], routeEntry{RouteBinding: r, comp: comp})
		seenDomains[d] = true
	}
	for d, list := range domains {
		sortRouteEntries(list)
		domains[d] = list
	}

	policy := map[string]*storage.VHostPolicy{}
	for i := range vhosts {
		v := vhosts[i]
		policy[storage.NormalizeDomain(v.Domain)] = &v
	}

	services := map[string][]storage.Instance{}
	for _, entries := range domains {
		for _, e := range entries {
			key := serviceKey(e.comp.GroupName, e.comp.ServiceName)
			if _, loaded := services[key]; loaded {
				continue
			}
			instances, err := st.ListInstances(ctx, storage.InstanceListQuery{
				NamespaceId: ns,
				GroupName:   e.comp.GroupName,
				ServiceName: e.comp.ServiceName,
				Clusters:    e.comp.ClusterName,
				HealthyOnly: true,
			})
			if err != nil {
				return err
			}
			services[key] = instances
		}
	}

	names := make([]string, 0, len(seenDomains))
	for d := range seenDomains {
		names = append(names, d)
	}
	sort.Strings(names)

	t.mu.Lock()
	t.domains = domains
	t.services = services
	t.vhosts = policy
	t.Domains = names
	t.LoadedAt = time.Now()
	t.Err = nil
	t.mu.Unlock()
	return nil
}

// Lookup resolves a host and path to a target, or nil when no route matches.
//
// metaFilters narrow the instance set, which is how a canary or a lane is
// selected per request without ever editing the instance table.
func (t *Table) Lookup(host, path string, metaFilters map[string]string) *Target {
	d := storage.NormalizeDomain(host)
	if path == "" {
		path = "/"
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, e := range t.domains[d] {
		if !pathPrefixMatches(e.PathPrefix, path) {
			continue
		}
		comp := e.comp
		group := comp.GroupName
		if group == "" {
			group = "DEFAULT_GROUP"
		}
		instances := t.services[serviceKey(group, comp.ServiceName)]
		instances = selectInstances(instances, comp.LBAlgorithm, comp.ClusterName)
		if len(metaFilters) > 0 {
			instances = filterInstancesByMeta(instances, metaFilters)
		}
		return &Target{
			Domain:        d,
			PathPrefix:    e.PathPrefix,
			AppName:       comp.AppName,
			ComponentName: comp.Name,
			Kind:          comp.Kind,
			Protocol:      comp.Protocol,
			UpstreamName:  e.UpstreamName,
			LBAlgorithm:   comp.LBAlgorithm,
			Sticky:        comp.Sticky,
			Weight:        comp.Weight,
			ServiceName:   comp.ServiceName,
			GroupName:     group,
			ClusterName:   comp.ClusterName,
			HealthPath:    comp.HealthPath,
			StripPrefix:   stripPrefixRequested(comp),
			Metadata:      comp.Metadata,
			Instances:     instances,
		}
	}
	return nil
}

// allTargets returns every distinct target declared for one domain, so callers
// that need the whole picture (transport pruning, console status) do not have
// to probe a single representative path.
// TargetKeys returns the selection-state key of every target this table can
// currently resolve — the exact strings Picker indexes its counters by.
//
// It exists so Forget is fed the keys it actually uses. Forget(live) deletes
// every counter set whose key is missing from live, so the caller's set has to
// be built from targetKey() output; handing it Domains (which are domain
// names) silently wipes every counter on every refresh, and least_conn then
// starts from zero on every routing tick and degenerates into "always pick the
// first instance".
//
// Deduped: several domains and several path prefixes can resolve to the same
// component, and they share one counter set by design.
func (t *Table) TargetKeys() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	seen := make(map[string]bool, len(t.domains))
	out := make([]string, 0, len(t.domains))
	for _, entries := range t.domains {
		for _, e := range entries {
			if e.comp == nil {
				continue
			}
			key := targetKey(&Target{
				AppName:       e.comp.AppName,
				ComponentName: e.comp.Name,
				ServiceName:   e.comp.ServiceName,
			})
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (t *Table) allTargets(domain string) []*Target {
	d := storage.NormalizeDomain(domain)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []*Target
	for _, e := range t.domains[d] {
		comp := e.comp
		group := comp.GroupName
		if group == "" {
			group = "DEFAULT_GROUP"
		}
		out = append(out, &Target{
			Domain:        d,
			PathPrefix:    e.PathPrefix,
			AppName:       comp.AppName,
			ComponentName: comp.Name,
			Kind:          comp.Kind,
			Protocol:      comp.Protocol,
			UpstreamName:  e.UpstreamName,
			LBAlgorithm:   comp.LBAlgorithm,
			Sticky:        comp.Sticky,
			ServiceName:   comp.ServiceName,
			GroupName:     group,
			ClusterName:   comp.ClusterName,
			HealthPath:    comp.HealthPath,
			StripPrefix:   stripPrefixRequested(comp),
			Metadata:      comp.Metadata,
			Instances:     selectInstances(t.services[serviceKey(group, comp.ServiceName)], comp.LBAlgorithm, comp.ClusterName),
		})
	}
	return out
}

// stripPrefixRequested reads the component's opt-in metadata.
//
// The value is parsed strictly: only "true"/"1"/"yes" enable it. A typo must
// not silently strip a prefix that the upstream expects to receive.
func stripPrefixRequested(comp *storage.ApplicationComponent) bool {
	switch strings.ToLower(strings.TrimSpace(comp.Metadata["stripPrefix"])) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Policy returns the traffic policy for a domain, or nil when it has none.
// A nil policy means "use the gateway defaults", not "no restrictions".
// sortRouteEntries 定序：最长前缀优先，其次显式 priority，最后组件名。
// 全序，保证每次刷新、每个网关实例解析出的落点都一样。
//
// 为什么单独抽出来：这份规则原先内联在 Refresh 里，而测试那边的 buildTable
// 自己又抄了一份 sortRoutes。两份抄写可以各改各的——把生产里的比较符翻成
// 「最短前缀优先」，测试依然全绿，因为它测的是自己那份。
// 现在测试直接调这个函数，规则只有一处。
func sortRouteEntries(list []routeEntry) {
	sort.SliceStable(list, func(i, j int) bool {
		li, lj := strings.TrimSuffix(list[i].PathPrefix, "/"), strings.TrimSuffix(list[j].PathPrefix, "/")
		if len(li) != len(lj) {
			return len(li) > len(lj)
		}
		if list[i].Priority != list[j].Priority {
			return list[i].Priority < list[j].Priority
		}
		return list[i].ComponentName < list[j].ComponentName
	})
}

func (t *Table) Policy(host string) *storage.VHostPolicy {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.vhosts[storage.NormalizeDomain(host)]
}

// KnownDomain reports whether this gateway serves the domain at all.
func (t *Table) KnownDomain(host string) bool {
	d := storage.NormalizeDomain(host)
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.domains[d]
	return ok
}

// Snapshot returns the loaded timestamp and last refresh error, for status.
func (t *Table) Snapshot() (time.Time, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.LoadedAt, t.Err
}

// selectInstances applies the component's load-balancing ordering and cluster
// narrowing. Ordering is done here rather than at pick time so that the LB
// algorithms only have to choose an index.
func selectInstances(instances []storage.Instance, lb, cluster string) []storage.Instance {
	out := make([]storage.Instance, 0, len(instances))
	for _, inst := range instances {
		if cluster != "" && inst.ClusterName != "" && inst.ClusterName != cluster {
			continue
		}
		if !inst.Enabled {
			continue
		}
		out = append(out, inst)
	}
	switch lb {
	case "least_conn", "cookie_sticky", "ip_hash", "round_robin", "":
	default:
		// An unrecognised algorithm falls back to the stable ordering rather
		// than to an error: an operator typo must not take a site down.
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		if out[i].IP != out[j].IP {
			return out[i].IP < out[j].IP
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// filterInstancesByMeta keeps instances whose metadata contains every requested
// pair. A missing key does not match, which keeps an unlabelled instance out of
// a canary set rather than into it.
func filterInstancesByMeta(instances []storage.Instance, filters map[string]string) []storage.Instance {
	out := make([]storage.Instance, 0, len(instances))
	for _, inst := range instances {
		meta := inst.Metadata()
		ok := true
		for k, v := range filters {
			if meta[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, inst)
		}
	}
	return out
}

// pathPrefixMatches treats "/" as a catch-all and otherwise requires a segment
// boundary, so "/api" must not match "/apixyz".
func pathPrefixMatches(prefix, path string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
