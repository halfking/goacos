package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Dialect identifies the SQL engine backing the store.
type Dialect string

const (
	DialectMySQL    Dialect = "mysql"
	DialectPostgres Dialect = "postgres"
)

// DialectFor resolves the dialect from an explicit type and/or DSN override.
// A postgres:// DSN always wins; otherwise the type string decides
// (mysql | postgres | postgresql | pg; empty/unknown → MySQL, the historical default).
func DialectFor(dbType, dsn string) Dialect {
	low := strings.ToLower(dsn)
	if strings.HasPrefix(low, "postgres://") || strings.HasPrefix(low, "postgresql://") {
		return DialectPostgres
	}
	switch strings.ToLower(strings.TrimSpace(dbType)) {
	case "postgres", "postgresql", "pg":
		return DialectPostgres
	default:
		return DialectMySQL
	}
}

// Rebind converts ? placeholders to $n for PostgreSQL. Both engines share
// every other construct used by goacos (COALESCE, NOW(), LIMIT/OFFSET, CONCAT).
// goacos SQL contains no literal '?' characters, so a sequential rewrite is safe.
func (d Dialect) Rebind(query string) string {
	if d != DialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BlurMatch returns the fuzzy-match fragment for blur searches. PostgreSQL's
// CONCAT is variadic "any" and cannot infer parameter types (SQLSTATE 42P18),
// so PG uses explicit text concatenation instead.
func (d Dialect) BlurMatch() string {
	if d == DialectPostgres {
		return "LIKE '%' || ? || '%'"
	}
	return "LIKE CONCAT('%',?,'%')"
}

// InsertIgnore builds an idempotent INSERT for the dialect. The target table
// must have a unique/PK constraint matching cols.
func (d Dialect) InsertIgnore(table string, cols ...string) string {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	collist := strings.Join(cols, ", ")
	if d == DialectPostgres {
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING", table, collist, ph)
	}
	return fmt.Sprintf("INSERT IGNORE INTO %s (%s) VALUES (%s)", table, collist, ph)
}

// UpsertNoop builds an insert that keeps the existing row on conflict
// (used for goacos_meta schema bookkeeping).
func (d Dialect) UpsertNoop(table, keyCol, valCol string) string {
	if d == DialectPostgres {
		return fmt.Sprintf("INSERT INTO %s (%s,%s) VALUES (?,?) ON CONFLICT (%s) DO NOTHING", table, keyCol, valCol, keyCol)
	}
	return fmt.Sprintf("INSERT INTO %s (%s,%s) VALUES (?,?) ON DUPLICATE KEY UPDATE %s=%s", table, keyCol, valCol, valCol, valCol)
}

// ServerDB returns the database to connect to when no target database exists
// yet: MySQL allows connecting without a database, PostgreSQL needs "postgres".
func (d Dialect) ServerDB() string {
	if d == DialectPostgres {
		return "postgres"
	}
	return ""
}

// DSNFor builds a connection string for the dialect. dbName may be empty
// (connects to the server/default database).
func DSNFor(d Dialect, host string, port int, user, password, dbName string) string {
	if d == DialectPostgres {
		db := dbName
		if db == "" {
			db = "postgres"
		}
		uid := url.UserPassword(user, password)
		return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable&connect_timeout=5", uid.String(), host, port, db)
	}
	params := "charset=utf8mb4&parseTime=true&loc=Local&timeout=5s&readTimeout=30s&writeTimeout=30s&interpolateParams=true"
	if dbName != "" {
		dbName = "/" + dbName
	} else {
		dbName = "/"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)%s?%s", user, password, host, port, dbName, params)
}

// DriverName returns the database/sql driver registered for the dialect.
func (d Dialect) DriverName() string {
	if d == DialectPostgres {
		return "postgres"
	}
	return "mysql"
}

// driverName returns the database/sql driver registered for the dialect.
func (d Dialect) driverName() string { return d.DriverName() }

// EnsureDatabase creates the target database when missing. Idempotent.
func (d Dialect) EnsureDatabase(ctx context.Context, db *sql.DB, name string) error {
	if d == DialectPostgres {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_database WHERE datname=$1", name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
			// tolerate a concurrent creator
			if strings.Contains(err.Error(), "42P04") || strings.Contains(strings.ToLower(err.Error()), "already exists") {
				return nil
			}
			return err
		}
		return nil
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci", name))
	return err
}

// open opens a pool for the dialect.
func (d Dialect) open(dsn string, maxOpen, maxIdle int, lifetimeSeconds int64) (*sql.DB, error) {
	db, err := sql.Open(d.driverName(), dsn)
	if err != nil {
		return nil, err
	}
	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}
	if maxIdle > 0 {
		db.SetMaxIdleConns(maxIdle)
	}
	if lifetimeSeconds > 0 {
		db.SetConnMaxLifetime(time.Duration(lifetimeSeconds) * time.Second)
	}
	return db, nil
}
