package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCasdoorScopeAllowed(t *testing.T) {
	for _, tc := range []struct{ method, path, query, body, resource, action string }{
		{"GET", "/nacos/v1/cs/configs", "tenant=ns&group=g&dataId=d", "", "goacos/config/ns/g/d", "read"},
		{"DELETE", "/nacos/v1/cs/configs", "tenant=ns&group=g&dataId=d", "", "goacos/config/ns/g/d", "write"},
		{"POST", "/nacos/v1/cs/configs", "", "tenant=ns&group=g&dataId=d&content=a%2Bb%26c", "goacos/config/ns/g/d", "write"},
		{"GET", "/nacos/v1/ns/instance/list", "namespaceId=ns&groupName=g&serviceName=s", "", "goacos/service/ns/g/s", "read"},
		{"POST", "/nacos/v1/ns/instance", "namespaceId=ns&groupName=g&serviceName=s&ip=127.0.0.1&port=80", "", "goacos/service/ns/g/s", "write"},
		{"DELETE", "/nacos/v1/ns/instance", "namespaceId=ns&groupName=g&serviceName=s", "", "goacos/service/ns/g/s", "write"},
		{"PUT", "/nacos/v1/ns/instance/beat", "namespaceId=ns&groupName=g&serviceName=s&beat=" + url.QueryEscape(`{"serviceName":"other"}`), "", "goacos/service/ns/g/s", "write"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path+"?"+tc.query, strings.NewReader(tc.body))
			if tc.body != "" {
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			got, action, err := casdoorScope(r)
			if err != nil || got != tc.resource || action != tc.action {
				t.Fatalf("scope=%q %q err=%v", got, action, err)
			}
			if strings.Contains(tc.path, "/ns/") {
				p := parseInstanceParams(r)
				if got != "goacos/service/"+p.NamespaceId+"/"+p.GroupName+"/"+p.ServiceName {
					t.Fatal("handler scope differs")
				}
				b, _ := io.ReadAll(r.Body)
				if len(b) != 0 {
					t.Fatal("beat handler must see empty body")
				}
			} else if tc.method == "POST" {
				p := formOrJSON(r)
				if got != "goacos/config/"+p["tenant"]+"/"+p["group"]+"/"+p["dataId"] || p["content"] != "a+b&c" {
					t.Fatal("handler scope/content differs")
				}
				b, _ := io.ReadAll(r.Body)
				if string(b) != tc.body {
					t.Fatal("body changed")
				}
			}
		})
	}
}

func TestCasdoorScopeDenied(t *testing.T) {
	const config = "/nacos/v1/cs/configs?tenant=ns&group=g&dataId=d"
	const naming = "/nacos/v1/ns/instance?namespaceId=ns&groupName=g&serviceName=s"
	for _, tc := range []struct{ name, method, target, body, ct string }{
		{"head", "HEAD", config, "", ""}, {"list", "GET", config + "&search=", "", ""}, {"page", "GET", config + "&pageNo=", "", ""},
		{"duplicate", "GET", config + "&tenant=ns", "", ""}, {"malformed", "GET", config + "&x=%GG", "", ""},
		{"mixed", "POST", config, "tenant=ns", "application/x-www-form-urlencoded"},
		{"split scope", "POST", "/nacos/v1/cs/configs?tenant=ns", "group=g&dataId=d", "application/x-www-form-urlencoded"},
		{"duplicate body", "POST", "/nacos/v1/cs/configs", "tenant=ns&group=g&dataId=d&dataId=d", "application/x-www-form-urlencoded"},
		{"malformed body", "POST", "/nacos/v1/cs/configs", "tenant=ns&group=g&dataId=d&content=%GG", "application/x-www-form-urlencoded"},
		{"json", "POST", config, `{"tenant":"other"}`, "application/json"},
		{"alias", "POST", naming + "&tenant=ns", "", ""}, {"mixed scope", "POST", naming + "&dataId=d", "", ""},
		{"body", "POST", naming, "serviceName=other", "application/x-www-form-urlencoded"},
		{"embedded group", "POST", strings.Replace(naming, "serviceName=s", "serviceName=g@@s", 1), "", ""},
		{"default", "GET", "/nacos/v1/cs/configs?group=g&dataId=d", "", ""},
		{"slash", "GET", strings.Replace(config, "tenant=ns", "tenant=a%2Fb", 1), "", ""},
		{"encoded path", "GET", strings.Replace(config, "configs", "%63onfigs", 1), "", ""},
		{"listener", "POST", "/nacos/v1/cs/configs/listener", "", ""},
		{"history", "GET", "/nacos/v1/cs/history", "", ""},
		{"v2", "GET", "/nacos/v2/cs/config", "", ""},
		{"admin", "GET", "/nacos/v1/auth/users", "", ""},
		{"large", "POST", config, strings.Repeat("x", (1<<20)+1), "application/x-www-form-urlencoded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.ct)
			if res, act, err := casdoorScope(r); err == nil || res != "" || act != "" {
				t.Fatalf("allowed %q %q %v", res, act, err)
			}
		})
	}
	for _, value := range []string{"", ".", "..", "*", "x y", "中文", strings.Repeat("x", 256)} {
		r := httptest.NewRequest("GET", "/nacos/v1/cs/configs?tenant="+url.QueryEscape(value)+"&group=g&dataId=d", nil)
		if _, _, err := casdoorScope(r); err == nil {
			t.Fatalf("allowed component %q", value)
		}
	}
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.ContentLength = -1 }, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("hidden")) }, func(r *http.Request) { r.Form = url.Values{"tenant": {"other"}} }, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }} {
		r := httptest.NewRequest("GET", config, nil)
		mutate(r)
		if _, _, err := casdoorScope(r); err == nil {
			t.Fatal("allowed uncertain request")
		}
	}
}
