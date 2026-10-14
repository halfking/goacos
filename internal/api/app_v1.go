package api

// Application model endpoints.
//
// # Why these are not Nacos endpoints
//
// Everything under /nacos/... is a compatibility surface: a stock Nacos client
// must work against goacos unchanged. The application model has no Nacos
// equivalent, so it gets its own /goacos/... prefix. Mixing it into /nacos/
// would imply a compatibility guarantee this API does not make and would make
// it impossible to ever diverge from upstream Nacos.
//
// # Who calls these
//
// The gateway's route renderer is the primary consumer. It asks one question —
// "for this domain and path, which healthy instances should serve it" — and
// gets back policy and live instance state together. That single call is what
// replaces the two hand-maintained tables this platform drifted apart on.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/halfking/goacos/internal/storage"
)

// ---------- helpers ----------

func defaultNamespace(ns string) string {
	if ns == "" {
		return "public"
	}
	return ns
}

func appError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg, "data": nil})
}

// componentJSON is the wire shape of a component.
func componentJSON(c storage.ApplicationComponent) map[string]any {
	return map[string]any{
		"namespaceId": c.NamespaceId,
		"appName":     c.AppName,
		"name":        c.Name,
		"kind":        c.Kind,
		"protocol":    c.Protocol,
		"serviceName": c.ServiceName,
		"groupName":   c.GroupName,
		"clusterName": c.ClusterName,
		"healthPath":  c.HealthPath,
		"weight":      c.Weight,
		"lbAlgorithm": c.LBAlgorithm,
		"sticky":      c.Sticky,
		"metadata":    c.Metadata,
	}
}

// ---------- applications ----------

func (s *Server) appList(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	items, err := s.ST.ListApplications(r.Context(), ns)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		out = append(out, map[string]any{
			"namespaceId": a.NamespaceId,
			"name":        a.Name,
			"displayName": a.DisplayName,
			"description": a.Description,
			"env":         a.Env,
			"metadata":    a.Metadata,
		})
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: out})
}

func (s *Server) appGet(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	name := r.PathValue("app")
	app, err := s.ST.GetApplication(r.Context(), ns, name)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if app == nil {
		appError(w, http.StatusNotFound, 404, "application not found: "+name)
		return
	}
	comps, err := s.ST.ListComponents(r.Context(), ns, name)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(comps))
	for _, c := range comps {
		out = append(out, componentJSON(c))
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"namespaceId": app.NamespaceId,
		"name":        app.Name,
		"displayName": app.DisplayName,
		"description": app.Description,
		"env":         app.Env,
		"metadata":    app.Metadata,
		"components":  out,
	}})
}

