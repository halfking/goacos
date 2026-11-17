//go:build integration

package storage_test

// The admin seed: it must never create an account nobody can use.
//
// ## What went wrong
//
// `Store.Seed` guards the admin insert with "the users table is empty".
// That makes the very first row it writes decide the fate of the database:
// if that row's username is the empty string, the real admin is never seeded,
// and it never will be — the table is no longer empty.
//
// A goacos started without `ADMIN_USERNAME` did exactly that. And nothing
// said so: `ValidateAuthState` only checks that no *active* admin still has
// the public password, not that an admin exists at all.
//
// It was not found by reading the code. It was found because a test that
// opened a shared registry database with a **zero-valued config** (no
// `AdminUsername`) left a row with an empty username behind, and the next
// package to open the same database could no longer log in — twenty of its
// criteria failed with "unknown user or wrong password", and **none of those
// twenty was wrong**.
//
// ## What this criterion pins
//
//  1. an empty `AdminUsername` must not create a user row at all;
//  2. a configured `AdminUsername` must still be seeded on an empty database,
//     with a password that actually authenticates.
//
// ⚠ It deliberately does **not** pin the policy question — "seed the admin
// when the table is empty" versus "seed the admin when that admin is
// missing". The second is a security-relevant choice (it would re-create an
// admin that an operator deleted on purpose), so it is the owner's call, not
// this file's. See the decision sheet.
//
// Both engines run it: MySQL is the engine 252 runs, PostgreSQL the one 211
// runs, and the seeding path differs in how the DSN is handled.

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

const seedProbePassword = "seed-probe-password-12345"

// countUsers counts the rows of the users table whose username is exactly
// the given value.
//
// It goes through the store rather than a raw *sql.DB because the store
// rewrites the `?` placeholders for the dialect — PostgreSQL does not accept
// them.
func countUsers(t *testing.T, store *storage.Store, username string) int {
	t.Helper()
	var n int
	if err := store.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM users WHERE username=?", username).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// TestSeedNeverCreatesAnEmptyAdmin is the criterion for the defect above.
//
// The first half is the regression guard: an Open with no `AdminUsername`
// must leave the users table **empty**, because the seeding decision it
// makes is irreversible — the real admin is only seeded while the table is
// empty.
//
// The second half is the positive control: the fix must not have broken the
// normal path. A configured admin is still seeded, with a usable password.
func TestSeedNeverCreatesAnEmptyAdmin(t *testing.T) {
	ctx := context.Background()

	for _, engine := range []struct {
		name  string
		dsn   func(t *testing.T) string
		count func(t *testing.T, store *storage.Store, username string) int
	}{
		{
			name: "mysql",
			dsn: func(t *testing.T) string {
				return resetScratchDatabase(t, requireScratchDSN(t, "GOACOS_TEST_MYSQL_DSN", "mysql"))
			},
			count: countUsers,
		},
		{
			name: "postgres",
			dsn: func(t *testing.T) string {
				return resetScratchDatabasePG(t, requireScratchDSN(t, "GOACOS_TEST_PG_DSN", "postgres"))
			},
			count: countUsers,
		},
	} {
		engine := engine
		t.Run(engine.name, func(t *testing.T) {
			dsn := engine.dsn(t)

			quiet := &config.Config{
				DBType: engine.name, DBDSN: dsn,
				MaxOpenConns: 4, MaxIdleConns: 2,
				AuthEnabled: true, AdminPassword: seedProbePassword,
				// AdminUsername is intentionally empty: this is the shape that
				// poisoned a shared registry.
			}
			store, err := storage.Open(ctx, quiet)
			if err != nil {
				t.Fatalf("Open without ADMIN_USERNAME: %v", err)
			}
			defer store.Close()

			if n := engine.count(t, store, ""); n != 0 {
				t.Errorf("没有 ADMIN_USERNAME 也能播种出 %d 行空用户名用户 —— "+
					"这一行会让真正的 admin 此后永远播种不出来（表不再为空）", n)
			}

			admin := &config.Config{
				DBType: engine.name, DBDSN: dsn,
				MaxOpenConns: 4, MaxIdleConns: 2,
				AuthEnabled: true, AdminUsername: "seeded-admin",
				AdminPassword: seedProbePassword,
			}
			store2, err := storage.Open(ctx, admin)
			if err != nil {
				t.Fatalf("Open with ADMIN_USERNAME: %v", err)
			}
			defer store2.Close()

			u, err := store2.GetUser(ctx, "seeded-admin")
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if u == nil {
				t.Fatal("空库上配了 ADMIN_USERNAME 却没有播种出 admin —— 登录会失败")
			}
			if err := bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(seedProbePassword)); err != nil {
				t.Errorf("播种出来的 admin 口令不对：%v", err)
			}
		})
	}
}
