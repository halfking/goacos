package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

// ---------- v1 ----------

// configV1Get handles GET /nacos/v1/cs/configs — raw content, show=all JSON,
// or a page listing when search/pageNo params are present.
func (s *Server) configV1Get(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	if qp.Has("search") || qp.Has("pageNo") {
		s.configV1List(w, r)
		return
	}
	dataId, group, tenant := qp.Get("dataId"), qp.Get("group"), qp.Get("tenant")
	if dataId == "" || group == "" {
		writeText(w, http.StatusBadRequest, "dataId or group is empty")
		return
	}
	c, err := s.ST.GetConfig(r.Context(), dataId, group, tenant)
	if err != nil {
		writeText(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c == nil {
		writeText(w, http.StatusNotFound, "config data not exist")
		return
	}
	if qp.Get("show") == "all" {
		writeJSON(w, http.StatusOK, respFromConfig(c))
		return
	}
	writeText(w, http.StatusOK, c.Content)
}

func (s *Server) configV1List(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	accurate := qp.Get("search") == "accurate"
	items, total, err := s.ST.ListConfigs(r.Context(), storage.ListQuery{
		TenantId: qp.Get("tenant"), DataId: qp.Get("dataId"), GroupId: qp.Get("group"),
		AppName: qp.Get("appName"), Tag: qp.Get("config_tags"),
		Accurate: accurate, PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, V1Result{Code: 500, Message: err.Error()})
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
	writeJSON(w, http.StatusOK, newPage(total, pageNo, pageSize, resp))
}

// configV1Publish handles POST /nacos/v1/cs/configs.
func (s *Server) configV1Publish(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	dataId, group, tenant := p["dataId"], p["group"], p["tenant"]
	if dataId == "" || group == "" {
		writeText(w, http.StatusBadRequest, "dataId or group is empty")
		return
	}
	content := p["content"]
	user := "anonymous"
	if c := ClaimsFrom(r.Context()); c != nil {
		user = c.Username
	}
	var tags []string
	if p["config_tags"] != "" {
		tags = strings.Split(p["config_tags"], ",")
	}
	id, _, err := s.ST.UpsertConfig(r.Context(), storage.ConfigUpsert{
		DataId: dataId, GroupId: group, TenantId: tenant, Content: content,
		SrcUser: user, SrcIp: srcIP(r), AppName: p["appName"],
		Type: configType(dataId, p["type"]), CDesc: p["desc"], CUse: p["use"], Effect: p["effect"], CSchema: p["schema"],
	})
	if err != nil {
		writeText(w, http.StatusInternalServerError, "publish failed: "+err.Error())
		return
	}
	if len(tags) > 0 {
		_ = s.ST.ReplaceConfigTags(r.Context(), id, dataId, group, tenant, tags)
	}
	s.Hub.Publish(core.ConfigKey{Tenant: tenant, Group: group, DataId: dataId}, md5Of(content))
	writeText(w, http.StatusOK, "true")
}

// configV1Delete handles DELETE /nacos/v1/cs/configs.
func (s *Server) configV1Delete(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	dataId, group, tenant := qp.Get("dataId"), qp.Get("group"), qp.Get("tenant")
	if dataId == "" || group == "" {
		writeText(w, http.StatusBadRequest, "dataId or group is empty")
		return
	}
	user := "anonymous"
	if c := ClaimsFrom(r.Context()); c != nil {
		user = c.Username
	}
	ok, err := s.ST.DeleteConfig(r.Context(), dataId, group, tenant, user, srcIP(r))
	if err != nil {
		writeText(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	if ok {
		s.Hub.Invalidate(core.ConfigKey{Tenant: tenant, Group: group, DataId: dataId})
	}
	writeText(w, http.StatusOK, "true")
}

// ListenKey is one entry of a long-poll request.
type ListenKey struct {
	Key core.ConfigKey
	Md5 string
}

// parseV1ListeningConfigs decodes the ^2/^1 separated listening payload.
func parseV1ListeningConfigs(raw string) []ListenKey {
	var out []ListenKey
	for _, entry := range strings.Split(raw, "\x01") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "\x02")
		if len(parts) < 3 {
			continue
		}
		k := ListenKey{Key: core.ConfigKey{DataId: parts[0], Group: parts[1]}, Md5: parts[2]}
		if len(parts) > 3 {
			k.Key.Tenant = parts[3]
		}
		out = append(out, k)
	}
	return out
}

// configV1Listener handles POST /nacos/v1/cs/configs/listener (long polling).
func (s *Server) configV1Listener(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	raw := r.Form.Get("Listening-Configs")
	keys := parseV1ListeningConfigs(raw)
	if len(keys) == 0 {
		writeText(w, http.StatusOK, "")
		return
	}
	timeout := 29 * time.Second
	if h := r.Header.Get("Long-Pulling-Timeout"); h != "" {
		if ms, err := strconv.ParseInt(h, 10, 64); err == nil && ms > 0 {
			if ms > 60000 {
				ms = 60000
			}
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	known := map[core.ConfigKey]string{}
	ckeys := make([]core.ConfigKey, 0, len(keys))
	for _, k := range keys {
		known[k.Key] = k.Md5
		ckeys = append(ckeys, k.Key)
	}
	changed := s.Hub.WaitChanged(r.Context(), ckeys, known, timeout)
	var sb strings.Builder
	for _, k := range changed {
		sb.WriteString(k.DataId)
		sb.WriteString("\x02")
		sb.WriteString(k.Group)
		sb.WriteString("\x02")
		sb.WriteString(k.Tenant)
		sb.WriteString("\x01")
	}
	writeText(w, http.StatusOK, sb.String())
}

func (s *Server) configV1HistoryList(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageNo, _ := strconv.Atoi(qp.Get("pageNo"))
	pageSize, _ := strconv.Atoi(qp.Get("pageSize"))
	items, total, err := s.ST.ListHistory(r.Context(), storage.HistoryListQuery{
		TenantId: qp.Get("tenant"), DataId: qp.Get("dataId"), GroupId: qp.Get("group"),
		Accurate: qp.Get("search") == "accurate", PageNo: pageNo, PageSize: pageSize,
	})
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
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
	writeJSON(w, http.StatusOK, newPage(total, pageNo, pageSize, resp))
}

func (s *Server) configV1HistoryDetail(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	nid, _ := strconv.ParseInt(qp.Get("nid"), 10, 64)
	h, err := s.ST.GetHistory(r.Context(), nid)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if h == nil {
		v1Fail(w, http.StatusNotFound, 404, "history not exist")
		return
	}
	writeJSON(w, http.StatusOK, respFromHistory(h, true))
}

// configV1HistoryPrevious returns the version before nid (console revert).
func (s *Server) configV1HistoryPrevious(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	nid, _ := strconv.ParseInt(qp.Get("id"), 10, 64)
	h, err := s.ST.GetPreviousHistory(r.Context(), qp.Get("dataId"), qp.Get("group"), qp.Get("tenant"), nid)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if h == nil {
		v1Fail(w, http.StatusNotFound, 404, "previous history not exist")
		return
	}
	writeJSON(w, http.StatusOK, respFromHistory(h, true))
}

func md5Of(s string) string {
	// local md5 helper (storage keeps its own)
	sum := md5Sum([]byte(s))
	return hexOf(sum)
}