// appWrite accepts either JSON or form-encoded input, because the two callers
// in this platform disagree: the gateway renderer posts JSON, while operators
// drive it from curl during an incident.
func (s *Server) appWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NamespaceId string            `json:"namespaceId"`
		Name        string            `json:"name"`
		DisplayName string            `json:"displayName"`
		Description string            `json:"description"`
		Env         string            `json:"env"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := jsonDecode(r, &body); err != nil {
		f := formOrJSON(r)
		body = struct {
			NamespaceId string            `json:"namespaceId"`
			Name        string            `json:"name"`
			DisplayName string            `json:"displayName"`
			Description string            `json:"description"`
			Env         string            `json:"env"`
			Metadata    map[string]string `json:"metadata"`
		}{
			NamespaceId: f["namespaceId"],
			Name:        f["name"],
			DisplayName: f["displayName"],
			Description: f["description"],
			Env:         f["env"],
		}
	}
	if body.Name == "" {
		appError(w, http.StatusBadRequest, 400, "name is required")
		return
	}
	created, err := s.ST.UpsertApplication(r.Context(), storage.Application{
		NamespaceId: defaultNamespace(body.NamespaceId),
		Name:        body.Name,
		DisplayName: body.DisplayName,
		Description: body.Description,
		Env:         body.Env,
		Metadata:    body.Metadata,
	})
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Message: "", Data: map[string]any{
		"created": created, "name": body.Name,
	}})
}

func (s *Server) appDelete(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	name := r.PathValue("app")
	if err := s.ST.DeleteApplication(r.Context(), ns, name); err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{"deleted": name}})
}

// ---------- components ----------

func (s *Server) componentList(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	app := r.PathValue("app")
	comps, err := s.ST.ListComponents(r.Context(), ns, app)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(comps))
	for _, c := range comps {
		out = append(out, componentJSON(c))
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: out})
}

func (s *Server) componentWrite(w http.ResponseWriter, r *http.Request) {
	var body componentSpec
	if err := jsonDecode(r, &body); err != nil {
		f := formOrJSON(r)
		body.NamespaceId = f["namespaceId"]
		body.AppName = r.PathValue("app")
		body.Name = f["name"]
		body.Kind = f["kind"]
		body.Protocol = f["protocol"]
		body.ServiceName = f["serviceName"]
		body.GroupName = f["groupName"]
		body.ClusterName = f["clusterName"]
		body.HealthPath = f["healthPath"]
		body.Sticky = f["sticky"]
		if v, err := strconv.ParseFloat(f["weight"], 64); err == nil {
			body.Weight = v
		}
	}
	// The URL is authoritative for identity: PUT .../components/{component}
	// must address that component, not whatever the body happens to name.
	if fromPath := r.PathValue("component"); fromPath != "" {
		body.Name = fromPath
	}
	if body.Name == "" {
		appError(w, http.StatusBadRequest, 400, "name is required in the path or the body")
		return
	}
	if body.AppName == "" {
		body.AppName = r.PathValue("app")
	}
	if body.ServiceName == "" {
		appError(w, http.StatusBadRequest, 400, "serviceName is required")
		return
	}
	if err := normalizeComponentSpec(&body); err != nil {
		appError(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	created, err := s.ST.UpsertComponent(r.Context(), storage.ApplicationComponent{
		NamespaceId: defaultNamespace(body.NamespaceId),
		AppName:     body.AppName,
		Name:        body.Name,
		Kind:        body.Kind,
		Protocol:    body.Protocol,
		ServiceName: body.ServiceName,
		GroupName:   body.GroupName,
		ClusterName: body.ClusterName,
		HealthPath:  body.HealthPath,
		Weight:      body.Weight,
		LBAlgorithm: body.LBAlgorithm,
		Sticky:      body.Sticky,
		Metadata:    body.Metadata,
	})
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{"created": created}})
}

func (s *Server) componentDelete(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	if err := s.ST.DeleteComponent(r.Context(), ns, r.PathValue("app"), r.PathValue("component")); err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{"deleted": true}})
}

// ---------- routes ----------

func (s *Server) routeList(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	routes, err := s.ST.ListRoutes(r.Context(), ns)
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(routes))
	for _, rt := range routes {
		out = append(out, routeJSON(rt))
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: out})
}

func routeJSON(rt storage.RouteBinding) map[string]any {
	return map[string]any{
		"namespaceId":   rt.NamespaceId,
		"domain":        rt.Domain,
		"pathPrefix":    rt.PathPrefix,
		"appName":       rt.AppName,
		"componentName": rt.ComponentName,
		"upstreamName":  rt.UpstreamName,
		"priority":      rt.Priority,
	}
}

func (s *Server) routeWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NamespaceId   string `json:"namespaceId"`
		Domain        string `json:"domain"`
		PathPrefix    string `json:"pathPrefix"`
		AppName       string `json:"appName"`
		ComponentName string `json:"componentName"`
		UpstreamName  string `json:"upstreamName"`
		Priority      int    `json:"priority"`
	}
	if err := jsonDecode(r, &body); err != nil {
		f := formOrJSON(r)
		body.NamespaceId = f["namespaceId"]
		body.Domain = f["domain"]
		body.PathPrefix = f["pathPrefix"]
		body.AppName = f["appName"]
		body.ComponentName = f["componentName"]
		body.UpstreamName = f["upstreamName"]
		if v, err := strconv.Atoi(f["priority"]); err == nil {
			body.Priority = v
		}
	}
	if body.Domain == "" || body.AppName == "" || body.ComponentName == "" {
		appError(w, http.StatusBadRequest, 400, "domain, appName and componentName are required")
		return
	}
	if body.PathPrefix == "" {
		body.PathPrefix = "/"
	}
	err := s.ST.UpsertRoute(r.Context(), storage.RouteBinding{
		NamespaceId:   defaultNamespace(body.NamespaceId),
		Domain:        strings.ToLower(body.Domain),
		PathPrefix:    body.PathPrefix,
		AppName:       body.AppName,
		ComponentName: body.ComponentName,
		UpstreamName:  body.UpstreamName,
		Priority:      body.Priority,
	})
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{"saved": true}})
}

func (s *Server) routeDelete(w http.ResponseWriter, r *http.Request) {
	ns := defaultNamespace(r.URL.Query().Get("namespaceId"))
	prefix := r.URL.Query().Get("pathPrefix")
	if prefix == "" {
		prefix = "/"
	}
	if err := s.ST.DeleteRoute(r.Context(), ns, strings.ToLower(r.PathValue("domain")), prefix); err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{"deleted": true}})
}

// routeResolve is the endpoint the gateway renderer depends on.
//
// It answers one question with one round trip: given a domain and path, which
// healthy instances serve it, and under what policy. An unresolved domain is a
// 200 with a null payload rather than a 404, because "not declared here" is a
// normal answer the renderer must be able to distinguish from a database error.
//
// metaFilter arrives as "key=value,key2=value2" so a canary or lane can be
// selected without a second endpoint.
func (s *Server) routeResolve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ns := defaultNamespace(q.Get("namespaceId"))
	domain := q.Get("domain")
	if domain == "" {
		appError(w, http.StatusBadRequest, 400, "domain is required")
		return
	}
	path := q.Get("path")
	if path == "" {
		path = "/"
	}
	resolved, err := s.ST.ResolveRoute(r.Context(), ns, strings.ToLower(domain), path, parseMetaFilter(q.Get("metaFilter")))
	if err != nil {
		appError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: nil})
		return
	}
	instances := make([]map[string]any, 0, len(resolved.Instances))
	for _, inst := range resolved.Instances {
		instances = append(instances, map[string]any{
			"ip":        inst.IP,
			"port":      inst.Port,
			"cluster":   inst.ClusterName,
			"weight":    inst.Weight,
			"healthy":   inst.Healthy,
			"enabled":   inst.Enabled,
			"ephemeral": inst.Ephemeral,
			"metadata":  inst.Metadata(),
		})
	}
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Data: map[string]any{
		"namespaceId":   resolved.NamespaceId,
		"domain":        resolved.Domain,
		"pathPrefix":    resolved.PathPrefix,
		"appName":       resolved.AppName,
		"componentName": resolved.ComponentName,
		"componentKind": resolved.ComponentKind,
		"protocol":      resolved.Protocol,
		"upstreamName":  resolved.UpstreamName,
		"lbAlgorithm":   resolved.LBAlgorithm,
		"sticky":        resolved.Sticky,
		"serviceName":   resolved.ServiceName,
		"groupName":     resolved.GroupName,
		"clusterName":   resolved.ClusterName,
		"healthPath":    resolved.HealthPath,
		"instances":     instances,
	}})
}

// parseMetaFilter reads "k=v,k2=v2". A malformed pair is dropped rather than
// failing the whole request: a partially applied filter is still a narrower,
// well-defined answer, whereas an error would take a live route offline.
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
