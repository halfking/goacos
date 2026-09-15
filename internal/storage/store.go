// Package storage provides the MySQL/PostgreSQL-backed persistence layer for goacos.
package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql driver
	_ "github.com/lib/pq"              // postgres driver
	"golang.org/x/crypto/bcrypt"

	"github.com/halfking/goacos/internal/config"
)

//go:embed schema
var schemaFS embed.FS

// Store wraps the database connection pool.
type Store struct {
	DB      *sql.DB
	cfg     *config.Config
	dialect Dialect
}

// Dialect reports the SQL engine backing this store.
func (s *Store) Dialect() Dialect { return s.dialect }

// Exec/Query wrappers run every statement through the dialect rebind, so
// call sites can keep writing MySQL-style `?` placeholders regardless of
// the backing engine (PostgreSQL receives $n placeholders).
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.DB.ExecContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.DB.QueryContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *Store) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.DB.QueryRowContext(ctx, s.dialect.Rebind(query), args...)
}

// DSN builds a MySQL connection string (kept for backward compatibility).
// New code should use DSNFor with an explicit dialect.
func DSN(host string, port int, user, password, dbName string) string {
	return DSNFor(DialectMySQL, host, port, user, password, dbName)
}

// Open connects to the database (MySQL or PostgreSQL, per config), creates the
// configured database when missing and ensures the schema + seed data exist.
// This is what makes goacos "attach" to a database at deploy time with zero
// manual setup.
func Open(ctx context.Context, cfg *config.Config) (*Store, error) {
	d := DialectFor(cfg.DBType, cfg.DBDSN)

	var db *sql.DB
	if cfg.DBDSN != "" {
		// fully-specified DSN from the deployment file: connect as-is, no
		// CREATE DATABASE (the DSN is expected to point at an existing db)
		pool, err := d.open(cfg.DBDSN, cfg.MaxOpenConns, cfg.MaxIdleConns, int64(cfg.ConnMaxLifetime/time.Second))
		if err != nil {
			return nil, fmt.Errorf("open dsn: %w", err)
		}
		db = pool
	} else {
		server, err := d.open(DSNFor(d, cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLUser, cfg.MySQLPassword, d.ServerDB()), 4, 2, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", d, err)
		}
		server.SetMaxOpenConns(4)
		if err := server.PingContext(ctx); err != nil {
			server.Close()
			return nil, fmt.Errorf("connect %s %s:%d: %w", d, cfg.MySQLHost, cfg.MySQLPort, err)
		}
		if err := d.EnsureDatabase(ctx, server, cfg.MySQLDB); err != nil {
			server.Close()
			return nil, fmt.Errorf("create database %s: %w", cfg.MySQLDB, err)
		}
		server.Close()

		pool, err := d.open(DSNFor(d, cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLUser, cfg.MySQLPassword, cfg.MySQLDB), cfg.MaxOpenConns, cfg.MaxIdleConns, int64(cfg.ConnMaxLifetime/time.Second))
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		db = pool
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database %s: %w", cfg.MySQLDB, err)
	}
	s := &Store{DB: db, cfg: cfg, dialect: d}
	if err := s.ensureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.Seed(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	name := "schema/schema.sql"
	if s.dialect == DialectPostgres {
		name = "schema/schema.postgres.sql"
	}
	raw, err := schemaFS.ReadFile(name)
	if err != nil {
		return err
	}
	for _, stmt := range splitSQLStatements(string(raw)) {
		if stmt == "" {
			continue
		}
		if _, err := s.Exec(ctx, s.dialect.Rebind(stmt)); err != nil {
			return fmt.Errorf("apply schema: %q: %w", truncate(stmt, 80), err)
		}
	}
	if _, err := s.Exec(ctx, s.dialect.Rebind(s.dialect.UpsertNoop("goacos_meta", "k", "v")),
		"schema_version", "1"); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

// splitSQLStatements splits a script on semicolon-terminated statements.
// The embedded schemas contain no procedures or semicolons inside strings.
func splitSQLStatements(script string) []string {
	lines := strings.Split(script, "\n")
	var stmts []string
	var cur strings.Builder
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "--") || t == "" {
			continue
		}
		if strings.HasPrefix(t, "SET NAMES") || strings.HasPrefix(t, "SET FOREIGN_KEY_CHECKS") {
			continue
		}
		cur.WriteString(ln)
		cur.WriteString("\n")
		if strings.HasSuffix(t, ";") {
			s := strings.TrimSpace(strings.TrimSuffix(cur.String(), ";"))
			if s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Seed inserts initial data (admin user, public namespace) when empty.
// It is idempotent and safe to run on every boot and from multiple nodes.
func (s *Store) Seed(ctx context.Context) error {
	var n int
	if err := s.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte(s.cfg.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx,
			"INSERT INTO users (username, password, enabled) VALUES (?, ?, 1)", s.cfg.AdminUsername, string(hash)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, s.dialect.Rebind(s.dialect.InsertIgnore("roles", "username", "role")),
			s.cfg.AdminUsername, "ROLE_ADMIN"); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, s.dialect.Rebind(s.dialect.InsertIgnore("permissions", "role", "resource", "action")),
			"ROLE_ADMIN", "/*", "rw"); err != nil {
			return err
		}
		log.Printf("[goacos] seeded admin user %q (initial password from ADMIN_PASSWORD)", s.cfg.AdminUsername)
	}
	// public namespace row so console tooling sees it
	var m int
	if err := s.QueryRow(ctx,
		"SELECT COUNT(*) FROM tenant_info WHERE kp='1' AND tenant_id=''").Scan(&m); err == nil && m == 0 {
		_, _ = s.Exec(ctx,
			"INSERT INTO tenant_info (kp, tenant_id, namespace_name, namespace_desc) VALUES ('1','', 'public', 'Public Namespace')")
	}
	return nil
}

// Close closes the pool.
func (s *Store) Close() error { return s.DB.Close() }

// NewID returns a random 32-hex-char identifier.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NowMS returns current unix milliseconds.
func NowMS() int64 { return time.Now().UnixMilli() }
