package api

import (
	"net/http"

	"github.com/halfking/goacos/internal/buildinfo"
)

// ---------- console / ops endpoints ----------

func (s *Server) namespaceListV1(w http.ResponseWriter, r *http.Request) {
	items, err := s.ST.ListNamespaces(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "message": err.Error()})
		return
	}
	type nsInfo struct {
		Namespace         string `json:"namespace"`
		NamespaceShowName string `json:"namespaceShowName"`
		NamespaceDesc     string `json:"namespaceDesc"`
		Quota             int64  `json:"quota"`
		ConfigCount       int64  `json:"configCount"`
		Type              int    `json:"type"`
	}
	out := make([]nsInfo, 0, len(items))
	for _, n := range items {
		out = append(out, nsInfo{
			Namespace: n.NamespaceId, NamespaceShowName: n.Name, NamespaceDesc: n.Desc,
			Quota: 200, ConfigCount: n.ConfigCount, Type: 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "message": nil, "data": out})
}

func (s *Server) namespaceListV2(w http.ResponseWriter, r *http.Request) {
	items, err := s.ST.ListNamespaces(r.Context())
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type nsInfo struct {
		Namespace         string `json:"namespace"`
		NamespaceShowName string `json:"namespaceShowName"`
		NamespaceDesc     string `json:"namespaceDesc"`
		Quota             int64  `json:"quota"`
		ConfigCount       int64  `json:"configCount"`
		Type              int    `json:"type"`
	}
	out := make([]nsInfo, 0, len(items))
	for _, n := range items {
		out = append(out, nsInfo{
			Namespace: n.NamespaceId, NamespaceShowName: n.Name, NamespaceDesc: n.Desc,
			Quota: 200, ConfigCount: n.ConfigCount, Type: 0,
		})
	}
	v2OK(w, out)
}

func (s *Server) namespaceCreateV1(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	id := firstNonEmpty(p["customNamespaceId"], p["namespaceId"])
	name := firstNonEmpty(p["customNamespaceName"], p["namespaceName"])
	if id == "" || name == "" {
		v1Fail(w, http.StatusBadRequest, 400, "namespaceId/namespaceName required")
		return
	}
	ok, err := s.ST.CreateNamespace(r.Context(), id, name, p["namespaceDesc"])
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		v1Fail(w, http.StatusBadRequest, 400, "namespace already exists")
		return
	}
	v1OK(w, "ok")
}

func (s *Server) namespaceCreateV2(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	id := firstNonEmpty(p["customNamespaceId"], p["namespaceId"])
	name := firstNonEmpty(p["customNamespaceName"], p["namespaceName"])
	if id == "" || name == "" {
		v2Fail(w, http.StatusBadRequest, 400, "namespaceId/namespaceName required")
		return
	}
	ok, err := s.ST.CreateNamespace(r.Context(), id, name, p["namespaceDesc"])
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		v2Fail(w, http.StatusBadRequest, 400, "namespace already exists")
		return
	}
	v2OK(w, true)
}

func (s *Server) namespaceUpdateV1(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	id := p["namespaceId"]
	if id == "" {
		v1Fail(w, http.StatusBadRequest, 400, "namespaceId required")
		return
	}
	name := firstNonEmpty(p["customNamespaceName"], p["namespaceName"])
	if err := s.ST.UpdateNamespace(r.Context(), id, name, p["namespaceDesc"]); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v1OK(w, "ok")
}

func (s *Server) namespaceUpdateV2(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	id := p["namespaceId"]
	if id == "" {
		v2Fail(w, http.StatusBadRequest, 400, "namespaceId required")
		return
	}
	name := firstNonEmpty(p["customNamespaceName"], p["namespaceName"])
	if err := s.ST.UpdateNamespace(r.Context(), id, name, p["namespaceDesc"]); err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v2OK(w, true)
}

func (s *Server) namespaceDeleteV1(w http.ResponseWriter, r *http.Request) {
	id := q(r, "namespaceId")
	if id == "" {
		v1Fail(w, http.StatusBadRequest, 400, "namespaceId required")
		return
	}
	ok, reason, err := s.ST.DeleteNamespace(r.Context(), id)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		msg := "namespace not found"
		if reason != "" {
			msg = reason
		}
		v1Fail(w, http.StatusBadRequest, 400, msg)
		return
	}
	v1OK(w, "ok")
}

func (s *Server) namespaceDeleteV2(w http.ResponseWriter, r *http.Request) {
	id := q(r, "namespaceId")
	if id == "" {
		v2Fail(w, http.StatusBadRequest, 400, "namespaceId required")
		return
	}
	ok, reason, err := s.ST.DeleteNamespace(r.Context(), id)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		msg := "namespace not found"
		if reason != "" {
			msg = reason
		}
		v2Fail(w, http.StatusBadRequest, 400, msg)
		return
	}
	v2OK(w, true)
}

// serverState handles GET /nacos/v1/console/server/state.
func (s *Server) serverState(w http.ResponseWriter, r *http.Request) {
	runtime := buildinfo.RuntimeInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"consoleUiEnabled": true,
		"functionMode":     "ALL",
		"version":          buildinfo.Version,
		"gitCommit":        buildinfo.GitCommit,
		"buildDate":        buildinfo.BuildDate,
		"compatibility":    buildinfo.NacosAPICompat,
		"standaloneMode":   true,
		"authEnabled":      s.Cfg.AuthEnabled,
		"runtime":          runtime,
	})
}

func (s *Server) healthReadiness(w http.ResponseWriter, r *http.Request) {
	if err := s.ST.DB.PingContext(r.Context()); err != nil {
		writeText(w, http.StatusServiceUnavailable, "DOWN")
		return
	}
	writeText(w, http.StatusOK, "OK")
}

func (s *Server) healthLiveness(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusOK, "OK")
}

// actuatorHealth handles GET /nacos/actuator/health.
func (s *Server) actuatorHealth(w http.ResponseWriter, r *http.Request) {
	status := "UP"
	if err := s.ST.DB.PingContext(r.Context()); err != nil {
		status = "DOWN"
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"DOWN"}`))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status,
		"components": map[string]any{
			"db":        map[string]string{"status": status},
			"ping":      map[string]string{"status": status},
			"diskSpace": map[string]string{"status": status},
		},
	})
}
