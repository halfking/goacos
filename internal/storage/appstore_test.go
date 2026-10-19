package storage

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestPathPrefixMatches(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/", "/anything", true},
		{"/", "/", true},
		{"", "/anything", true},
		{"/api", "/api", true},
		{"/api", "/api/v2/health", true},
		{"/api", "/apixyz", false},
		{"/api/", "/api/v2/health", true},
		{"/api/v2/", "/api/v2/runtime/register", true},
		{"/api/v2/", "/api/v3/x", false},
	}
	for _, c := range cases {
		if got := pathPrefixMatches(c.prefix, c.path); got != c.want {
			t.Errorf("pathPrefixMatches(%q,%q) = %v, want %v", c.prefix, c.path, got, c.want)
		}
	}
}

func TestFilterInstancesByMeta(t *testing.T) {
	instances := []Instance{
		{IP: "10.0.0.1", MetadataJSON: `{"lane":"canary","zone":"a"}`},
		{IP: "10.0.0.2", MetadataJSON: `{"lane":"stable","zone":"a"}`},
		{IP: "10.0.0.3", MetadataJSON: ``}, // unlabelled
	}
	got := filterInstancesByMeta(instances, map[string]string{"lane": "canary"})
	if len(got) != 1 || got[0].IP != "10.0.0.1" {
		t.Fatalf("canary filter wrong: %+v", got)
	}
	got = filterInstancesByMeta(instances, map[string]string{"zone": "a"})
	if len(got) != 2 {
		t.Fatalf("zone filter should keep 2, got %d", len(got))
	}
	// An unlabelled instance must NOT match a filter that names a key: keeping
	// it in a canary set is the dangerous direction, not the safe one.
	got = filterInstancesByMeta(instances, map[string]string{"lane": ""})
	if len(got) != 1 {
		t.Fatalf("empty-value filter should keep only the unlabelled instance, got %d", len(got))
	}
	if len(filterInstancesByMeta(instances, nil)) != len(instances) {
		t.Fatalf("nil filter must be a no-op")
	}
}

func TestSortInstancesByWeight(t *testing.T) {
	instances := []Instance{
		{IP: "10.0.0.3", Weight: 1},
		{IP: "10.0.0.1", Weight: 5},
		{IP: "10.0.0.2", Weight: 5},
	}
	sortInstances(instances, "round_robin")
	if instances[0].IP != "10.0.0.1" || instances[1].IP != "10.0.0.2" || instances[2].IP != "10.0.0.3" {
		t.Fatalf("weight-then-ip ordering wrong: %v %v %v", instances[0].IP, instances[1].IP, instances[2].IP)
	}
}

// Every registered migration must ship both dialects. A missing one compiles
// fine and fails only at deploy time, against whichever database nobody tested.
//
// The check is stronger than "both files exist": the two dialects must declare
// the same set of tables. That is the invariant that actually breaks in
// practice — a table added to the MySQL twin during a feature and forgotten in
// the PostgreSQL one produces a server that boots, passes every unit test, and
// then 500s on the first query against that table in production.
//
// "Declares something" is the per-dialect half of that check, and it is
// deliberately about **schema objects** rather than tables. A migration is
// allowed to add an index to a table the base script already created — that is
// exactly what the base script cannot express on MySQL, where
// `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table and MySQL 8 has
// no `CREATE INDEX IF NOT EXISTS`. Counting tables alone would reject that
// legitimate case while catching nothing the object check misses, because a
// dialect file that declares no table and no index does nothing at all.
func TestEveryMigrationHasBothDialects(t *testing.T) {
	for _, m := range migrations {
		tables := map[Dialect][]string{}
		for _, d := range []Dialect{DialectMySQL, DialectPostgres} {
			p, err := migrationPath(m, d)
			if err != nil {
				t.Errorf("migration %03d_%s: %v", m.Version, m.Name, err)
				continue
			}
			raw, err := migrationFS.ReadFile(p)
			if err != nil {
				t.Errorf("read %s: %v", p, err)
				continue
			}
			body := string(raw)
			// Comments are stripped before the tables are extracted, for the
			// same reason TestApplicationModelHasNoAddressColumns strips
			// them: a migration is allowed to *discuss* a construct in prose,
			// and a naive scan would read the discussion as a declaration.
			// The PostgreSQL file for migration 004 says out loud that
			// MySQL's `CREATE TABLE IF NOT EXISTS` is a no-op, and without
			// this the MySQL twin was scored as creating a table called "IF".
			tables[d] = createdTables(stripSQLComments(body))
			if len(createdSchemaObjects(body)) == 0 {
				t.Errorf("%s declares no schema objects", p)
			}
		}
		if len(tables) != 2 {
			continue
		}
		if !reflect.DeepEqual(tables[DialectMySQL], tables[DialectPostgres]) {
			t.Errorf("migration %03d_%s declares different tables per dialect:\n  mysql:    %v\n  postgres: %v",
				m.Version, m.Name, tables[DialectMySQL], tables[DialectPostgres])
		}
	}
}

