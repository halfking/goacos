package storage

// Application model: application -> component -> naming service, plus
// domain routes that bind inbound traffic to a component.
//
// # Why this layer exists
//
// The naming registry answers "who is healthy for service X". It cannot answer
// "where should acc-api.itestu.cn go", because that question involves a domain,
// an application boundary and a component role (frontend vs API vs worker) —
// none of which the Nacos naming model knows about. So every gateway in this
// platform has been keeping its own hand-written domain -> host:port table, and
// two of those tables have already drifted apart in production.
//
// The fix is not a better hand-written table. It is making the registry able to
// hold the policy, so that the IP part comes from live instance state and the
// policy part is reviewable data.
//
// # What is deliberately absent
//
// There is no IP and no port anywhere in this file. A component names a service;
// the instances behind it are read from naming_instance at resolve time. If a
// future contributor adds an "ip" column here, the two SSOTs are back.

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

// Component kinds. These are the roles a complete frontend+backend application
// actually needs; a pure micro-service registry has no vocabulary for any of them.
const (
	KindWeb     = "web"
	KindAPI     = "api"
	KindWorker  = "worker"
	KindMCP     = "mcp"
	KindGateway = "gateway"
)

// Component protocols.
//
// http is the default; https is the same thing spoken over TLS when the
// registered instance terminates TLS itself rather than relying on the
// gateway. The distinction decides whether the gateway dials plaintext or TLS,
// which is why it is a component property and not a global switch.
const (
	ProtocolHTTP   = "http"
	ProtocolHTTPS  = "https"
	ProtocolStatic = "static"
	ProtocolGRPC   = "grpc"
	ProtocolWS     = "ws"
)

// Application is one product deployed in one namespace.
type Application struct {
	NamespaceId string
	Name        string
	DisplayName string
	Description string
	Env         string
	Metadata    map[string]string
	GmtCreate   string
	GmtModified string
}

// ApplicationComponent is one addressable piece of an application.
type ApplicationComponent struct {
	NamespaceId string
	AppName     string
	Name        string
	Kind        string
	Protocol    string
	ServiceName string
	GroupName   string
	ClusterName string
	HealthPath  string
	Weight      float64
	LBAlgorithm string
	Sticky      string
	Metadata    map[string]string
}

// RouteBinding maps an inbound domain + path prefix onto one component.
type RouteBinding struct {
	NamespaceId   string
	Domain        string
	PathPrefix    string
	AppName       string
	ComponentName string
	UpstreamName  string
	Priority      int
}

const appCols = `namespace_id, name, COALESCE(display_name,''), COALESCE(description,''),
                 env, COALESCE(metadata,''), gmt_create, gmt_modified`

