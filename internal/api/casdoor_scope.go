package api

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const casdoorMaxFormBytes = 1 << 20

// casdoorScope accepts only request shapes whose scope agrees with the v1
// handlers. Keep this allowlist narrow when adding routes or parser features.
func casdoorScope(r *http.Request) (resource, action string, err error) {
	deny := func() (string, string, error) { return "", "", errors.New("unsupported or ambiguous Casdoor scope") }
	if r.URL == nil || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || r.Form != nil || r.PostForm != nil || r.MultipartForm != nil {
		return deny()
	}
	config := r.URL.Path == "/nacos/v1/cs/configs"
	switch {
	case config && (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete):
	case r.URL.Path == "/nacos/v1/ns/instance/list" && r.Method == http.MethodGet:
	case r.URL.Path == "/nacos/v1/ns/instance" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
	case r.URL.Path == "/nacos/v1/ns/instance/beat" && r.Method == http.MethodPut:
	default:
		return deny()
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || !casdoorSingleValues(q) {
		return deny()
	}
	publish := config && r.Method == http.MethodPost
	limit := int64(0)
	if publish {
		limit = casdoorMaxFormBytes
	}
	if r.ContentLength < 0 || r.ContentLength > limit || len(r.TransferEncoding) != 0 {
		return deny()
	}
	var body []byte
	if r.Body != nil {
		original := r.Body
		body, e = io.ReadAll(io.LimitReader(original, limit+1))
		_ = original.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if e != nil || int64(len(body)) != r.ContentLength || int64(len(body)) > limit {
			return deny()
		}
	} else if r.ContentLength != 0 {
		return deny()
	} else {
		r.Body = http.NoBody
	}
	post := url.Values{}
	if publish {
		ct, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || ct != "application/x-www-form-urlencoded" {
			return deny()
		}
		post, e = url.ParseQuery(string(body))
		if e != nil || !casdoorSingleValues(post) {
			return deny()
		}
	}
	merged := url.Values{}
	for key, values := range q {
		merged[key] = append([]string(nil), values...)
	}
	for key, values := range post {
		if _, exists := merged[key]; exists {
			return deny()
		}
		merged[key] = append([]string(nil), values...)
	}
	keys := []string{"namespaceId", "groupName", "serviceName"}
	forbidden := []string{"namespace_id", "tenant", "group", "dataId"}
	kind := "service"
	if config {
		keys = []string{"tenant", "group", "dataId"}
		forbidden = []string{"namespaceId", "namespace_id", "groupName", "serviceName"}
		kind = "config"
		if r.Method == http.MethodGet && (q.Has("search") || q.Has("pageNo")) {
			return deny()
		}
	}
	for _, key := range forbidden {
		if merged.Has(key) {
			return deny()
		}
	}
	components := make([]string, 0, 3)
	scopeInQuery, scopeInBody := false, false
	for _, key := range keys {
		value := merged.Get(key)
		if !casdoorScopeComponent(value) {
			return deny()
		}
		components = append(components, value)
		scopeInQuery = scopeInQuery || q.Has(key)
		scopeInBody = scopeInBody || post.Has(key)
	}
	if scopeInQuery && scopeInBody {
		return deny()
	}
	// Prepopulate both forms from our single strict parse. formOrJSON's ParseForm
	// becomes a no-op, and the original bounded body remains available unchanged.
	r.Form = merged
	r.PostForm = post
	action = "write"
	if r.Method == http.MethodGet {
		action = "read"
	}
	return "goacos/" + kind + "/" + strings.Join(components, "/"), action, nil
}

func casdoorSingleValues(values url.Values) bool {
	for key, vs := range values {
		if key == "" || len(vs) != 1 {
			return false
		}
	}
	return true
}

func casdoorScopeComponent(s string) bool {
	if len(s) == 0 || len(s) > 255 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
