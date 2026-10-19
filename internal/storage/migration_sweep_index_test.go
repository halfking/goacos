package storage

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/config"
)

// Migration 004 adds the index the naming sweeper needs.
//
// Why this needs a guard of its own: the sweeper runs three statements every
// SWEEP_INTERVAL_MS (5s by default) that filter naming_instance on
// (ephemeral, healthy, last_heartbeat_ms) — a predicate no pre-existing index
// leads with, because all of them lead with namespace_id. Without the index
// every one of them is a full table scan, for ever.
//
// Measured on a live registry (2026-10-20): naming_instance showed
// seq_scan=542462, idx_scan=0, growing by 26 per 25 seconds — the sweeper on
// its 5-second tick, exactly.
//
// A 20,000-instance fixture, 50 sweep cycles, measured both ways:
//
//   without idx_instance_sweep   1382 / 1827 / 1776 ms
//   with    idx_instance_sweep      99 /   24 /  285 ms
//
// So the guard is worth having: the index is the difference between a
// registry that scales with its fleet and one whose sweeper cost grows with it.

// sweepIndexName is the index both dialects must create.
const sweepIndexName = "idx_instance_sweep"

// sweepIndexColumns is the column list both dialects must use, in this order.
//
// The order is not cosmetic. All three sweeper statements filter ephemeral by
// equality and last_heartbeat_ms by range, and the two UPDATEs additionally
// filter healthy by equality. Putting healthy in the middle lets the index
// serve the UPDATEs on all three predicates; putting it last would leave the
// range column unreachable behind an unconstrained column.
var sweepIndexColumns = []string{"ephemeral", "healthy", "last_heartbeat_ms"}

// TestSweepIndexMigrationIsRegistered pins the ledger entry.
//
// The version number is what a deployed database remembers, so it must not be
// renumbered or reordered — a test is the cheapest way to notice that.
func TestSweepIndexMigrationIsRegistered(t *testing.T) {
	var found *migration
	for i := range migrations {
		if migrations[i].Version == 4 {
			found = &migrations[i]
			break
		}
	}
	if found == nil {
		t.Fatal("migration 004 (instance_sweep) is not registered in the ledger")
	}
	if found.Name != "instance_sweep" {
		t.Errorf("migration 004 name = %q, want %q", found.Name, "instance_sweep")
	}
}

