//go:build integration

package storage_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

func TestSingleActiveMySQLActualDialect(t *testing.T) {
	dsn := os.Getenv("GOACOS_TEST_MYSQL_DSN")
	d, e := mysql.ParseDSN(dsn)
	if e != nil || d == nil || !strings.HasPrefix(d.DBName, "w038_") || !strings.HasPrefix(d.Addr, "127.0.0.1:") || len(d.Passwd) < 12 {
		t.Fatal("explicit owned loopback w038_ MySQL fixture required")
	}
	cfg := &config.Config{DBType: "mysql", DBDSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2, AuthEnabled: true, AdminUsername: "w038-test-admin", AdminPassword: d.Passwd}
	open := func() *storage.Store {
		s, e := storage.Open(t.Context(), cfg)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b := open(), open()
	ha, e := core.NewConfigHub(t.Context(), a)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = core.NewConfigHub(t.Context(), b); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("MySQL second owner=%v", e)
	}
	ns := storage.NewID()
	k := core.ConfigKey{DataId: "mysql-loss", Group: "g", Tenant: ns}
	u := storage.ConfigUpsert{DataId: k.DataId, GroupId: k.Group, TenantId: ns, Content: "committed"}
	id, _, e := a.UpsertConfig(t.Context(), u)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.ReplaceConfigTags(t.Context(), id, k.DataId, k.Group, ns, []string{"mysql-tag"}); e != nil {
		t.Fatal(e)
	}
	if _, n, e := a.ListConfigs(t.Context(), storage.ListQuery{TenantId: ns, Tag: "mysql-tag"}); e != nil || n != 1 {
		t.Fatalf("MySQL tagged listing count=%d err=%v", n, e)
	}
	var nonTransactional int
	if e = a.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('config_info','his_config_info','config_tags_relation') AND ENGINE<>'InnoDB'`).Scan(&nonTransactional); e != nil || nonTransactional != 0 {
		t.Fatalf("MySQL schema does not preserve transactional config/history: count=%d err=%v", nonTransactional, e)
	}
	var pid int
	e = a.WithConfigTransaction(t.Context(), func(ctx context.Context) error { return a.QueryRow(ctx, "SELECT CONNECTION_ID()").Scan(&pid) })
	if e != nil {
		t.Fatal(e)
	}
	written, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithConfigTransaction(t.Context(), func(ctx context.Context) error {
			u.Content = "must-rollback"
			if _, _, e := a.UpsertConfig(ctx, u); e != nil {
				return e
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("MySQL write not reached")
	}
	if _, e = a.DB.ExecContext(t.Context(), "KILL CONNECTION "+strconv.Itoa(pid)); e != nil {
		t.Fatal(e)
	}
	hb, e := core.NewConfigHub(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	close(release)
	select {
	case e = <-done:
		if !errors.Is(e, storage.ErrConfigInactive) {
			t.Fatalf("old MySQL transaction=%v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MySQL old write blocked")
	}
	c, e := b.GetConfig(t.Context(), k.DataId, k.Group, ns)
	if e != nil || c == nil || c.Content != "committed" {
		t.Fatalf("MySQL aborted write survived: %v %v", c, e)
	}
	if _, n, e := b.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: k.DataId, GroupId: k.Group}); e != nil || n != 1 {
		t.Fatalf("MySQL history rollback=%d %v", n, e)
	}
	if _, e = ha.Md5(t.Context(), k); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("old MySQL read=%v", e)
	}
	if _, e = ha.WaitChanged(t.Context(), []core.ConfigKey{k}, nil, time.Millisecond); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("old MySQL listener=%v", e)
	}
	if _, e = hb.Md5(t.Context(), k); e != nil {
		t.Fatal(e)
	}
	if ok, e := b.DeleteConfig(t.Context(), k.DataId, k.Group, ns, "", ""); e != nil || !ok {
		t.Fatalf("MySQL delete=%v %v", ok, e)
	}
	if _, n, e := b.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: k.DataId, GroupId: k.Group}); e != nil || n != 2 {
		t.Fatalf("MySQL config/history=%d %v", n, e)
	}
	if e = b.ReleaseConfigOwnership(); e != nil {
		t.Fatal(e)
	}
	cstore := open()
	if _, e = core.NewConfigHub(t.Context(), cstore); e != nil {
		t.Fatalf("explicit MySQL session release did not release named lock: %v", e)
	}
	if e = cstore.CheckConfigOwnership(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Log("actual MySQL8.4 GET_LOCK same-connection transactions, KILL rollback, takeover and physical session release verified")
}

func TestSingleActiveMySQLNonTransactionalHistoryRefused(t *testing.T) {
	dsn := os.Getenv("GOACOS_TEST_MYSQL_DSN")
	d, e := mysql.ParseDSN(dsn)
	if e != nil || d == nil || !strings.HasPrefix(d.DBName, "w038_") || !strings.HasPrefix(d.Addr, "127.0.0.1:") {
		t.Fatal("owned MySQL fixture required")
	}
	cfg := &config.Config{DBType: "mysql", DBDSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2, AuthEnabled: true, AdminUsername: "w038-test-admin", AdminPassword: d.Passwd}
	s, e := storage.Open(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.DB.ExecContext(t.Context(), "ALTER TABLE his_config_info ENGINE=MyISAM"); e != nil {
		t.Fatal(e)
	}
	defer s.DB.ExecContext(context.Background(), "ALTER TABLE his_config_info ENGINE=InnoDB")
	if _, e = core.NewConfigHub(t.Context(), s); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("nontransactional history admitted active ownership: %v", e)
	}
	t.Log("legacy MyISAM history must fail closed instead of pretending rollback is atomic")
}
