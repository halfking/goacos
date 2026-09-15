package api

import (
	"net/http"
	"strconv"

	"github.com/halfking/goacos/internal/storage"
)

// ---------- v2 naming ----------

func (s *Server) namingV2Register(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ServiceName == "" || p.IP == "" || p.Port == 0 {
		v2Fail(w, http.StatusBadRequest, 400, "serviceName/ip/port required")
		return
	}
	if _, err := s.ST.RegisterInstance(r.Context(), p.upsert()); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, "ok")
}

func (s *Server) namingV2Deregister(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if _, err := s.ST.DeleteInstance(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, p.ClusterName, p.IP, p.Port); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, "ok")
}

func (s *Server) namingV2GetInstance(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	i, err := s.ST.GetInstance(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, p.ClusterName, p.IP, p.Port)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if i == nil {
		v2Fail(w, http.StatusOK, 300, "instance not exist")
		return
	}
	v2OK(w, s.instanceResp(i, p.GroupName, p.ServiceName, true))
}

func (s *Server) namingV2ListInstances(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ServiceName == "" {
		v2Fail(w, http.StatusBadRequest, 400, "serviceName required")
		return
	}
	qp := r.URL.Query()
	items, err := s.ST.ListInstances(r.Context(), storage.InstanceListQuery{
		NamespaceId: p.NamespaceId, GroupName: p.GroupName, ServiceName: p.ServiceName,
		Clusters: qp.Get("clusters"), HealthyOnly: qp.Get("healthyOnly") == "true",
	})
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, s.serviceInfo(p.GroupName, p.ServiceName, qp.Get("clusters"), items))
}

func (s *Server) namingV2Beat(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ClusterName == "" {
		p.ClusterName = "DEFAULT"
	}
	created, err := s.ST.BeatInstance(r.Context(), p.upsert())
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, map[string]any{
		"clientBeatInterval": s.Cfg.HeartbeatIntervalMs,
		"code":               10200,
		"lightBeatEnabled":   true,
		"created":            created,
	})
}

func (s *Server) namingV2ListServices(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	group := qp.Get("groupName")
	if group == "" {
		group = qp.Get("group")
	}
	items, total, err := s.ST.ListServices(r.Context(), storage.ServiceListQuery{
		NamespaceId: firstNonEmpty(qp.Get("namespaceId"), qp.Get("tenant")),
		GroupName:   group, NameBlur: qp.Get("serviceNameParam"),
		PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
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
		names = append(names, srow.GroupName+"@@"+srow.Name)
	}
	v2OK(w, map[string]any{
		"count": total, "pageNumber": pageNo, "pageSize": pageSize, "services": names,
	})
}

func (s *Server) namingV2ServiceDetail(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	srow, err := s.ST.GetService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if srow == nil {
		v2Fail(w, http.StatusOK, 300, "service not exist")
		return
	}
	v2OK(w, s.serviceDetailResp(srow))
}

func (s *Server) namingV2CreateService(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	if p.ServiceName == "" {
		v2Fail(w, http.StatusBadRequest, 400, "serviceName required")
		return
	}
	if err := s.ST.EnsureService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	var protect *float64
	var meta *string
	if v := p.Metadata; v != nil {
		m := mapToJSONString(v)
		meta = &m
	}
	if err := s.ST.UpdateService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, protect, meta, nil); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, true)
}

func (s *Server) namingV2UpdateService(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	qp := r.URL.Query()
	var protect *float64
	if v := firstNonEmpty(qp.Get("protectThreshold"), r.Header.Get("protectThreshold")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			protect = &f
		}
	}
	var meta *string
	if v := qp.Get("metadata"); v != "" {
		meta = &v
	}
	if err := s.ST.UpdateService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName, protect, meta, nil); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, true)
}

func (s *Server) namingV2DeleteService(w http.ResponseWriter, r *http.Request) {
	p := parseInstanceParams(r)
	ok, err := s.ST.DeleteService(r.Context(), p.NamespaceId, p.GroupName, p.ServiceName)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, ok)
}
