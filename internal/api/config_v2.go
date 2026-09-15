package api

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

func md5Sum(b []byte) [md5.Size]byte  { return md5.Sum(b) }
func hexOf(sum [md5.Size]byte) string { return hex.EncodeToString(sum[:]) }

// ---------- v2 config ----------

// configV2Get handles GET /nacos/v2/cs/config.
func (s *Server) configV2Get(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	dataId, group := qp.Get("dataId"), qp.Get("group")
	tenant := qp.Get("namespaceId")
	if tenant == "" {
		tenant = qp.Get("tenant")
	}
	if dataId == "" || group == "" {
		v2Fail(w, http.StatusBadRequest, 400, "dataId or group is empty")
		return
	}
	c, err := s.ST.GetConfig(r.Context(), dataId, group, tenant)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if c == nil {
		v2Fail(w, http.StatusOK, 300, "config not exist")
		return
	}
	v2OK(w, respFromConfig(c))
}

// configV2Publish handles POST /nacos/v2/cs/config.
func (s *Server) configV2Publish(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	dataId, group := p["dataId"], p["group"]
	tenant := p["namespaceId"]
	if tenant == "" {
		tenant = p["tenant"]
	}
	if dataId == "" || group == "" {
		v2Fail(w, http.StatusBadRequest, 400, "dataId or group is empty")
		return
	}
	user := "anonymous"
	if c := ClaimsFrom(r.Context()); c != nil {
		user = c.Username
	}
	var tags []string
	if p["configTags"] != "" {
		tags = strings.Split(p["configTags"], ",")
	} else if p["config_tags"] != "" {
		tags = strings.Split(p["config_tags"], ",")
	}
	id, _, err := s.ST.UpsertConfig(r.Context(), storage.ConfigUpsert{
		DataId: dataId, GroupId: group, TenantId: tenant, Content: p["content"],
		SrcUser: user, SrcIp: srcIP(r), AppName: p["appName"],
		Type: configType(dataId, p["type"]), CDesc: firstNonEmpty(p["desc"], p["cDesc"]),
	})
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if len(tags) > 0 {
		_ = s.ST.ReplaceConfigTags(r.Context(), id, dataId, group, tenant, tags)
	}
	s.Hub.Publish(core.ConfigKey{Tenant: tenant, Group: group, DataId: dataId}, md5Of(p["content"]))
	v2OK(w, true)
}

// configV2Delete handles DELETE /nacos/v2/cs/config.
func (s *Server) configV2Delete(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	dataId, group := qp.Get("dataId"), qp.Get("group")
	tenant := qp.Get("namespaceId")
	if tenant == "" {
		tenant = qp.Get("tenant")
	}
	if dataId == "" || group == "" {
		v2Fail(w, http.StatusBadRequest, 400, "dataId or group is empty")
		return
	}
	user := "anonymous"
	if c := ClaimsFrom(r.Context()); c != nil {
		user = c.Username
	}
	ok, err := s.ST.DeleteConfig(r.Context(), dataId, group, tenant, user, srcIP(r))
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if ok {
		s.Hub.Invalidate(core.ConfigKey{Tenant: tenant, Group: group, DataId: dataId})
	}
	v2OK(w, ok)
}

// configV2List handles GET /nacos/v2/cs/config/list.
func (s *Server) configV2List(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	tenant := qp.Get("namespaceId")
	if tenant == "" {
		tenant = qp.Get("tenant")
	}
	items, total, err := s.ST.ListConfigs(r.Context(), storage.ListQuery{
		TenantId: tenant, DataId: qp.Get("dataId"), GroupId: qp.Get("group"),
		Accurate: qp.Get("search") == "accurate", PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	resp := make([]ConfigResp, 0, len(items))
	for i := range items {
		resp = append(resp, respFromConfig(&items[i]))
	}
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}
	v2OK(w, newPage(total, pageNo, pageSize, resp))
}

// configV2ListenerInfo is one entry of the v2 listener request.
type configV2ListenerInfo struct {
	DataId      string `json:"dataId"`
	Group       string `json:"group"`
	NamespaceId string `json:"namespaceId"`
	Md5         string `json:"md5"`
}

type configV2ListenerRequest struct {
	ConfigInfos []configV2ListenerInfo `json:"configInfos"`
}

// configV2Listener handles POST /nacos/v2/cs/config/listener (long polling).
func (s *Server) configV2Listener(w http.ResponseWriter, r *http.Request) {
	var req configV2ListenerRequest
	if err := jsonDecode(r, &req); err != nil || len(req.ConfigInfos) == 0 {
		v2OK(w, []configV2ListenerInfo{})
		return
	}
	known := map[core.ConfigKey]string{}
	keys := make([]core.ConfigKey, 0, len(req.ConfigInfos))
	for _, ci := range req.ConfigInfos {
		k := core.ConfigKey{Tenant: ci.NamespaceId, Group: ci.Group, DataId: ci.DataId}
		known[k] = ci.Md5
		keys = append(keys, k)
	}
	changed := s.Hub.WaitChanged(r.Context(), keys, known, 29500*time.Millisecond)
	out := make([]configV2ListenerInfo, 0, len(changed))
	for _, k := range changed {
		out = append(out, configV2ListenerInfo{DataId: k.DataId, Group: k.Group, NamespaceId: k.Tenant})
	}
	v2OK(w, out)
}

// ---------- v2 history ----------

func (s *Server) configV2HistoryList(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	tenant := qp.Get("namespaceId")
	if tenant == "" {
		tenant = qp.Get("tenant")
	}
	items, total, err := s.ST.ListHistory(r.Context(), storage.HistoryListQuery{
		TenantId: tenant, DataId: qp.Get("dataId"), GroupId: qp.Get("group"),
		Accurate: true, PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	resp := make([]HistoryResp, 0, len(items))
	for i := range items {
		resp = append(resp, respFromHistory(&items[i], false))
	}
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}
	v2OK(w, newPage(total, pageNo, pageSize, resp))
}

func (s *Server) configV2HistoryDetail(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	nid, _ := strconv.ParseInt(qp.Get("nid"), 10, 64)
	if nid == 0 {
		nid, _ = strconv.ParseInt(qp.Get("id"), 10, 64)
	}
	h, err := s.ST.GetHistory(r.Context(), nid)
	if err != nil {
		v2Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if h == nil {
		v2Fail(w, http.StatusOK, 300, "history not exist")
		return
	}
	v2OK(w, respFromHistory(h, true))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
