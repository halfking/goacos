package registry

// Publishing an application model: application → components → routes.
//
// # Why this file exists
//
// The naming registry answers "who is healthy for service X". It cannot answer
// "where should acc-api.itestu.cn go" — that question needs a domain, an
// application boundary and a component role (frontend vs API vs worker), and
// none of those are in the Nacos vocabulary.
//
// goacos has a model for all three: `application`, `application_component`
// and `route_binding`. The model existed and **nothing wrote it**.
//
// That is the gap this file closes. Before it, the only writer of the
// application model in the whole workspace was a test fixture, so a service
// that wanted to be an application had no way to say so: the SDK spoke the
// Nacos naming API and nothing else. The model was a set of empty tables with
// a gateway perfectly able to read them.
//
// # What publishing means here
//
// Publishing is **declaring**, not registering. It says "this service is an
// application with these components, reachable under these domains". It does
// not say which host answers: the instances still come from the naming
// registry, live, at resolve time. Nothing here pins an address.
//
// # Every write is an upsert
//
// `POST /goacos/v1/apps`, `PUT .../components/{component}` and
// `POST /goacos/v1/routes` are all upserts, keyed by identity. Republishing the
// same declaration is therefore harmless — which is what lets every replica
// publish it without a leader election: the writes are the same writes.
//
// The one thing that is NOT idempotent is a route's identity: a route is keyed
// by (namespace, domain, pathPrefix). Publishing a route with a different
// component under the same key **moves** traffic, so `PublishRoute` refuses a
// route that has no component, and the caller is expected to mean it.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Application is one product deployed in one namespace.
//
// The name is the application's identity in the gateway's routing table, not
// a service name: an application may have several components, each backed by
// its own naming service.
type Application struct {
	NamespaceID string
	Name        string
	DisplayName string
	Description string
	Env         string
	Metadata    map[string]string
}

// Component is one addressable piece of an application.
//
// ServiceName is the naming service the gateway resolves the component's
// instances from. It is the same name the component's own instances register
// under, which is why the two sides must agree — and why the SDK does not
// invent one.
type Component struct {
	NamespaceID string
	AppName     string
	Name        string
	// Kind is the component's role: web, api, worker, mcp or gateway.
	// It decides the default load-balancing algorithm (a frontend defaults to
	// a sticky algorithm; an API defaults to round robin).
	Kind string
	// Protocol is http, https, static, grpc or ws. It decides whether the
	// component is instance-backed and how the gateway dials it.
	Protocol string
	// ServiceName is the naming service behind this component.
	ServiceName string
	GroupName   string
	ClusterName string
	HealthPath  string
	Weight      float64
	// LBAlgorithm is round_robin, ip_hash, least_conn or cookie_sticky.
	// Empty means "the server's default for this kind".
	LBAlgorithm string
	Sticky      string
	Metadata    map[string]string
}

// Route binds an inbound domain and path prefix to one component.
//
// The route carries no address either: it says "requests for this domain and
// path go to this component", and the component's live instances decide where.
type Route struct {
	NamespaceID string
	Domain      string
	PathPrefix  string
	AppName     string
	// ComponentName is the component inside AppName that serves this path.
	ComponentName string
	// UpstreamName is the operator-facing name of the upstream the gateway
	// reports for this route. It is a label, not an address.
	UpstreamName string
	// Priority breaks ties between routes with the same path prefix.
	Priority int
}

// ErrNilApplicationSpec reports a declaration that cannot be published.
//
// It is returned rather than a partially applied write: publishing half an
// application model is worse than publishing none, because the gateway would
// route part of a domain to a component that has no declaration behind it.
var ErrNilApplicationSpec = errors.New("registry: application declaration is incomplete")

// PublishApplication writes one application.
//
// The write is an upsert, so publishing the same application twice is
// harmless.
func (c *Client) PublishApplication(ctx context.Context, app Application) error {
	if c == nil {
		return ErrNilClient
	}
	if strings.TrimSpace(app.Name) == "" {
		return fmt.Errorf("%w: an application needs a name", ErrNilApplicationSpec)
	}
	body := map[string]any{
		"namespaceId": app.NamespaceID,
		"name":        app.Name,
		"displayName": app.DisplayName,
		"description": app.Description,
		"env":         app.Env,
	}
	if len(app.Metadata) > 0 {
		body["metadata"] = app.Metadata
	}
	return c.doAt(ctx, "POST", goacosPrefix, "/apps", nil, body, nil)
}

