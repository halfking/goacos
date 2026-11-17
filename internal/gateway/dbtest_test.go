package gateway

// The database fixture for the application-model runtime path.
//
// ## Why this file exists
//
// The seam between the registry and the data plane had a test file that
// **had never been executed**. It skipped unless a DSN was in the environment,
// and nobody had ever put one there — so the file was, in practice, a comment
// about what the code should do.
//
// What it said was worth having. `Table.Refresh` is the one function that
// reads instances out of goacos's naming storage; every other test in this
// package builds its routing table by hand and therefore bypasses it. The
// comment on that file was blunt about the consequence: "if its query were
// wrong … the gateway would resolve every domain to 'no healthy instance',
// and the suite would still be green".
//
// **The suite was green. The seam was never exercised.** When the DSN was
// finally supplied (2026-10-24), four of the five end-to-end tests failed —
// every route resolved to zero instances.
//
// ## Why the fixture was the thing that was broken
//
// The failure was not in the product. `effectiveInstanceHealth`
// (`internal/storage/namingstore.go`) re-derives an ephemeral instance's
// health at the read boundary:
//
//	if s.cfg == nil || s.cfg.HeartbeatTimeoutMs <= 0 || … { return false }
//
// The old fixture built `config.Config{DBType: "postgres", DBDSN: dsn}` —
// every other field zero, so `HeartbeatTimeoutMs` was **0**, and every
// ephemeral instance read back as unhealthy. Production is not affected:
// `config.Default()` sets `HEARTBEAT_TIMEOUT_MS=15000`
// (`internal/config/config.go:267`).
//
// So the fixture was asserting nothing, and it was asserting it *silently*:
// the file's own guard (`…IsNotHandedAWorkspaceTooSmallToBehave`) checked
// that a written row could be read back, but not that it read back as
// **healthy** — which is exactly where the breakage was.
//
// ## What this fixture does differently
//
//  1. it sets `HeartbeatTimeoutMs`, because without it every ephemeral
//     instance is unhealthy by construction;
//  2. it runs against **both** engines — MySQL is the engine 252 runs, and
//     the application-model runtime path had never been executed on it;
//  3. the guard now asserts health, not just row presence;
//  4. it owns a **database of its own**, because the configured DSN is shared
//     with `internal/storage` and `internal/api` and `go test` runs packages
//     in parallel — see ownDatabase.
//
// ## Running it
//
// Needs scratch databases (the tests truncate the tables they write):
//
//	GOACOS_TEST_PG_DSN='postgres://…@127.0.0.1:5432/goacostest?sslmode=disable' \
//	GOACOS_TEST_MYSQL_DSN='root:…@tcp(127.0.0.1:3306)/goacostest' \
//	go test ./internal/gateway/ -count=1 -v
//
// A dialect without a DSN is skipped, and the skip is visible in the output —
// which is the point: a test that quietly never runs is worse than no test,
// because it reads as coverage.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// testHeartbeatTimeoutMs mirrors the production default.
//
// The value only has to be positive and generous enough that an instance
// written a moment ago is still fresh. 15000 is `config.Default()`'s value
// (`HEARTBEAT_TIMEOUT_MS`), so the fixture exercises the same rule production
// does rather than a laxer one.
const testHeartbeatTimeoutMs = 15000

// appModelTables are the tables these tests write into.
var appModelTables = []string{
	"naming_instance",
	"naming_service",
	"route_binding",
	"gateway_vhost",
	"application_component",
	"application",
}

// openStore opens the scratch database for one dialect and clears the tables
// these tests write, so repeated runs do not inherit each other's rows.
//
// The schema and the migrations are applied by `storage.Open` itself — there is
// no separate "apply the schema first" step any more, and the old file's
// header said otherwise. That header was written when the migration runner
// did not exist yet, and it was one of the two reasons nobody ran these
// tests: it asked for more setup than it needed to.
//
// The database it opens is **this package's own** — see ownDatabase.
func openStore(t *testing.T, dialect string) *storage.Store {
	t.Helper()
	dsn := os.Getenv(dsnEnvFor(dialect))
	if dsn == "" {
		t.Skip(dsnEnvFor(dialect) + " is not configured")
	}
	dsn = ownDatabase(t, dialect, dsn)
	cfg := &config.Config{
		DBType:             dialect,
		DBDSN:              dsn,
		HeartbeatTimeoutMs: testHeartbeatTimeoutMs,
	}
	store, err := storage.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open store (%s): %v", dialect, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	truncateAppModelTables(t, store)
	return store
}

