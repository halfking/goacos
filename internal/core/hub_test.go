package core

import (
	"context"
	"testing"
	"time"
)

func TestConfigHubWaitChanged(t *testing.T) {
	h := &ConfigHub{
		md5s:     map[ConfigKey]string{},
		watchers: map[ConfigKey]map[chan struct{}]struct{}{},
	}
	k := ConfigKey{Tenant: "", Group: "G", DataId: "D"}
	h.md5s[k] = "md5-1"

	// same md5 -> no change within short timeout
	got := h.WaitChanged(context.Background(), []ConfigKey{k}, map[ConfigKey]string{k: "md5-1"}, 300*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("expected no change, got %v", got)
	}

	// publish triggers immediate wake
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.Publish(k, "md5-2")
	}()
	start := time.Now()
	got = h.WaitChanged(context.Background(), []ConfigKey{k}, map[ConfigKey]string{k: "md5-1"}, 5*time.Second)
	if len(got) != 1 || got[0] != k {
		t.Fatalf("expected change on k, got %v", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("publish notification too slow: %v", elapsed)
	}

	// deletion => md5 "" counts as change
	got = h.WaitChanged(context.Background(), []ConfigKey{k}, map[ConfigKey]string{k: "md5-2"}, 300*time.Millisecond)
	_ = got
	h.Invalidate(k)
	if h.Md5(k) != "" {
		t.Fatal("invalidate should clear md5")
	}
	got = h.WaitChanged(context.Background(), []ConfigKey{k}, map[ConfigKey]string{k: "md5-2"}, 300*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("deletion should count as change, got %v", got)
	}
}

func TestConfigHubMultiKey(t *testing.T) {
	h := &ConfigHub{
		md5s:     map[ConfigKey]string{},
		watchers: map[ConfigKey]map[chan struct{}]struct{}{},
	}
	k1 := ConfigKey{Group: "G", DataId: "one"}
	k2 := ConfigKey{Group: "G", DataId: "two"}
	h.md5s[k1] = "a"
	h.md5s[k2] = "b"
	known := map[ConfigKey]string{k1: "stale", k2: "b"}
	got := h.WaitChanged(context.Background(), []ConfigKey{k1, k2}, known, 200*time.Millisecond)
	if len(got) != 1 || got[0] != k1 {
		t.Fatalf("expected only k1 changed, got %v", got)
	}
}
