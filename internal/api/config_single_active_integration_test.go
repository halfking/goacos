//go:build integration

package api

import (
	"context"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
	"testing"
	"time"
)

func TestSingleActiveRejectsStaleSecondHub(t *testing.T) {
	a, ns, _ := freshnessServer(t)
	b, _, _ := freshnessServer(t)
	key := core.ConfigKey{Tenant: ns, Group: "DEFAULT_GROUP", DataId: "single-active.yaml"}
	ha, e := core.NewConfigHub(t.Context(), a.ST)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = a.ST.UpsertConfig(t.Context(), storage.ConfigUpsert{TenantId: ns, GroupId: key.Group, DataId: key.DataId, Content: "before"})
	if e != nil {
		t.Fatal(e)
	}
	ha.Publish(key, md5Of("before"))
	hb, e := core.NewConfigHub(t.Context(), b.ST)
	if e != nil {
		t.Log("database refused second active hub")
		return
	}
	// Baseline admits both hubs; only the publishing hub changes its index.
	_, _, e = a.ST.UpsertConfig(t.Context(), storage.ConfigUpsert{TenantId: ns, GroupId: key.Group, DataId: key.DataId, Content: "after"})
	if e != nil {
		t.Fatal(e)
	}
	ha.Publish(key, md5Of("after"))
	changed, _ := hb.WaitChanged(context.Background(), []core.ConfigKey{key}, map[core.ConfigKey]string{key: md5Of("before")}, 1200*time.Millisecond)
	t.Logf("actual second-hub stale listener changed=%v while database content=after", changed)
	t.Fatal("two independently preloaded hubs both activated; stale second listener must be refused by database ownership")
}
