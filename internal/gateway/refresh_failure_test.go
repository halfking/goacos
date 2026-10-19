package gateway

// These tests inject SQL query/cursor errors through database/sql. They exercise
// real storage reads and Gateway.refresh, not a copied refresh implementation.
// The prior snapshot is a fixture: this is NOT a real registry-process kill or
// proof of database loading, late component/instance query failures, or TLS.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

var errRefreshRead = errors.New("injected refresh read failure")

type refreshConnector struct {
	queries       atomic.Int32
	failAt        int32
	cursorFailure bool
}

func (c *refreshConnector) Connect(context.Context) (driver.Conn, error) { return &refreshConn{c}, nil }
func (c *refreshConnector) Driver() driver.Driver                        { return refreshDriver{} }

type refreshDriver struct{}

func (refreshDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type refreshConn struct{ connector *refreshConnector }

func (*refreshConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (*refreshConn) Close() error              { return nil }
func (*refreshConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected Begin") }
func (c *refreshConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	query := c.connector.queries.Add(1)
	if query == c.connector.failAt && !c.connector.cursorFailure {
		return nil, errRefreshRead
	}
	return &refreshRows{fail: query == c.connector.failAt}, nil
}

type refreshRows struct{ fail bool }

func (*refreshRows) Columns() []string { return []string{"empty_result"} }
func (*refreshRows) Close() error      { return nil }
func (r *refreshRows) Next([]driver.Value) error {
	if r.fail {
		return errRefreshRead
	}
	return io.EOF
}

func refreshFixture(t *testing.T, connector *refreshConnector) (*Gateway, func(string)) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served-from-prior-snapshot"))
	}))
	t.Cleanup(upstream.Close)
	host, port := splitHostPort(t, upstream.URL)
	table := buildTable(testRoute{domain: "refresh.example.com", app: "refresh", component: "api",
		service: "refresh-svc", lb: "least_conn", instances: []Instance{instanceOf(host, port, 1, nil)}})
	table.LoadedAt = time.Unix(100, 0)
	g := testGateway(table, nil)
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	g.st = &storage.Store{DB: db}
	g.ns = "public"
	t.Cleanup(g.pool.closeAll)
	request := func(label string) {
		t.Helper()
		rec := httptest.NewRecorder()
		g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://refresh.example.com/", nil), false)
		if rec.Code != 200 || rec.Body.String() != "served-from-prior-snapshot" {
			t.Fatalf("%s: status=%d body=%q", label, rec.Code, rec.Body.String())
		}
	}
	return g, request
}

func TestRefreshReadFailurePreservesSnapshotAndTraffic(t *testing.T) {
	for _, cursor := range []bool{false, true} {
		for stage := int32(1); stage <= 3; stage++ {
			name := []string{"routes", "applications", "vhosts"}[stage-1]
			if cursor {
				name += "-cursor"
			}
			t.Run(name, func(t *testing.T) {
				connector := &refreshConnector{failAt: stage, cursorFailure: cursor}
				g, request := refreshFixture(t, connector)
				request("baseline")
				before := g.table.Lookup("refresh.example.com", "/", nil)
				key, address := targetKey(before), addr(before.Instances[0])
				g.picker.Acquire(key, address)
				defer g.picker.Release(key, address)
				g.pool.mu.RLock()
				poolSize := len(g.pool.entries)
				original := make(map[string]proxyEntry, poolSize)
				originalPointers := make(map[string]*proxyEntry, poolSize)
				for k, entry := range g.pool.entries {
					original[k] = *entry
					originalPointers[k] = entry
				}
				g.pool.mu.RUnlock()
				for iteration := 0; iteration < 3; iteration++ {
					connector.queries.Store(0)
					g.refresh(context.Background())
					if got := connector.queries.Load(); got != stage {
						t.Fatalf("failure injection not reached: queries=%d want=%d", got, stage)
					}
					if !g.table.LoadedAt.Equal(time.Unix(100, 0)) {
						t.Fatal("failed refresh replaced prior snapshot timestamp")
					}
					g.pool.mu.RLock()
					gotSize := len(g.pool.entries)
					identityPreserved := true
					for k, prior := range original {
						entry := g.pool.entries[k]
						if entry == nil || entry != originalPointers[k] || entry.transport != prior.transport || entry.reverse != prior.reverse {
							identityPreserved = false
						}
					}
					g.pool.mu.RUnlock()
					if gotSize != poolSize || poolSize == 0 {
						t.Fatalf("failed refresh pruned transport pool: before=%d after=%d", poolSize, gotSize)
					}
					if !identityPreserved {
						t.Fatal("failed refresh replaced transport pool identity")
					}
					g.picker.mu.Lock()
					state := g.picker.stats[key]
					count := 0
					if state != nil {
						count = state.inflight[address]
					}
					g.picker.mu.Unlock()
					if count != 1 {
						t.Fatalf("failed refresh forgot inflight: got=%d want=1", count)
					}
					request("after failed refresh")
				}
			})
		}
	}
}

func TestRefreshSuccessfulEmptyResultRemovesPriorRoutes(t *testing.T) {
	connector := &refreshConnector{}
	g, request := refreshFixture(t, connector)
	request("baseline")
	g.refresh(context.Background())
	if got := connector.queries.Load(); got != 3 {
		t.Fatalf("incomplete successful read: queries=%d", got)
	}
	if g.table.Lookup("refresh.example.com", "/", nil) != nil {
		t.Fatal("successful empty result retained deleted route")
	}
	rec := httptest.NewRecorder()
	g.dispatch(rec, httptest.NewRequest(http.MethodGet, "http://refresh.example.com/", nil), false)
	if rec.Code != 404 {
		t.Fatalf("deleted route: status=%d body=%q", rec.Code, rec.Body.String())
	}
}
