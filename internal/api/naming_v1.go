package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// instanceParams extracts naming parameters from query/form/JSON body.
type instanceParams struct {
	NamespaceId string
	GroupName   string
	ServiceName string
	ClusterName string
	IP          string
	Port        uint32
	Weight      float64
	Enabled     bool
	Ephemeral   bool
	Metadata    map[string]string
	BeatJSON    string
}

func parseInstanceParams(r *http.Request) instanceParams {
	p := formOrJSON(r)
	ns := firstNonEmpty(p["namespaceId"], p["namespace_id"], p["tenant"])
	group := p["groupName"]
	if group == "" {
		group = p["group"]
	}
	group, name := parseServiceName(p["serviceName"], group)
	cluster := p["clusterName"]
	if cluster == "" {
		cluster = "DEFAULT"
	}
	port, _ := strconv.Atoi(p["port"])
	weight := 1.0
	if p["weight"] != "" {
		if f, err := strconv.ParseFloat(p["weight"], 64); err == nil && f > 0 {
			weight = f
		}
	}
	enabled := true
	if p["enabled"] != "" {
		enabled = p["enabled"] == "true"
	}
	ephemeral := true
	if p["ephemeral"] != "" {
		ephemeral = p["ephemeral"] == "true"
	}
	meta := metaFromParam(p["metadata"])
	return instanceParams{
		NamespaceId: ns, GroupName: group, ServiceName: name, ClusterName: cluster,
		IP: p["ip"], Port: uint32(port), Weight: weight, Enabled: enabled, Ephemeral: ephemeral,
		Metadata: meta, BeatJSON: p["beat"],
	}
}

func (p instanceParams) upsert() storage.InstanceUpsert {
	return storage.InstanceUpsert{
		NamespaceId: p.NamespaceId, GroupName: p.GroupName, ServiceName: p.ServiceName,
		ClusterName: p.ClusterName, IP: p.IP, Port: p.Port, Weight: p.Weight,
		Enabled: p.Enabled, Ephemeral: p.Ephemeral, Metadata: p.Metadata,
	}
}

// ---------- v1 naming ----------

func (s *Server) namingV1Register(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ServiceName == "" || p.IP == "" || p.Port == 0 {
		writeText(w, http.StatusBadRequest, "serviceName/ip/port required")
		return
	}
	if _, err := s.ST.RegisterInstance(r.Context(), p.upsert()); err != nil {
		writeText(w, http.StatusInternalServerError, "register failed: "+err.Error())
		return
	}
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) namingV1Deregister(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	ok, err := s.ST.DeleteInstance(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, p.ClusterName, p.IP, p.Port)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "deregister failed: "+err.Error())
		return
	}
	_ = ok
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) namingV1GetInstance(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	i, err := s.ST.GetInstance(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, p.ClusterName, p.IP, p.Port)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": err.Error()})
		return
	}
	if i == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "instance not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.instanceResp(i, p.GroupName, p.ServiceName, true))
}

func (s *Server) namingV1ListInstances(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ServiceName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "message": "serviceName required"})
		return
	}
	items, err := s.ST.ListInstances(r.Context(), storage.InstanceListQuery{
		NamespaceId: p.NamespaceId, GroupName: p.GroupName, ServiceName: p.ServiceName,
		Clusters: r.URL.Query().Get("clusters"), HealthyOnly: r.URL.Query().Get("healthyOnly") == "true",
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.serviceInfo(p.GroupName, p.ServiceName, r.URL.Query().Get("clusters"), items))
}

