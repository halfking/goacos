package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/config"
)

// This driver rejects local identity access in federated mode. It requires no
// external database and records the namespace bootstrap independently.
type seedProbe struct {
	forbidAuth bool
	users      int64
	namespaces int64
	authReads  int
	authWrites int
	nsReads    int
	nsWrites   int
}

type seedConnector struct{ probe *seedProbe }
type seedDriver struct{}
type seedConn struct{ probe *seedProbe }
type seedRows struct {
	columns []string
	values  [][]driver.Value
}

func (c seedConnector) Connect(context.Context) (driver.Conn, error) { return seedConn{c.probe}, nil }
func (seedConnector) Driver() driver.Driver                          { return seedDriver{} }
func (seedDriver) Open(string) (driver.Conn, error)                  { return nil, errors.New("use connector") }
func (seedConn) Prepare(string) (driver.Stmt, error)                 { return nil, errors.New("unexpected prepare") }
func (seedConn) Close() error                                        { return nil }
func (seedConn) Begin() (driver.Tx, error)                           { return nil, errors.New("unexpected transaction") }
func (r *seedRows) Columns() []string                                { return r.columns }
func (r *seedRows) Close() error                                     { return nil }
func (r *seedRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func (c seedConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "tenant_info") {
		c.probe.nsReads++
		return &seedRows{[]string{"count"}, [][]driver.Value{{c.probe.namespaces}}}, nil
	}
	c.probe.authReads++
	if c.probe.forbidAuth {
		return nil, errors.New("local auth query forbidden")
	}
	if query == "SELECT COUNT(*) FROM users" {
		return &seedRows{[]string{"count"}, [][]driver.Value{{c.probe.users}}}, nil
	}
	if strings.Contains(query, "SELECT DISTINCT u.password") {
		return &seedRows{columns: []string{"password"}}, nil
	}
	return nil, errors.New("unexpected query")
}

func (c seedConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "tenant_info") {
		c.probe.nsWrites++
	} else {
		c.probe.authWrites++
		if c.probe.forbidAuth {
			return nil, errors.New("local auth write forbidden")
		}
	}
	return driver.RowsAffected(1), nil
}

func seedTestStore(t *testing.T, provider string, probe *seedProbe) *Store {
	t.Helper()
	db := sql.OpenDB(seedConnector{probe})
	t.Cleanup(func() { _ = db.Close() })
	return &Store{DB: db, dialect: DialectPostgres, cfg: &config.Config{
		AuthEnabled: true, AuthProvider: provider,
		AdminUsername: "fixture-admin", AdminPassword: "private-fixture-password",
	}}
}

func TestCasdoorSeedDoesNotTouchLocalIdentity(t *testing.T) {
	for _, namespaces := range []int64{0, 1} {
		probe := &seedProbe{forbidAuth: true, namespaces: namespaces}
		store := seedTestStore(t, "casdoor", probe)
		if err := store.Seed(context.Background()); err != nil {
			t.Fatalf("Casdoor seed: %v", err)
		}
		if probe.authReads != 0 || probe.authWrites != 0 || probe.nsReads != 1 || probe.nsWrites != int(1-namespaces) {
			t.Fatalf("unexpected seed statements: %+v", probe)
		}
	}
}

func TestCasdoorValidateAuthStateDoesNotQueryLocalIdentity(t *testing.T) {
	probe := &seedProbe{forbidAuth: true}
	store := seedTestStore(t, "casdoor", probe)
	if err := store.ValidateAuthState(context.Background()); err != nil {
		t.Fatalf("Casdoor auth state: %v", err)
	}
	if probe.authReads != 0 || probe.authWrites != 0 || probe.nsReads != 0 {
		t.Fatalf("unexpected validation statements: %+v", probe)
	}
}

func TestLocalSeedStillBootstrapsAndValidatesIdentity(t *testing.T) {
	for _, provider := range []string{"", "local"} {
		for _, users := range []int64{0, 1} {
			probe := &seedProbe{users: users, namespaces: 1}
			store := seedTestStore(t, provider, probe)
			if err := store.Seed(context.Background()); err != nil {
				t.Fatalf("local seed: %v", err)
			}
			if probe.authReads != 2 || probe.authWrites != int(3*(1-users)) || probe.nsReads != 1 || probe.nsWrites != 0 {
				t.Fatalf("provider %q changed local behavior: %+v", provider, probe)
			}
		}
	}
}
