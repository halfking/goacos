// Package api implements the Nacos-compatible HTTP surface of goacos.
package api

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

// Server wires configuration, storage and core services into the HTTP API.
type Server struct {
	Cfg     *config.Config
	ST      *storage.Store
	Hub     *core.ConfigHub
	Auth    *core.AuthService
	Started time.Time
}

// ---------- response helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonDecode decodes a JSON request body into v.
func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeText(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(s))
}

// V2Result is the Nacos v2 response envelope.
type V2Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func v2OK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, V2Result{Code: 0, Message: "success", Data: data})
}

func v2Fail(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, V2Result{Code: code, Message: msg})
}

// V1Result is the legacy Nacos v1 RestResult envelope (code 200 = success).
type V1Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func v1OK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, V1Result{Code: 200, Message: "ok", Data: data})
}

func v1Fail(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, V1Result{Code: code, Message: msg})
}

// ---------- request helpers ----------

func q(r *http.Request, key string) string { return r.URL.Query().Get(key) }

// formOrJSON merges query/form params with a JSON body (JSON wins).
func formOrJSON(r *http.Request) map[string]string {
	out := map[string]string{}
	_ = r.ParseForm()
	for k, vs := range r.Form {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var m map[string]any
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&m); err == nil {
			for k, v := range m {
				switch tv := v.(type) {
				case string:
					out[k] = tv
				case float64:
					out[k] = trimFloat(tv)
				case bool:
					out[k] = fmt.Sprintf("%t", tv)
				default:
					b, _ := json.Marshal(v)
					out[k] = string(b)
				}
			}
		}
	}
	return out
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// metaFromParam parses a metadata JSON object string (never nil).
func metaFromParam(s string) map[string]string {
	m := map[string]string{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

// mapToJSONString serializes a metadata map to its JSON form.
func mapToJSONString(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func srcIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// parseServiceName splits "group@@name" style service names.
func parseServiceName(serviceName, groupName string) (string, string) {
	if i := strings.Index(serviceName, "@@"); i >= 0 {
		return serviceName[:i], serviceName[i+2:]
	}
	if groupName == "" {
		groupName = "DEFAULT_GROUP"
	}
	return groupName, serviceName
}

func configType(dataId, explicit string) string {
	if explicit != "" {
		return explicit
	}
	switch {
	case strings.HasSuffix(dataId, ".yaml"), strings.HasSuffix(dataId, ".yml"):
		return "yaml"
	case strings.HasSuffix(dataId, ".json"):
		return "json"
	case strings.HasSuffix(dataId, ".properties"):
		return "properties"
	case strings.HasSuffix(dataId, ".xml"):
		return "xml"
	case strings.HasSuffix(dataId, ".html"), strings.HasSuffix(dataId, ".htm"):
		return "html"
	default:
		return "text"
	}
}

// ---------- DTOs ----------

// Page is the Nacos page envelope.
type Page[T any] struct {
	TotalCount     int64 `json:"totalCount"`
	PageNumber     int   `json:"pageNumber"`
	PagesAvailable int64 `json:"pagesAvailable"`
	PageItems      []T   `json:"pageItems"`
}

func newPage[T any](total int64, pageNo, pageSize int, items []T) Page[T] {
	pages := (total + int64(pageSize) - 1) / int64(pageSize)
	if pages < 1 {
		pages = 1
	}
	if items == nil {
		items = []T{}
	}
	return Page[T]{TotalCount: total, PageNumber: pageNo, PagesAvailable: pages, PageItems: items}
}

// ConfigResp is the config JSON shape used by console and v2 clients.
type ConfigResp struct {
	ID          int64  `json:"id"`
	DataId      string `json:"dataId"`
	Group       string `json:"group"`
	NamespaceId string `json:"namespaceId"`
	TenantId    string `json:"tenantId"`
	Content     string `json:"content"`
	Md5         string `json:"md5"`
	Type        string `json:"type"`
	AppName     string `json:"appName"`
	CDesc       string `json:"cDesc"`
	SrcUser     string `json:"srcUser"`
	CreateUser  string `json:"createUser"`
	GmtCreate   int64  `json:"gmtCreate"`
	GmtModified int64  `json:"gmtModified"`
	CreateDate  string `json:"createDate"`
	ModifyDate  string `json:"modifyDate"`
}

func respFromConfig(c *storage.ConfigInfo) ConfigResp {
	return ConfigResp{
		ID: c.ID, DataId: c.DataId, Group: c.GroupId, NamespaceId: c.TenantId, TenantId: c.TenantId,
		Content: c.Content, Md5: c.Md5, Type: c.Type, AppName: c.AppName, CDesc: c.CDesc,
		SrcUser: c.SrcUser, CreateUser: c.SrcUser,
		GmtCreate: c.GmtCreate.UnixMilli(), GmtModified: c.GmtModified.UnixMilli(),
		CreateDate: c.GmtCreate.Format("2006-01-02 15:04:05"), ModifyDate: c.GmtModified.Format("2006-01-02 15:04:05"),
	}
}

// HistoryResp is the config history JSON shape.
type HistoryResp struct {
	ID               int64   `json:"id"`
	ConfigID         int64   `json:"configId"`
	DataId           string  `json:"dataId"`
	Group            string  `json:"group"`
	TenantId         string  `json:"tenantId"`
	NamespaceId      string  `json:"namespaceId"`
	AppName          string  `json:"appName"`
	Content          *string `json:"content"`
	Md5              string  `json:"md5"`
	SrcUser          string  `json:"srcUser"`
	SrcIp            string  `json:"srcIp"`
	OpType           string  `json:"opType"`
	CreatedTime      string  `json:"createdTime"`
	LastModifiedTime string  `json:"lastModifiedTime"`
	GmtCreate        int64   `json:"gmtCreate"`
	GmtModified      int64   `json:"gmtModified"`
}

func respFromHistory(h *storage.HistoryInfo, withContent bool) HistoryResp {
	r := HistoryResp{
		ID: h.Nid, ConfigID: h.ID, DataId: h.DataId, Group: h.GroupId,
		TenantId: h.TenantId, NamespaceId: h.TenantId, AppName: h.AppName, Md5: h.Md5,
		SrcUser: h.SrcUser, SrcIp: h.SrcIp, OpType: h.OpType,
		CreatedTime:      h.GmtCreate.Format("2006-01-02 15:04:05"),
		LastModifiedTime: h.GmtModified.Format("2006-01-02 15:04:05"),
		GmtCreate:        h.GmtCreate.UnixMilli(), GmtModified: h.GmtModified.UnixMilli(),
	}
	if withContent {
		c := h.Content
		r.Content = &c
	}
	return r
}

// InstanceResp is the Nacos instance JSON shape.
type InstanceResp struct {
	InstanceId                string            `json:"instanceId"`
	IP                        string            `json:"ip"`
	Port                      uint32            `json:"port"`
	Weight                    float64           `json:"weight"`
	Healthy                   bool              `json:"healthy"`
	Enabled                   bool              `json:"enabled"`
	Ephemeral                 bool              `json:"ephemeral"`
	ClusterName               string            `json:"clusterName"`
	ServiceName               string            `json:"serviceName"`
	Metadata                  map[string]string `json:"metadata"`
	InstanceHeartBeatInterval int64             `json:"instanceHeartBeatInterval,omitempty"`
	InstanceHeartBeatTimeOut  int64             `json:"instanceHeartBeatTimeOut,omitempty"`
	InstanceIdGenerator       string            `json:"instanceIdGenerator,omitempty"`
}

func (s *Server) instanceResp(i *storage.Instance, group, name string, withHeartbeatMeta bool) InstanceResp {
	if i.ClusterName == "" {
		i.ClusterName = "DEFAULT"
	}
	r := InstanceResp{
		InstanceId:  fmt.Sprintf("%s#%d#%s#%s@@%s", i.IP, i.Port, i.ClusterName, group, name),
		IP:          i.IP,
		Port:        i.Port,
		Weight:      i.Weight,
		Healthy:     i.Healthy,
		Enabled:     i.Enabled,
		Ephemeral:   i.Ephemeral,
		ClusterName: i.ClusterName,
		ServiceName: group + "@@" + name,
		Metadata:    i.Metadata(),
	}
	if withHeartbeatMeta {
		r.InstanceHeartBeatInterval = s.Cfg.HeartbeatIntervalMs
		r.InstanceHeartBeatTimeOut = s.Cfg.HeartbeatTimeoutMs
		r.InstanceIdGenerator = "simple"
	}
	return r
}

// ServiceInfoResp is the Nacos ServiceInfo JSON shape (instance list).
type ServiceInfoResp struct {
	Name                     string            `json:"name"`
	Clusters                 string            `json:"clusters"`
	CacheMillis              int64             `json:"cacheMillis"`
	Hosts                    []InstanceResp    `json:"hosts"`
	Checksum                 string            `json:"checksum"`
	LastRefTime              int64             `json:"lastRefTime"`
	Env                      string            `json:"env"`
	Metadata                 map[string]string `json:"metadata"`
	GroupName                string            `json:"groupName"`
	Valid                    bool              `json:"valid"`
	AllIPs                   bool              `json:"allIPs"`
	ReachProtectionThreshold bool              `json:"reachProtectionThreshold"`
	Ok                       bool              `json:"ok"`
}

func (s *Server) serviceInfo(group, name, clusters string, items []storage.Instance) ServiceInfoResp {
	hosts := make([]InstanceResp, 0, len(items))
	for i := range items {
		hosts = append(hosts, s.instanceResp(&items[i], group, name, true))
	}
	sum := md5.Sum([]byte(fmt.Sprintf("%v", hosts)))
	return ServiceInfoResp{
		Name: group + "@@" + name, Clusters: clusters, CacheMillis: 1000,
		Hosts: hosts, Checksum: hex.EncodeToString(sum[:]), LastRefTime: time.Now().UnixMilli(),
		Metadata: map[string]string{}, GroupName: group, Valid: true, Ok: true,
	}
}
