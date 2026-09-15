package api

import (
	"testing"

	"github.com/halfking/goacos/internal/core"
)

func TestParseV1ListeningConfigs(t *testing.T) {
	raw := "app.yaml\x02DEFAULT_GROUP\x02abc123\x02\x01svc.json\x02MY_GROUP\x02def456\x02tenant1\x01"
	keys := parseV1ListeningConfigs(raw)
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
	if keys[0].Key.DataId != "app.yaml" || keys[0].Key.Group != "DEFAULT_GROUP" || keys[0].Key.Tenant != "" || keys[0].Md5 != "abc123" {
		t.Errorf("key0 mismatch: %+v", keys[0])
	}
	if keys[1].Key.Tenant != "tenant1" || keys[1].Md5 != "def456" {
		t.Errorf("key1 mismatch: %+v", keys[1])
	}
	if got := parseV1ListeningConfigs(""); len(got) != 0 {
		t.Errorf("empty payload should yield no keys, got %d", len(got))
	}
}

func TestParseServiceName(t *testing.T) {
	if g, n := parseServiceName("MY_GROUP@@order", ""); g != "MY_GROUP" || n != "order" {
		t.Errorf("group@@name split failed: %s %s", g, n)
	}
	if g, n := parseServiceName("order", "OTHER"); g != "OTHER" || n != "order" {
		t.Errorf("explicit group failed: %s %s", g, n)
	}
	if g, _ := parseServiceName("order", ""); g != "DEFAULT_GROUP" {
		t.Errorf("default group failed: %s", g)
	}
}

func TestConfigType(t *testing.T) {
	cases := map[string]string{
		"app.yaml": "yaml", "a.yml": "yaml", "a.json": "json", "a.properties": "properties",
		"a.xml": "xml", "a.txt": "text", "noext": "text",
	}
	for in, want := range cases {
		if got := configType(in, ""); got != want {
			t.Errorf("configType(%q)=%q want %q", in, got, want)
		}
	}
	if got := configType("whatever", "json"); got != "json" {
		t.Errorf("explicit type override failed: %q", got)
	}
}

func TestMetaFromParam(t *testing.T) {
	m := metaFromParam(`{"a":"1","b":"x"}`)
	if m["a"] != "1" || m["b"] != "x" {
		t.Errorf("metadata parse failed: %v", m)
	}
	if m := metaFromParam(""); len(m) != 0 {
		t.Errorf("empty metadata should be empty map")
	}
	if m := metaFromParam("not-json"); len(m) != 0 {
		t.Errorf("bad metadata should degrade to empty map")
	}
}

func TestConfigKeyEquality(t *testing.T) {
	a := core.ConfigKey{Tenant: "", Group: "G", DataId: "D"}
	b := core.ConfigKey{Tenant: "", Group: "G", DataId: "D"}
	if a != b {
		t.Error("identical keys should be equal map keys")
	}
}
