package api

import (
	"reflect"
	"testing"
)

// A malformed filter pair is dropped, not fatal: a partially applied filter is
// still a well-defined narrower answer, whereas an error would take a live
// route offline.
func TestParseMetaFilter(t *testing.T) {
	if got := parseMetaFilter(""); got != nil {
		t.Errorf("empty should be nil, got %v", got)
	}
	if got := parseMetaFilter("garbage"); got != nil {
		t.Errorf("no '=' should yield nil, got %v", got)
	}
	got := parseMetaFilter("lane=canary,zone=a")
	want := map[string]string{"lane": "canary", "zone": "a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got = parseMetaFilter(" lane = canary ,, bad , version=1.2.3 ")
	want = map[string]string{"lane": "canary", "version": "1.2.3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tolerant parse wrong: got %v want %v", got, want)
	}
	if got := parseMetaFilter("empty="); got == nil || got["empty"] != "" {
		t.Errorf("empty value should be kept as an explicit filter: %v", got)
	}
}

func TestDefaultNamespace(t *testing.T) {
	if got := defaultNamespace(""); got != "public" {
		t.Errorf("empty namespace should normalise to public, got %q", got)
	}
	if got := defaultNamespace("itestu"); got != "itestu" {
		t.Errorf("explicit namespace must be preserved, got %q", got)
	}
}