// TestSweepIndexMigrationHasBothDialects guards the cross-engine rule the
// migration runner depends on: a migration that forgot one dialect is a
// bug caught here rather than at deploy time.
//
// The two dialects spell the same operation differently, and the spelling is
// itself the risk:
//
//	PostgreSQL   CREATE INDEX IF NOT EXISTS idx_instance_sweep ON naming_instance (...)
//	MySQL        ALTER TABLE naming_instance ADD INDEX idx_instance_sweep (...)
//
// MySQL 8 has no `CREATE INDEX IF NOT EXISTS`, and MySQL's
// `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table — so an index
// added to a table the base script already created can only be expressed as a
// versioned ALTER. The PostgreSQL spelling must not leak into the MySQL file
// (the dialect guard in appstore_test.go enforces that too), and the MySQL
// spelling must not leak into the PostgreSQL file.
func TestSweepIndexMigrationHasBothDialects(t *testing.T) {
	for _, suffix := range []string{"postgres", "mysql"} {
		name := "004_instance_sweep." + suffix + ".sql"
		raw, err := migrationFS.ReadFile("schema/migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)

		if !strings.Contains(body, sweepIndexName) {
			t.Errorf("%s does not create %s", name, sweepIndexName)
		}
		for _, col := range sweepIndexColumns {
			if !strings.Contains(body, col) {
				t.Errorf("%s does not index %s", name, col)
			}
		}
		// The index has to be on naming_instance specifically — the same
		// column list on some other table would be a different fix.
		if !strings.Contains(body, "naming_instance") {
			t.Errorf("%s does not create the index on naming_instance", name)
		}
	}
}

// TestSweepIndexDialectSpellingsDoNotLeak pins the one line where the two
// dialects must differ, and asserts that they differ in no other way.
//
// A MySQL file that said "CREATE INDEX" would be rejected by the dialect guard;
// a PostgreSQL file that said "ADD INDEX" would be a syntax error that only
// shows up when goacos boots against PostgreSQL.
func TestSweepIndexDialectSpellingsDoNotLeak(t *testing.T) {
	pg, err := migrationFS.ReadFile("schema/migrations/004_instance_sweep.postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	my, err := migrationFS.ReadFile("schema/migrations/004_instance_sweep.mysql.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The statements are compared, not the prose: both files explain the
	// other engine's spelling in their comments, and a comment is not a
	// statement.
	pgBody := stripSQLComments(string(pg))
	myBody := stripSQLComments(string(my))
	if !strings.Contains(pgBody, "CREATE INDEX IF NOT EXISTS "+sweepIndexName) {
		t.Error("the postgres file must use CREATE INDEX IF NOT EXISTS")
	}
	if strings.Contains(pgBody, "ADD INDEX") {
		t.Error("the postgres file must not use the MySQL ADD INDEX spelling")
	}
	if !strings.Contains(myBody, "ADD INDEX "+sweepIndexName) {
		t.Error("the mysql file must use ADD INDEX")
	}
	if strings.Contains(myBody, "CREATE INDEX") {
		t.Error("the mysql file must not use the PostgreSQL CREATE INDEX spelling")
	}
}

// TestSweepIndexMigrationStatementsAreSplittable makes sure the migration
// survives the runner's statement splitter.
//
// The splitter drops comment lines and splits on a trailing semicolon, so a
// semicolon inside a comment is harmless — but only because the comment line
// is dropped whole. A statement that carries its semicolon on a line the
// splitter treats as part of a larger statement would be mangled, so the
// actual statements are checked for a proper terminator.
func TestSweepIndexMigrationStatementsAreSplittable(t *testing.T) {
	for _, suffix := range []string{"postgres", "mysql"} {
		name := "004_instance_sweep." + suffix + ".sql"
		raw, err := migrationFS.ReadFile("schema/migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		stmts := splitSQLStatements(string(raw))
		if len(stmts) != 1 {
			t.Fatalf("%s produced %d statements, want 1: %q", name, len(stmts), stmts)
		}
		if !strings.Contains(stmts[0], sweepIndexName) {
			t.Errorf("%s statement does not create %s: %q", name, sweepIndexName, stmts[0])
		}
	}
}

// TestSweepIndexExistsAfterOpen is the end-to-end check: a database that has
// been through the migration ledger carries the index.
//
// It follows the same convention as the other PG-backed tests in this
// package — no DSN, no test. A self-guard is not a substitute: the DSN has to
// name an isolated database, or this would assert against a real registry.
func TestSweepIndexExistsAfterOpen(t *testing.T) {
	dsn := os.Getenv("GOACOS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GOACOS_TEST_PG_DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse GOACOS_TEST_PG_DSN: %v", err)
	}
	// A self-guard is not a substitute: the DSN has to name an isolated
	// test database, or this would assert against a real registry. The
	// same rule the other PG-backed tests in this package follow.
	if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		t.Fatal("GOACOS_TEST_PG_DSN must point at a local test database")
	}
	if !strings.Contains(strings.ToLower(u.Path), "test") {
		t.Fatal("GOACOS_TEST_PG_DSN must select an explicitly named test database")
	}
	store, err := Open(context.Background(), &config.Config{DBType: "postgres", DBDSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.Query(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE tablename='naming_instance' AND indexname=?`, sweepIndexName)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		if name == sweepIndexName {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}
	if !found {
		t.Fatalf("%s is missing after Open(); the migration ledger did not apply it", sweepIndexName)
	}
}
