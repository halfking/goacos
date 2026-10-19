package storage

// Versioned schema migrations.
//
// # Why this exists
//
// Until migration 002 goacos created its entire schema with a single
// "CREATE TABLE IF NOT EXISTS" script applied on every boot, which is fine
// exactly once: the day a column or a table has to change on an existing
// database, "IF NOT EXISTS" silently does nothing and the running server
// disagrees with the code about what the schema is. Adding the application
// model is that day, so the project needed a real migration ledger first.
//
// # Why the base schema is still applied
//
// Legacy deployments (252, 211) already have a database created by the old
// path. The base script is idempotent, so re-running it is harmless and it is
// what brings a brand-new empty database all the way up. Migrations then only
// carry what the base script cannot express — additive changes to databases
// that already exist.
//
// # Why each migration carries two dialects
//
// MySQL and PostgreSQL disagree on auto-increment, unsigned integers and index
// creation syntax. Rather than pretending they are the same and discovering it
// in production, every migration states both forms explicitly and the store
// picks the one matching its dialect. A migration that forgets one is a
// compile-time-adjacent bug caught here, not at deploy time.

import (
	"context"
	"embed"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
)

//go:embed schema/migrations
var migrationFS embed.FS

// migration is one numbered schema change, in both dialects.
type migration struct {
	Version int
	Name    string
}

// migrations is the ordered ledger. Never renumber or reorder an applied entry:
// the version number is what a deployed database remembers. To change an
// existing migration, add a new one.
var migrations = []migration{
	{Version: 2, Name: "application"},
	{Version: 3, Name: "gateway"},
	{Version: 4, Name: "instance_sweep"},
}

// baselineSchemaVersion is what a database created before the ledger existed
// reports through goacos_meta.schema_version. Migrations start at 2 so a legacy
// database is treated as "version 1" rather than as "unknown".
const baselineSchemaVersion = 1

// ensureMigrationLedger creates the ledger table. It is separate from the
// migration files because the ledger must exist before any file can be
// consulted — including its own.
func (s *Store) ensureMigrationLedger(ctx context.Context) error {
	ddl := map[Dialect]string{
		DialectMySQL: `CREATE TABLE IF NOT EXISTS goacos_schema_version (
  version int NOT NULL,
  name varchar(128) NOT NULL DEFAULT '',
  applied_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		DialectPostgres: `CREATE TABLE IF NOT EXISTS goacos_schema_version (
  version int NOT NULL PRIMARY KEY,
  name varchar(128) NOT NULL DEFAULT '',
  applied_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
	}[s.dialect]
	if ddl == "" {
		return fmt.Errorf("unknown dialect %q", s.dialect)
	}
	if _, err := s.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	return nil
}

// appliedMigrations returns the set of versions already recorded.
func (s *Store) appliedMigrations(ctx context.Context) (map[int]bool, error) {
	rows, err := s.Query(ctx, "SELECT version FROM goacos_schema_version")
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// migrationPath returns the embedded file for one migration under this dialect.
func migrationPath(m migration, d Dialect) (string, error) {
	suffix := "mysql"
	if d == DialectPostgres {
		suffix = "postgres"
	}
	name := fmt.Sprintf("%03d_%s.%s.sql", m.Version, m.Name, suffix)
	p := path.Join("schema/migrations", name)
	if _, err := migrationFS.ReadFile(p); err != nil {
		return "", fmt.Errorf("migration %03d_%s has no %s dialect file (%s): %w",
			m.Version, m.Name, suffix, p, err)
	}
	return p, nil
}

// runMigrations applies every pending migration in version order and records it.
// Safe to call on every boot: an applied migration is skipped by version, not
// by content, so editing an applied file has no retroactive effect and cannot
// corrupt an already-migrated database.
func (s *Store) runMigrations(ctx context.Context) error {
	if err := s.ensureMigrationLedger(ctx); err != nil {
		return err
	}
	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}
	ordered := append([]migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })

	// Work out what is pending before touching anything, so the boot log can
	// say which version the database is coming *from*.
	pending := make([]migration, 0, len(ordered))
	for _, m := range ordered {
		if m.Version <= baselineSchemaVersion {
			return fmt.Errorf("migration %03d collides with the pre-ledger baseline version %d", m.Version, baselineSchemaVersion)
		}
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		log.Printf("[goacos] schema at version %d (no migration applied)", highest(applied))
	} else {
		log.Printf("[goacos] schema at version %d, applying %d migration(s): %s",
			highest(applied), len(pending), describe(pending))
	}

	for _, m := range pending {
		p, err := migrationPath(m, s.dialect)
		if err != nil {
			return err
		}
		raw, err := migrationFS.ReadFile(p)
		if err != nil {
			return err
		}
		for _, stmt := range splitSQLStatements(string(raw)) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := s.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("migration %03d_%s: %q: %w", m.Version, m.Name, truncate(stmt, 80), err)
			}
		}
		if _, err := s.Exec(ctx,
			"INSERT INTO goacos_schema_version (version, name) VALUES (?, ?)", m.Version, m.Name); err != nil {
			return fmt.Errorf("record migration %03d: %w", m.Version, err)
		}
		log.Printf("[goacos] applied schema migration %03d_%s", m.Version, m.Name)
	}
	return nil
}

// highest reports the newest applied version, counting the pre-ledger
// baseline as version 1.
func highest(applied map[int]bool) int {
	version := baselineSchemaVersion
	for v := range applied {
		if v > version {
			version = v
		}
	}
	return version
}

// describe renders pending migrations for the boot log: "002_application,
// 003_gateway".
func describe(pending []migration) string {
	names := make([]string, 0, len(pending))
	for _, m := range pending {
		names = append(names, fmt.Sprintf("%03d_%s", m.Version, m.Name))
	}
	return strings.Join(names, ", ")
}

// CurrentSchemaVersion reports the highest applied version, counting the
// pre-ledger baseline as 1. Used by /nacos/v1/console/server/state so an
// operator can tell a database that never migrated from one that did.
func (s *Store) CurrentSchemaVersion(ctx context.Context) int {
	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return baselineSchemaVersion
	}
	version := baselineSchemaVersion
	for v := range applied {
		if v > version {
			version = v
		}
	}
	return version
}