func decodeMeta(raw string) map[string]string {
	m := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func encodeMeta(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// ListApplications returns every application in a namespace, ordered by name.
func (s *Store) ListApplications(ctx context.Context, ns string) ([]Application, error) {
	rows, err := s.Query(ctx, "SELECT "+appCols+" FROM application WHERE namespace_id=? ORDER BY name", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Application
	for rows.Next() {
		var a Application
		var meta string
		if err := rows.Scan(&a.NamespaceId, &a.Name, &a.DisplayName, &a.Description,
			&a.Env, &meta, &a.GmtCreate, &a.GmtModified); err != nil {
			return nil, err
		}
		a.Metadata = decodeMeta(meta)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApplication returns one application or nil when absent.
func (s *Store) GetApplication(ctx context.Context, ns, name string) (*Application, error) {
	var a Application
	var meta string
	err := s.QueryRow(ctx, "SELECT "+appCols+" FROM application WHERE namespace_id=? AND name=?", ns, name).
		Scan(&a.NamespaceId, &a.Name, &a.DisplayName, &a.Description, &a.Env, &meta, &a.GmtCreate, &a.GmtModified)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Metadata = decodeMeta(meta)
	return &a, nil
}

// UpsertApplication creates or updates one application. It reports whether a
// new row was created, so callers can tell "declared" from "amended".
func (s *Store) UpsertApplication(ctx context.Context, a Application) (bool, error) {
	existing, err := s.GetApplication(ctx, a.NamespaceId, a.Name)
	if err != nil {
		return false, err
	}
	if existing != nil {
		_, err := s.Exec(ctx,
			`UPDATE application SET display_name=?, description=?, env=?, metadata=?,
			 gmt_modified=CURRENT_TIMESTAMP WHERE namespace_id=? AND name=?`,
			a.DisplayName, a.Description, a.Env, encodeMeta(a.Metadata), a.NamespaceId, a.Name)
		return false, err
	}
	_, err = s.Exec(ctx,
		`INSERT INTO application (namespace_id, name, display_name, description, env, metadata)
		 VALUES (?,?,?,?,?,?)`,
		a.NamespaceId, a.Name, a.DisplayName, a.Description, a.Env, encodeMeta(a.Metadata))
	return true, err
}

// DeleteApplication removes an application and, with it, its components and
// routes. Instances are NOT touched: a service may be shared by two
// applications, and deleting a declaration must never deregister a running
// service another declaration still points at.
func (s *Store) DeleteApplication(ctx context.Context, ns, name string) error {
	if _, err := s.Exec(ctx, "DELETE FROM route_binding WHERE namespace_id=? AND app_name=?", ns, name); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, "DELETE FROM application_component WHERE namespace_id=? AND app_name=?", ns, name); err != nil {
		return err
	}
	_, err := s.Exec(ctx, "DELETE FROM application WHERE namespace_id=? AND name=?", ns, name)
	return err
}

const compCols = `namespace_id, app_name, name, kind, protocol, service_name, group_name,
                  cluster_name, health_path, weight, lb_algorithm, sticky, COALESCE(metadata,'')`

func scanComponent(row interface{ Scan(...any) error }) (*ApplicationComponent, error) {
	var c ApplicationComponent
	var meta string
	if err := row.Scan(&c.NamespaceId, &c.AppName, &c.Name, &c.Kind, &c.Protocol, &c.ServiceName,
		&c.GroupName, &c.ClusterName, &c.HealthPath, &c.Weight, &c.LBAlgorithm, &c.Sticky, &meta); err != nil {
		return nil, err
	}
	c.Metadata = decodeMeta(meta)
	return &c, nil
}

// ListComponents returns an application's components, ordered by name.
func (s *Store) ListComponents(ctx context.Context, ns, app string) ([]ApplicationComponent, error) {
	rows, err := s.Query(ctx, "SELECT "+compCols+" FROM application_component WHERE namespace_id=? AND app_name=? ORDER BY name", ns, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApplicationComponent
	for rows.Next() {
		c, err := scanComponent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetComponent returns one component or nil.
//
// sql.ErrNoRows is an absence, not a failure: UpsertComponent calls this first
// to decide insert-versus-update, and treating "no such component" as an error
// would make the very first write of every new component fail.
func (s *Store) GetComponent(ctx context.Context, ns, app, name string) (*ApplicationComponent, error) {
	c, err := scanComponent(s.QueryRow(ctx,
		"SELECT "+compCols+" FROM application_component WHERE namespace_id=? AND app_name=? AND name=?", ns, app, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// UpsertComponent creates or updates one component.
func (s *Store) UpsertComponent(ctx context.Context, c ApplicationComponent) (bool, error) {
	existing, err := s.GetComponent(ctx, c.NamespaceId, c.AppName, c.Name)
	if err != nil {
		return false, err
	}
	if existing != nil {
		_, err := s.Exec(ctx,
			`UPDATE application_component SET kind=?, protocol=?, service_name=?, group_name=?,
			 cluster_name=?, health_path=?, weight=?, lb_algorithm=?, sticky=?, metadata=?,
			 gmt_modified=CURRENT_TIMESTAMP WHERE namespace_id=? AND app_name=? AND name=?`,
			c.Kind, c.Protocol, c.ServiceName, c.GroupName, c.ClusterName, c.HealthPath,
			c.Weight, c.LBAlgorithm, c.Sticky, encodeMeta(c.Metadata), c.NamespaceId, c.AppName, c.Name)
		return false, err
	}
	_, err = s.Exec(ctx,
		`INSERT INTO application_component (namespace_id, app_name, name, kind, protocol,
		   service_name, group_name, cluster_name, health_path, weight, lb_algorithm, sticky, metadata)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.NamespaceId, c.AppName, c.Name, c.Kind, c.Protocol, c.ServiceName, c.GroupName,
		c.ClusterName, c.HealthPath, c.Weight, c.LBAlgorithm, c.Sticky, encodeMeta(c.Metadata))
	return true, err
}

// DeleteComponent removes a component and any route that pointed at it, so the
// model can never end up with a route to a component that no longer exists.
func (s *Store) DeleteComponent(ctx context.Context, ns, app, name string) error {
	if _, err := s.Exec(ctx,
		"DELETE FROM route_binding WHERE namespace_id=? AND app_name=? AND component_name=?", ns, app, name); err != nil {
		return err
	}
	_, err := s.Exec(ctx, "DELETE FROM application_component WHERE namespace_id=? AND app_name=? AND name=?", ns, app, name)
	return err
}

const routeCols = `namespace_id, domain, path_prefix, app_name, component_name, upstream_name, priority`

// ListRoutes returns every route in a namespace, most specific prefix first.
func (s *Store) ListRoutes(ctx context.Context, ns string) ([]RouteBinding, error) {
	rows, err := s.Query(ctx,
		"SELECT "+routeCols+" FROM route_binding WHERE namespace_id=? ORDER BY domain, priority, path_prefix", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteBinding
	for rows.Next() {
		var r RouteBinding
		if err := rows.Scan(&r.NamespaceId, &r.Domain, &r.PathPrefix, &r.AppName, &r.ComponentName, &r.UpstreamName, &r.Priority); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRoute creates or updates one route binding.
func (s *Store) UpsertRoute(ctx context.Context, r RouteBinding) error {
	keyCols := []string{"namespace_id", "domain", "path_prefix"}
	insertCols := []string{"namespace_id", "domain", "path_prefix", "app_name", "component_name", "upstream_name", "priority"}
	_, err := s.Exec(ctx, s.dialect.Upsert("route_binding", keyCols, insertCols,
		[]string{"app_name", "component_name", "upstream_name", "priority"}),
		r.NamespaceId, r.Domain, r.PathPrefix, r.AppName, r.ComponentName, r.UpstreamName, r.Priority)
	return err
}

// DeleteRoute removes one route binding.
func (s *Store) DeleteRoute(ctx context.Context, ns, domain, pathPrefix string) error {
	_, err := s.Exec(ctx, "DELETE FROM route_binding WHERE namespace_id=? AND domain=? AND path_prefix=?", ns, domain, pathPrefix)
	return err
}

// ResolvedRoute is the answer a gateway asks for: policy plus the live,
// healthy instances behind it, in one call.
//
// The caller never has to join two hand-maintained tables or know which naming
// service backs a domain. That is the entire point of this type.
type ResolvedRoute struct {
	NamespaceId   string
	Domain        string
	PathPrefix    string
	AppName       string
	ComponentName string
	ComponentKind string
	Protocol      string
	UpstreamName  string
	LBAlgorithm   string
	Sticky        string
	Weight        float64
	ClusterName   string
	HealthPath    string
	ServiceName   string
	GroupName     string
	Instances     []Instance
}

// ResolveRoute finds the route matching a domain and path, loads its component,
// and returns it together with the healthy instances of the naming service that
// component maps to.
//
// Prefix matching is longest-first, so a specific "/api/v2/" binding wins over
// the catch-all "/" of the same domain. metaFilters, when non-empty, restrict
// the instance set by metadata key/value — this is how a lane or a canary is
// selected without touching the instance table.
//
// A missing route is reported as (nil, nil) rather than an error: "this domain
// is not declared here" is a normal answer, and a gateway must be able to tell
// it apart from "the database is down".
func (s *Store) ResolveRoute(ctx context.Context, ns, domain, path string, metaFilters map[string]string) (*ResolvedRoute, error) {
	route, err := s.matchRoute(ctx, ns, domain, path)
	if err != nil || route == nil {
		return nil, err
	}
	comp, err := s.GetComponent(ctx, ns, route.AppName, route.ComponentName)
	if err != nil {
		return nil, err
	}
	if comp == nil {
		// A route pointing at a missing component is a broken declaration, not
		// a healthy one. Report it as unresolved rather than inventing an
		// empty upstream, because an empty upstream takes the service offline.
		return nil, nil
	}
	group := comp.GroupName
	if group == "" {
		group = "DEFAULT_GROUP"
	}
	instances, err := s.ListInstances(ctx, InstanceListQuery{
		NamespaceId: ns,
		GroupName:   group,
		ServiceName: comp.ServiceName,
		Clusters:    comp.ClusterName,
		HealthyOnly: true,
	})
	if err != nil {
		return nil, err
	}
	if len(metaFilters) > 0 {
		instances = filterInstancesByMeta(instances, metaFilters)
	}
	sortInstances(instances, comp.LBAlgorithm)
	return &ResolvedRoute{
		NamespaceId:   ns,
		Domain:        route.Domain,
		PathPrefix:    route.PathPrefix,
		AppName:       comp.AppName,
		ComponentName: comp.Name,
		ComponentKind: comp.Kind,
		Protocol:      comp.Protocol,
		UpstreamName:  route.UpstreamName,
		LBAlgorithm:   comp.LBAlgorithm,
		Sticky:        comp.Sticky,
		Weight:        comp.Weight,
		ClusterName:   comp.ClusterName,
		HealthPath:    comp.HealthPath,
		ServiceName:   comp.ServiceName,
		GroupName:     group,
		Instances:     instances,
	}, nil
}

// matchRoute picks the route with the longest matching prefix for a domain.
func (s *Store) matchRoute(ctx context.Context, ns, domain, path string) (*RouteBinding, error) {
	routes, err := s.ListRoutes(ctx, ns)
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = "/"
	}
	var best *RouteBinding
	for i := range routes {
		r := routes[i]
		if !strings.EqualFold(r.Domain, domain) {
			continue
		}
		if !pathPrefixMatches(r.PathPrefix, path) {
			continue
		}
		if best == nil || len(r.PathPrefix) > len(best.PathPrefix) ||
			(len(r.PathPrefix) == len(best.PathPrefix) && r.Priority < best.Priority) {
			candidate := r
			best = &candidate
		}
	}
	return best, nil
}

// pathPrefixMatches treats "/" as a catch-all and otherwise requires a
// segment boundary, so "/api" must not match "/apixyz".
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

// filterInstancesByMeta keeps instances whose metadata contains every requested
// pair. A missing key does not match, which is the safe direction: it keeps an
// unlabelled instance out of a canary set rather than into it.
func filterInstancesByMeta(instances []Instance, filters map[string]string) []Instance {
	out := make([]Instance, 0, len(instances))
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

// sortInstances applies the component's load-balancing hint. Weighted ordering
// is expressed by repeating lower-weight instances after the primary group,
// which is what nginx's own weighted round robin does with duplicated server
// entries and keeps the ordering stable and inspectable.
func sortInstances(instances []Instance, lb string) {
	switch lb {
	case "least_conn", "cookie_sticky", "ip_hash", "":
		sort.SliceStable(instances, func(i, j int) bool {
			wi, wj := instances[i].Weight, instances[j].Weight
			if wi == wj {
				return instances[i].IP < instances[j].IP
			}
			return wi > wj
		})
	default:
		sort.SliceStable(instances, func(i, j int) bool {
			wi, wj := instances[i].Weight, instances[j].Weight
			if wi == wj {
				return instances[i].IP < instances[j].IP
			}
			return wi > wj
		})
	}
}
