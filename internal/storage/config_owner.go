package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var ErrConfigInactive = errors.New("configuration instance is not database-active")

// One database-local advisory key, held by the same physical connection as
// every config transaction. No heartbeat, process label or second registry.
const configAdvisoryKey int64 = 0x676f61636f73
const configLockHigh int64 = configAdvisoryKey >> 32
const configLockLow int64 = configAdvisoryKey & 0xffffffff

type configOwner struct {
	conn      *sql.Conn
	sem       chan struct{}
	done      chan struct{}
	once      sync.Once
	lost      atomic.Bool
	mysqlName string
}
type configTransactionKey struct{}
type configTransaction struct {
	store       *Store
	tx          *sql.Tx
	afterCommit []func()
}

func (s *Store) configTransaction(ctx context.Context) *configTransaction {
	v, _ := ctx.Value(configTransactionKey{}).(*configTransaction)
	if v != nil && v.store == s {
		return v
	}
	return nil
}
func (o *configOwner) lose() { o.lost.Store(true); o.once.Do(func() { close(o.done) }) }
func (o *configOwner) discard() {
	o.lose()
	_ = o.conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = o.conn.Close()
}
func (s *Store) AcquireConfigOwnership(ctx context.Context) error {
	s.configOwnerMu.Lock()
	defer s.configOwnerMu.Unlock()
	if o := s.configOwner.Load(); o != nil {
		if o.lost.Load() {
			return ErrConfigInactive
		}
		return s.CheckConfigOwnership(ctx)
	}
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return fmt.Errorf("%w: connection unavailable", ErrConfigInactive)
	}
	o := &configOwner{conn: conn, sem: make(chan struct{}, 1), done: make(chan struct{})}
	o.sem <- struct{}{}
	var acquired bool
	if s.dialect == DialectPostgres {
		e = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", configAdvisoryKey).Scan(&acquired)
	} else if s.dialect == DialectMySQL {
		var name string
		e = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name)
		if e == nil {
			hash := sha256.Sum256([]byte(name))
			o.mysqlName = "goacos:config:" + hex.EncodeToString(hash[:16])
			var value sql.NullInt64
			e = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", o.mysqlName).Scan(&value)
			acquired = value.Valid && value.Int64 == 1
		}
	} else {
		e = ErrConfigInactive
	}
	if e != nil || !acquired {
		o.discard()
		return ErrConfigInactive
	}
	if s.dialect == DialectMySQL {
		// Existing CREATE TABLE IF NOT EXISTS cannot repair a legacy MyISAM
		// history table. Refuse instead of claiming atomic rollback on it.
		var transactional int
		e = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('config_info','his_config_info','config_tags_relation','tenant_info') AND ENGINE='InnoDB'`).Scan(&transactional)
		if e != nil || transactional != 4 {
			o.discard()
			return ErrConfigInactive
		}
	}
	s.configOwner.Store(o)
	return nil
}
func (s *Store) verifyConfigOwner(ctx context.Context, o *configOwner, tx *sql.Tx) error {
	if o == nil || o.lost.Load() {
		return ErrConfigInactive
	}
	var owns bool
	var e error
	if s.dialect == DialectPostgres {
		q := `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=1)`
		if tx != nil {
			e = tx.QueryRowContext(ctx, q, configLockHigh, configLockLow).Scan(&owns)
		} else {
			e = o.conn.QueryRowContext(ctx, q, configLockHigh, configLockLow).Scan(&owns)
		}
	} else if s.dialect == DialectMySQL {
		q := `SELECT COALESCE(IS_USED_LOCK(?)=CONNECTION_ID(),false)`
		if tx != nil {
			e = tx.QueryRowContext(ctx, q, o.mysqlName).Scan(&owns)
		} else {
			e = o.conn.QueryRowContext(ctx, q, o.mysqlName).Scan(&owns)
		}
	} else {
		return ErrConfigInactive
	}
	if e != nil || !owns {
		o.lose()
		return ErrConfigInactive
	}
	return nil
}
func (s *Store) ConfigOwnershipDone() <-chan struct{} {
	if o := s.configOwner.Load(); o != nil {
		return o.done
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (s *Store) WithConfigTransaction(ctx context.Context, fn func(context.Context) error) error {
	if v := s.configTransaction(ctx); v != nil {
		return fn(ctx)
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	o := s.configOwner.Load()
	if o == nil || o.lost.Load() {
		return ErrConfigInactive
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return ErrConfigInactive
	case <-o.sem:
	}
	defer func() {
		o.sem <- struct{}{}
		if o.lost.Load() {
			o.discard()
		}
	}()
	if e := s.verifyConfigOwner(ctx, o, nil); e != nil {
		return e
	}
	tx, e := o.conn.BeginTx(ctx, nil)
	if e != nil {
		o.lose()
		return ErrConfigInactive
	}
	defer tx.Rollback()
	v := &configTransaction{store: s, tx: tx}
	bound := context.WithValue(ctx, configTransactionKey{}, v)
	if e = fn(bound); e != nil {
		return e
	}
	// Verify on the actual transaction connection immediately before commit.
	// Losing this session aborts the transaction before another session can own
	// the lock; the old instance never retries a write through its pooled DB.
	if e = s.verifyConfigOwner(ctx, o, tx); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		o.lose()
		return fmt.Errorf("%w: config commit failed", ErrConfigInactive)
	}
	for _, notify := range v.afterCommit {
		notify()
	}
	return nil
}
func (s *Store) CheckConfigOwnership(ctx context.Context) error {
	return s.WithConfigTransaction(ctx, func(context.Context) error { return nil })
}
func (s *Store) AfterConfigCommit(ctx context.Context, fn func()) error {
	v := s.configTransaction(ctx)
	if v == nil {
		return ErrConfigInactive
	}
	v.afterCommit = append(v.afterCommit, fn)
	return nil
}
func (s *Store) ReleaseConfigOwnership() error {
	o := s.configOwner.Load()
	if o == nil {
		return nil
	}
	o.lose()
	// A session discard (not sql.Conn.Close alone, which returns it to the pool)
	// releases the lock. In-flight config ops are canceled by connection loss;
	// shutdown waits for their slot before handing the connection back.
	select {
	case <-o.sem:
		defer func() { o.sem <- struct{}{} }()
	case <-time.After(5 * time.Second):
		return ErrConfigInactive
	}
	o.discard()
	return nil
}
