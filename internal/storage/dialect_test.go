package storage

import (
	"strings"
	"testing"
)

func TestDialectFor(t *testing.T) {
	cases := []struct {
		dbType, dsn string
		want        Dialect
	}{
		{"", "", DialectMySQL},
		{"mysql", "", DialectMySQL},
		{"postgres", "", DialectPostgres},
		{"POSTGRESQL", "", DialectPostgres},
		{"pg", "", DialectPostgres},
		{"mysql", "postgres://u:p@h:5432/db", DialectPostgres}, // DSN wins
		{"", "postgresql://u:p@h/db", DialectPostgres},
		{"bogus", "", DialectMySQL},
	}
	for _, c := range cases {
		if got := DialectFor(c.dbType, c.dsn); got != c.want {
			t.Errorf("DialectFor(%q,%q)=%v want %v", c.dbType, c.dsn, got, c.want)
		}
	}
}

func TestRebind(t *testing.T) {
	if got := DialectMySQL.Rebind("INSERT INTO t (a,b) VALUES (?,?)"); got != "INSERT INTO t (a,b) VALUES (?,?)" {
		t.Errorf("mysql rebind should be identity, got %q", got)
	}
	got := DialectPostgres.Rebind("SELECT * FROM t WHERE a=? AND b LIKE CONCAT('%',?,'%') LIMIT ? OFFSET ?")
	want := "SELECT * FROM t WHERE a=$1 AND b LIKE CONCAT('%',$2,'%') LIMIT $3 OFFSET $4"
	if got != want {
		t.Errorf("pg rebind:\n got %q\nwant %q", got, want)
	}
	if got := DialectPostgres.Rebind("no placeholders"); got != "no placeholders" {
		t.Errorf("no-op rebind failed: %q", got)
	}
}

func TestInsertIgnore(t *testing.T) {
	m := DialectMySQL.InsertIgnore("roles", "username", "role")
	if m != "INSERT IGNORE INTO roles (username, role) VALUES (?,?)" {
		t.Errorf("mysql insertIgnore: %q", m)
	}
	p := DialectPostgres.InsertIgnore("roles", "username", "role")
	if p != "INSERT INTO roles (username, role) VALUES (?,?) ON CONFLICT DO NOTHING" {
		t.Errorf("pg insertIgnore: %q", p)
	}
}

func TestUpsertNoop(t *testing.T) {
	if got := DialectMySQL.UpsertNoop("goacos_meta", "k", "v"); !strings.Contains(got, "ON DUPLICATE KEY UPDATE v=v") {
		t.Errorf("mysql upsertNoop: %q", got)
	}
	if got := DialectPostgres.UpsertNoop("goacos_meta", "k", "v"); !strings.Contains(got, `ON CONFLICT (k) DO NOTHING`) {
		t.Errorf("pg upsertNoop: %q", got)
	}
}

func TestDSNFor(t *testing.T) {
	pg := DSNFor(DialectPostgres, "127.0.0.1", 5432, "postgres", "p@ss", "goacos")
	if !strings.HasPrefix(pg, "postgres://postgres:p%40ss@127.0.0.1:5432/goacos?") {
		t.Errorf("pg DSN (password escaping expected): %q", pg)
	}
	pgNoDB := DSNFor(DialectPostgres, "h", 5432, "u", "p", "")
	if !strings.Contains(pgNoDB, "/postgres?") {
		t.Errorf("pg no-db should connect to postgres db: %q", pgNoDB)
	}
	my := DSNFor(DialectMySQL, "127.0.0.1", 3306, "root", "pw", "goacos")
	if !strings.Contains(my, "@tcp(127.0.0.1:3306)/goacos?") {
		t.Errorf("mysql DSN: %q", my)
	}
}

func TestSplitSQLStatementsPostgres(t *testing.T) {
	raw, err := schemaFS.ReadFile("schema/schema.postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmts := splitSQLStatements(string(raw))
	if len(stmts) < 20 {
		t.Fatalf("postgres schema should yield 20+ statements, got %d", len(stmts))
	}
	for _, s := range stmts {
		if strings.Contains(s, "?") {
			t.Errorf("DDL should contain no placeholders: %q", truncate(s, 60))
		}
	}
}