// PublishComponent writes one component of an application.
//
// The component's identity is (app, component). The write is an upsert.
func (c *Client) PublishComponent(ctx context.Context, comp Component) error {
	if c == nil {
		return ErrNilClient
	}
	if strings.TrimSpace(comp.AppName) == "" || strings.TrimSpace(comp.Name) == "" {
		return fmt.Errorf("%w: a component needs an app name and a component name",
			ErrNilApplicationSpec)
	}
	body := map[string]any{
		"namespaceId": comp.NamespaceID,
		"appName":     comp.AppName,
		"name":        comp.Name,
		"kind":        comp.Kind,
		"protocol":    comp.Protocol,
		"serviceName": comp.ServiceName,
		"groupName":   comp.GroupName,
		"clusterName": comp.ClusterName,
		"healthPath":  comp.HealthPath,
		"weight":      comp.Weight,
		"lbAlgorithm": comp.LBAlgorithm,
		"sticky":      comp.Sticky,
	}
	if len(comp.Metadata) > 0 {
		body["metadata"] = comp.Metadata
	}
	// PUT on the addressed component: the URL carries the identity, so the
	// write cannot land on a different component than the caller meant.
	path := "/apps/" + url.PathEscape(comp.AppName) + "/components/" + url.PathEscape(comp.Name)
	return c.doAt(ctx, "PUT", goacosPrefix, path, nil, body, nil)
}

// PublishRoute binds a domain and path prefix to a component.
//
// A route with no component is refused: the gateway would have nothing to
// forward to, and a route that resolves to nothing looks configured while
// answering 404 for every request.
func (c *Client) PublishRoute(ctx context.Context, route Route) error {
	if c == nil {
		return ErrNilClient
	}
	if strings.TrimSpace(route.Domain) == "" ||
		strings.TrimSpace(route.AppName) == "" ||
		strings.TrimSpace(route.ComponentName) == "" {
		return fmt.Errorf("%w: a route needs a domain, an app name and a component name",
			ErrNilApplicationSpec)
	}
	pathPrefix := route.PathPrefix
	if pathPrefix == "" {
		pathPrefix = "/"
	}
	body := map[string]any{
		"namespaceId":   route.NamespaceID,
		"domain":        route.Domain,
		"pathPrefix":    pathPrefix,
		"appName":       route.AppName,
		"componentName": route.ComponentName,
		"upstreamName":  route.UpstreamName,
		"priority":      route.Priority,
	}
	return c.doAt(ctx, "POST", goacosPrefix, "/routes", nil, body, nil)
}

// PublishApplicationModel publishes an application with its components and
// routes, in that order.
//
// The order is the dependency order: a route points at a component, and a
// component belongs to an application. Publishing them in any other order
// would leave a route pointing at something that does not exist yet.
//
// The call stops at the first failure and reports which step failed, because
// a half-published model is a real state that somebody has to know about —
// the gateway would route part of a domain to a component that has no
// declaration behind it, and nothing in the status output would say so.
//
// It is safe to call on every replica: every write is an upsert keyed by
// identity, so the same declaration written twice is the same declaration.
func (c *Client) PublishApplicationModel(
	ctx context.Context,
	app Application,
	components []Component,
	routes []Route,
) error {
	if c == nil {
		return ErrNilClient
	}
	if err := c.PublishApplication(ctx, app); err != nil {
		return fmt.Errorf("publish application %q: %w", app.Name, err)
	}
	for _, comp := range components {
		if comp.NamespaceID == "" {
			comp.NamespaceID = app.NamespaceID
		}
		if comp.AppName == "" {
			comp.AppName = app.Name
		}
		if err := c.PublishComponent(ctx, comp); err != nil {
			return fmt.Errorf("publish component %q/%q: %w", comp.AppName, comp.Name, err)
		}
	}
	for _, route := range routes {
		if route.NamespaceID == "" {
			route.NamespaceID = app.NamespaceID
		}
		if route.AppName == "" {
			route.AppName = app.Name
		}
		if err := c.PublishRoute(ctx, route); err != nil {
			return fmt.Errorf("publish route %q%s: %w", route.Domain, route.PathPrefix, err)
		}
	}
	return nil
}
