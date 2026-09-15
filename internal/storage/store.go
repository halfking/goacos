// Package storage provides the MySQL-backed persistence layer for goacos.
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

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"github.com/halfking/goacos/internal/config"
)

//go:embed schema/schema.sql
var schemaFS embed.FS

// Store wraps the MySQL connection pool.
type Store struct {
	DB  *sql.DB
	cfg *config.Config
}

// DSN builds a MySQL DSN. When dbName is empty the DSN connects without
// selecting a database (used for CREATE DATABASE).
func DSN(host string, port int, user, password, dbName string) string {
	params := "charset=utf8mb4&parseTime=true&loc=Local&timeout=5s&readTimeout=30s&writeTimeout=30s&interpolateParams=true"
	if dbName != "" {
		dbName = "/" + dbName
	} else {
		dbName = "/"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)%s?%s", user, password, host, port, dbName, params)
}

// Open connects to MySQL, creates the configured database when missing and
// ensures the schema + seed data exist. This is what makes goacos "attach" to
// a database at deploy time with zero manual setup.
func Open(ctx context.Context, cfg *config.Config) (*Store, error) {
	server, err := sql.Open("mysql", DSN(cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLUser, cfg.MySQLPassword, ""))
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	server.SetMaxOpenConns(4)
	if err := server.PingContext(ctx); err != nil {
		server.Close()
		return nil, fmt.Errorf("connect mysql %s: %w", cfg.MySQLAddr(), err)
	}
	if _, err := server.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci", cfg.MySQLDB)); err != nil {
		server.Close()
		return nil, fmt.Errorf("create database %s: %w", cfg.MySQLDB, err)
	}
	server.Close()

	db, err := sql.Open("mysql", DSN(cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLUser, cfg.MySQLPassword, cfg.MySQLDB))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database %s: %w", cfg.MySQLDB, err)
	}
	s := &Store{DB: db, cfg: cfg}
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
	raw, err := schemaFS.ReadFile("schema/schema.sql")
	if err != nil {
		return err
	}
	for _, stmt := range splitSQLStatements(string(raw)) {
		if stmt == "" {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %q: %w", truncate(stmt, 80), err)
		}
	}
	_, _ = s.DB.ExecContext(ctx,
		"INSERT INTO goacos_meta (k,v) VALUES ('schema_version','1') ON DUPLICATE KEY UPDATE v=v")
	return nil
}

// splitSQLStatements splits a script on semicolon-terminated statements.
// The embedded schema contains no procedures or semicolons inside strings.
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
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte(s.cfg.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx,
			"INSERT INTO users (username, password, enabled) VALUES (?, ?, 1)", s.cfg.AdminUsername, string(hash)); err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx,
			"INSERT IGNORE INTO roles (username, role) VALUES (?, 'ROLE_ADMIN')", s.cfg.AdminUsername); err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx,
			"INSERT IGNORE INTO permissions (role, resource, action) VALUES ('ROLE_ADMIN', '/*', 'rw')"); err != nil {
			return err
		}
		log.Printf("[goacos] seeded admin user %q (initial password from ADMIN_PASSWORD)", s.cfg.AdminUsername)
	}
	// public namespace row so console tooling sees it
	var m int
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tenant_info WHERE kp='1' AND tenant_id=''").Scan(&m); err == nil && m == 0 {
		_, _ = s.DB.ExecContext(ctx,
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