// ## Why this package owns a database instead of using the configured one
//
// The DSN in the environment is **shared**: `internal/storage` and
// `internal/api` read the same two variables, and `go test` runs packages
// **in parallel**. This package truncates the tables it writes, so on a
// shared database it wipes rows out from under a neighbour that is halfway
// through its own test.
//
// That is not a hypothetical, and it is not a defect in either neighbour:
//
//	go test -tags integration ./internal/storage/... ./internal/gateway/... ./internal/api/...
//	--- FAIL: TestFullApplicationRoutesEachPrefixToItsOwnComponent
//	    --- FAIL: TestFullApplicationRoutesEachPrefixToItsOwnComponent/mysql
//	# and on the next run, the failing subtest was /postgres
//
// A failure that **moves between engines** is the signature of a race. The
// fix is not to guess which assertion was wrong; it is to stop sharing.
//
// So this package **owns** a database whose name is derived from the
// configured one (`<name>_gw`), creates it once per run, and truncates inside
// it. The configured database is never dropped and never truncated from here.
//
// The account needs CREATE/DROP DATABASE rights — the same account that
// already has to be able to create the schema in the first place.
type ownedDatabase struct {
	once sync.Once
	dsn  string
	err  error
}

var ownedDatabases = map[string]*ownedDatabase{
	"postgres": {},
	"mysql":    {},
}

// ownDatabase returns the DSN of this package's own database for one dialect,
// creating it on first use.
func ownDatabase(t *testing.T, dialect, dsn string) string {
	t.Helper()
	owned, ok := ownedDatabases[dialect]
	if !ok {
		t.Fatalf("unknown dialect %q", dialect)
	}
	owned.once.Do(func() {
		owned.dsn, owned.err = createOwnedDatabase(dialect, dsn)
	})
	if owned.err != nil {
		t.Fatalf("准备 %s 自有库失败：%v", dialect, owned.err)
	}
	return owned.dsn
}

// createOwnedDatabase drops and recreates `<configured>_gw` and returns the
// DSN that points at it.
//
// It is dropped rather than merely emptied because a fresh database is the
// only version of "clean" that cannot inherit a row from a previous run.
func createOwnedDatabase(dialect, dsn string) (string, error) {
	name := databaseName(dsn) + "_gw"
	server := serverDSN(dialect, dsn)
	db, err := sql.Open(dialect, server)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if dialect == "mysql" {
		if _, err := db.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			return "", err
		}
		if _, err := db.Exec("CREATE DATABASE `" + name + "`"); err != nil {
			return "", err
		}
	} else {
		if _, err := db.Exec(`DROP DATABASE IF EXISTS "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			return "", err
		}
		if _, err := db.Exec(`CREATE DATABASE "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			return "", err
		}
	}
	return dsnWithDatabase(dialect, dsn, name), nil
}

// databaseName returns the database name of a DSN.
func databaseName(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		return strings.TrimPrefix(u.Path, "/")
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return ""
	}
	name := dsn[slash+1:]
	if q := strings.Index(name, "?"); q >= 0 {
		name = name[:q]
	}
	return name
}

// serverDSN returns a DSN that points at the server rather than at a
// database — that is how CREATE/DROP DATABASE is issued.
func serverDSN(dialect, dsn string) string {
	if dialect == "postgres" {
		if u, err := url.Parse(dsn); err == nil {
			u.Path = "/postgres"
			return u.String()
		}
		return dsn
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1]
}

// dsnWithDatabase returns the same DSN pointing at a different database.
func dsnWithDatabase(dialect, dsn, name string) string {
	if dialect == "postgres" {
		if u, err := url.Parse(dsn); err == nil {
			u.Path = "/" + name
			return u.String()
		}
		return dsn
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	rest := dsn[slash+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		return dsn[:slash+1] + name + rest[q:]
	}
	return dsn[:slash+1] + name
}

// dsnEnvFor returns the environment variable that carries the DSN for a
// dialect.
func dsnEnvFor(dialect string) string {
	switch dialect {
	case "postgres":
		return "GOACOS_TEST_PG_DSN"
	case "mysql":
		return "GOACOS_TEST_MYSQL_DSN"
	default:
		return ""
	}
}

// truncateAppModelTables clears the tables these tests write.
//
// One statement per table: MySQL's TRUNCATE takes a single table, and the
// form used here is valid on PostgreSQL too.
func truncateAppModelTables(t *testing.T, store *storage.Store) {
	t.Helper()
	for _, table := range appModelTables {
		if _, err := store.DB.ExecContext(context.Background(), "TRUNCATE TABLE "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
}

// forEachEngine runs a body against every engine, in its own subtest.
//
// A dialect with no DSN is skipped, and the skip shows up in the output. That
// visibility is deliberate: these tests used to be invisible, and the cost of
// that was a whole subsystem that had never been executed.
func forEachEngine(t *testing.T, body func(t *testing.T, store *storage.Store)) {
	t.Helper()
	for _, dialect := range []string{"postgres", "mysql"} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			body(t, openStore(t, dialect))
		})
	}
}