// namingV1Beat handles GET/PUT /nacos/v1/ns/instance/beat.
func (s *Server) namingV1Beat(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	// client beat payload may carry ip/port/serviceName overrides
	if p.BeatJSON != "" {
		var b struct {
			IP          string            `json:"ip"`
			Port        uint32            `json:"port"`
			Weight      float64           `json:"weight"`
			ServiceName string            `json:"serviceName"`
			Cluster     string            `json:"cluster"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := jsonDecode(r, &b); err == nil {
			if b.IP != "" {
				p.IP = b.IP
			}
			if b.Port > 0 {
				p.Port = b.Port
			}
			if b.ServiceName != "" {
				_, name := parseServiceName(b.ServiceName, p.GroupName)
				p.ServiceName = name
			}
			if b.Cluster != "" {
				p.ClusterName = b.Cluster
			}
		}
	}
	if p.ClusterName == "" {
		p.ClusterName = "DEFAULT"
	}
	created, err := s.ST.BeatInstance(r.Context(), p.upsert())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": err.Error()})
		return
	}
	resp := map[string]any{
		"clientBeatInterval": s.Cfg.HeartbeatIntervalMs,
		"code":               10200,
		"lightBeatEnabled":   true,
	}
	if created {
		resp["code"] = 10200
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) namingV1ListServices(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	group := qp.Get("groupName")
	if group == "" {
		group = qp.Get("group")
	}
	items, total, err := s.ST.ListServices(r.Context(), storage.ServiceListQuery{
		NamespaceId: firstNonEmpty(qp.Get("namespaceId"), qp.Get("tenant")),
		GroupName:   group, NameBlur: qp.Get("serviceNameParam"), PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": err.Error()})
		return
	}
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	names := make([]string, 0, len(items))
	for _, srow := range items {
		names = append(names, srow.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": total, "doms": names})
}

func (s *Server) namingV1ServiceDetail(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	srow, err := s.ST.GetService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if srow == nil {
		v1Fail(w, http.StatusNotFound, 404, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, s.serviceDetailResp(srow))
}

func (s *Server) serviceDetailResp(srow *storage.ServiceRow) map[string]any {
	items, _ := s.ST.ListInstances(context.Background(), storage.InstanceListQuery{
		NamespaceId: srow.NamespaceId, GroupName: srow.GroupName, ServiceName: srow.Name,
	})
	clusterMap := map[string]any{}
	healthyByCluster := map[string]int{}
	for _, i := range items {
		if i.Healthy {
			healthyByCluster[i.ClusterName]++
		}
	}
	for _, i := range items {
		if _, ok := clusterMap[i.ClusterName]; ok {
			continue
		}
		clusterMap[i.ClusterName] = map[string]any{
			"clusterName":   i.ClusterName,
			"metadata":      map[string]string{},
			"healthyCount":  healthyByCluster[i.ClusterName],
			"instanceCount": countCluster(items, i.ClusterName),
			"healthChecker": map[string]string{"type": "none"},
		}
	}
	if len(clusterMap) == 0 {
		clusterMap["DEFAULT"] = map[string]any{
			"clusterName": "DEFAULT", "metadata": map[string]string{},
			"healthyCount": 0, "instanceCount": 0, "healthChecker": map[string]string{"type": "none"},
		}
	}
	return map[string]any{
		"metadata": srow.Metadata(), "groupName": srow.GroupName, "namespaceId": srow.NamespaceId,
		"name": srow.Name, "selector": map[string]string{"type": "none"},
		"protectThreshold": srow.ProtectThreshold, "clusterMap": clusterMap,
		"ipCount": srow.IPCount, "clusterCount": srow.ClusterCount,
		"appId": srow.AppName,
	}
}

func countCluster(items []storage.Instance, cluster string) int {
	n := 0
	for _, i := range items {
		if i.ClusterName == cluster {
			n++
		}
	}
	return n
}

func (s *Server) namingV1UpdateService(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	var protect *float64
	if v := r.URL.Query().Get("protectThreshold"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			protect = &f
		}
	}
	var meta *string
	if v := r.URL.Query().Get("metadata"); v != "" {
		meta = &v
	}
	if err := s.ST.UpdateService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, protect, meta, nil); err != nil {
		writeText(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) namingV1DeleteService(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	ok, err := s.ST.DeleteService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		v1Fail(w, http.StatusBadRequest, 400, "service not empty or not found")
		return
	}
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) namingV1Metrics(w http.ResponseWriter, r *http.Request) {
	services, instances, err := s.ST.CountNaming(r.Context())
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	configs, _ := s.ST.CountConfigs(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"status":               map[string]any{"responsibilityForServiceCount": services},
		"serviceCount":         services,
		"instanceCount":        instances,
		"configCount":          configs,
		"subscribeCount":       0,
		"healthyInstanceCount": instances,
		"upTime":               time.Since(s.Started).String(),
	})
}