// createdTables extracts "CREATE TABLE <name>" targets, ignoring the two
// dialects' different identifier quoting so the comparison is about schema,
// not syntax.
var createTableRe = regexp.MustCompile("(?i)CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?[`\"]?([A-Za-z0-9_]+)")

func createdTables(sql string) []string {
	matches := createTableRe.FindAllStringSubmatch(sql, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// The two dialects spell an index three different ways, and a migration has to
// be able to use any of them:
//
//	PostgreSQL   CREATE INDEX [IF NOT EXISTS] name ON table (...)
//	MySQL inline KEY name (...)            -- inside a CREATE TABLE body
//	MySQL later  ALTER TABLE table ADD INDEX name (...)
var createIndexRe = regexp.MustCompile("(?i)CREATE\\s+INDEX(?:\\s+IF\\s+NOT\\s+EXISTS)?\\s+[`\"]?([A-Za-z0-9_]+)")
var inlineKeyRe = regexp.MustCompile("(?i)\\bKEY\\s+[`\"]?([A-Za-z0-9_]+)")
var addIndexRe = regexp.MustCompile("(?i)\\bADD\\s+(?:UNIQUE\\s+)?INDEX\\s+[`\"]?([A-Za-z0-9_]+)")

// createdSchemaObjects returns every table and index a migration declares.
//
// It is the "does this dialect file do anything" measure, deliberately
// counting indexes as well as tables so that an index-only migration — adding
// an index to a table the base script already created — is a legitimate
// member of the ledger rather than a shape the guard has to be argued past.
func createdSchemaObjects(sql string) []string {
	out := createdTables(sql)
	for _, re := range []*regexp.Regexp{createIndexRe, inlineKeyRe, addIndexRe} {
		for _, m := range re.FindAllStringSubmatch(sql, -1) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// The application model must never carry an address column: the whole point is
// that instances come from live registry state, not from a second copy of it.
//
// Comments are stripped before checking, because this file's own comments
// discuss the absence of an "IP or port column" in prose — a naive substring
// scan would flag its own explanation as the violation it forbids.
func TestApplicationModelHasNoAddressColumns(t *testing.T) {
	forbidden := []string{"ip", "host", "port", "endpoint", "address"}
	for _, m := range migrations {
		for _, d := range []Dialect{DialectMySQL, DialectPostgres} {
			raw, err := migrationFS.ReadFile(migrationPathOrFatal(t, m, d))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if i := strings.Index(line, "--"); i >= 0 {
					line = line[:i]
				}
				if strings.TrimSpace(line) == "" {
					continue
				}
				// Only look at column-definition lines: "name type ..." inside
				// a CREATE TABLE body, not index or constraint clauses.
				lower := strings.ToLower(line)
				if !strings.HasPrefix(strings.TrimSpace(lower), "constraint ") &&
					!strings.HasPrefix(strings.TrimSpace(lower), "unique ") &&
					!strings.HasPrefix(strings.TrimSpace(lower), "primary key") {
					for _, f := range forbidden {
						if strings.HasPrefix(strings.TrimSpace(lower), f+" ") {
							t.Errorf("migration %03d_%s (%s) declares column %q — "+
								"instances must be resolved from naming_instance, never copied",
								m.Version, m.Name, d, f)
						}
					}
				}
			}
		}
	}
}

// Cross-dialect type leakage is invisible to the compiler and to a MySQL-only
// test run: `weight double` is valid MySQL and invalid PostgreSQL, so the
// migration file itself was fine right up until goacos was started against the
// second engine and exited during boot. Each dialect file must therefore be
// free of the *other* engine's spellings.
func TestMigrationDialectsDoNotLeak(t *testing.T) {
	// Spellings that only one engine understands.
	postgresOnly := []string{"bigserial", "double precision", "ON CONFLICT", "CREATE INDEX"}
	mysqlOnly := []string{
		"AUTO_INCREMENT", "ENGINE=", "UNIQUE KEY", "KEY idx_",
		"ON DUPLICATE KEY", "tinyint", "datetime",
	}
	for _, m := range migrations {
		for _, d := range []Dialect{DialectMySQL, DialectPostgres} {
			raw, err := migrationFS.ReadFile(migrationPathOrFatal(t, m, d))
			if err != nil {
				t.Fatal(err)
			}
			lower := strings.ToLower(stripSQLComments(string(raw)))
			var forbidden []string
			if d == DialectPostgres {
				forbidden = mysqlOnly
				// Bare `double` is the MySQL spelling; PostgreSQL needs
				// `double precision`.
				for _, line := range strings.Split(lower, "\n") {
					if strings.Contains(line, "double") && !strings.Contains(line, "double precision") {
						t.Errorf("migration %03d_%s (postgres) uses bare `double`, which PostgreSQL rejects: %s",
							m.Version, m.Name, strings.TrimSpace(line))
					}
				}
			} else {
				forbidden = postgresOnly
			}
			for _, f := range forbidden {
				if strings.Contains(lower, strings.ToLower(f)) {
					t.Errorf("migration %03d_%s (%s) contains %q, which belongs to the other engine",
						m.Version, m.Name, d, f)
				}
			}
		}
	}
}

// stripSQLComments removes `--` comments so prose about a construct is never
// mistaken for the construct itself.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func migrationPathOrFatal(t *testing.T, m migration, d Dialect) string {
	t.Helper()
	p, err := migrationPath(m, d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A version at or below the pre-ledger baseline would be invisible to a legacy
// database: the ledger would already claim it is applied.
func TestMigrationsStartAboveBaseline(t *testing.T) {
	for _, m := range migrations {
		if m.Version <= baselineSchemaVersion {
			t.Errorf("migration %03d must be above baseline %d", m.Version, baselineSchemaVersion)
		}
	}
	// Versions must be unique and strictly increasing.
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Errorf("migrations not strictly increasing at %d", i)
		}
	}
}

func TestDialectUpsertUsesNativeConflictSyntax(t *testing.T) {
	key := []string{"namespace_id", "domain", "path_prefix"}
	ins := []string{"namespace_id", "domain", "path_prefix", "app_name"}
	set := []string{"app_name"}

	my := DialectMySQL.Upsert("route_binding", key, ins, set)
	if !strings.Contains(my, "ON DUPLICATE KEY UPDATE") || strings.Contains(my, "ON CONFLICT") {
		t.Errorf("mysql upsert wrong: %s", my)
	}
	if !strings.Contains(my, "app_name=VALUES(app_name)") {
		t.Errorf("mysql upsert should assign from VALUES(): %s", my)
	}

	pg := DialectPostgres.Upsert("route_binding", key, ins, set)
	if !strings.Contains(pg, "ON CONFLICT (namespace_id, domain, path_prefix) DO UPDATE SET") {
		t.Errorf("postgres upsert wrong: %s", pg)
	}
	if !strings.Contains(pg, "app_name=excluded.app_name") {
		t.Errorf("postgres upsert should assign from excluded: %s", pg)
	}
	// Both must be rebindable, i.e. written with ? placeholders.
	for _, q := range []string{my, pg} {
		if strings.Count(q, "?") != len(ins) {
			t.Errorf("placeholder count wrong in %q", q)
		}
	}
}
