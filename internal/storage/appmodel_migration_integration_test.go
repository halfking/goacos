//go:build integration

package storage_test

// 这个文件判的是**两套注册面在同一个二进制下能不能落成同一张 schema**，
// 以及**迁移在真引擎上跑不跑得通**。
//
// ## 为什么要有它
//
// 需求要求「kxpms 与 itestu 是两套同样的系统」。实测：两站的注册中心**引擎不同**
// （252 是 MySQL 8.0，211 是 PostgreSQL 17），表结构不同，能力不同 —— 「同构」在
// 注册中心这一层是 **0%**（`evidence/THE-TWO-REGISTRIES-DIFFER-IN-ENGINE-AGE-AND-SCHEMA…`）。
//
// 代码这一侧本来是打算同构的：基础 schema 有 `.sql` / `.postgres.sql` 两份，
// 增量迁移也有 `.mysql.sql` / `.postgres.sql` **方言孪生**。但**「文件写着两套都有」
// 不等于「在真 MySQL 上跑得通」**：
//
//   - `TestEveryMigrationHasBothDialects` 只断言**两份文件声明同样的表**（静态检查）；
//   - `TestMigrationDialectsDoNotLeak` 只断言**方言词不串**（静态检查）；
//   - PG 那边的真库验证是一条**手工步骤**（`routetable_pg_test.go` 里的注释写着
//     让谁手工 `psql -f …`），**没有自动化**。
//
// ⇒ **MySQL 这一侧从来没有被自动化验证过**，而 252 升级要走的正是这条路径。
// 一次迁移失败会**把生产注册中心停在半套 app model 上** —— 那比不升级更难收拾。
//
// ## 这条判据判什么
//
// 1. **能落**：用**真的** `storage.Open`（它会 ensureSchema + 跑迁移）打开一个
//    空的 MySQL ⇒ 基础 schema + 002/003/004 全部执行成功；
// 2. **落全**：迁移账本 `goacos_schema_version` 里必须有 2/3/4 三行；
// 3. **表齐**：app model 与网关那张表必须真的在（5 张）；
// 4. **索引在**：004 给 `naming_instance` 加的 `idx_instance_sweep` 必须在
//    （MySQL 的 `ALTER TABLE … ADD INDEX` 是 004 里唯一的非幂等写法 ⇒ 特别要证）；
// 5. **两边一样**：MySQL 与 PostgreSQL 落出来的**表集合必须相同**；
// 6. **幂等**：**再打开一次**不能报错、也不能改表集合
//    ⇒ 容器重启时 `Open` 会再跑一遍，这条就是生产重启的安全带。
//
// ## 跑法
//
// 需要两个本地库（**不要指向任何生产库**）：
//
//	docker run -d --name goacos-mysql-it -e MYSQL_ROOT_PASSWORD=goacos_it_pwd_12345 \
//	  -e MYSQL_DATABASE=goacos_it_mysql -p 33306:3306 mysql:8.0
//	docker run -d --name goacos-pg-it -e POSTGRES_PASSWORD=goacos_it_pwd_12345 \
//	  -e POSTGRES_DB=goacos_it_pg -p 35432:5432 postgres:17-alpine
//
//	GOACOS_TEST_MYSQL_DSN='root:goacos_it_pwd_12345@tcp(host.docker.internal:33306)/goacos_it_mysql?parseTime=true' \
//	GOACOS_TEST_PG_DSN='postgres://postgres:goacos_it_pwd_12345@host.docker.internal:35432/goacos_it_pg?sslmode=disable' \
//	go test -tags integration ./internal/storage/... -run TestSchemaMigratesOnBothEngines -count=1
//
// ⚠ **这个文件带 `integration` 标签**：默认的 `go test ./...` 不跑它。
// 但**一旦带上标签，缺 DSN 是硬红，不是跳过** —— 一个「找不到就跳过」的判据
// 会变成恒真，而它要判的恰恰是「能不能跑通」。

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// 迁移必须落到的版本。
//
// 1 是基础 schema（`goacos_meta.schema_version = '1'`），迁移从 2 开始。
// 2 = 应用/组件/路由模型，3 = 网关控制面，4 = 实例清扫索引。
var requiredMigrationVersions = []int{2, 3, 4}

// appModelTables 是 app model 与网关那几张表。
//
// **5 张 + 基础 15 张 + 迁移账本 1 张 = 21**，与 211 实测的 21 张对得上
// （`evidence/THE-TWO-REGISTRIES-DIFFER-IN-ENGINE-AGE-AND-SCHEMA…`）。
// 数字必须带口径：这是**空库走完整迁移**后的表数，不是任意时刻的表数。
var appModelTables = []string{
	"application",
	"application_component",
	"route_binding",
	"gateway_certificate",
	"gateway_vhost",
}

// scratchPrefixes 是这套判据允许的库名前缀。
//
// 硬红而不是跳过：DSN 指错库 ⇒ 这条判据就测的不是它声称在测的东西。
//
// 两套都收：
//   - `goacos_it_`：本轮自建的临时库；
//   - `w038_`：**本仓既有的约定** —— `internal/storage/config_owner_integration_test.go`
//     与 `internal/api/*_integration_test.go` 要求「库名以 w038_ 开头、连回环、
//     库名显式独立」，不满足它们是 `t.Fatal`（不是 skip）。
//
// 收两套是为了让**一套 DSN 能让整个仓的集成判据都跑起来**：
// 否则本仓会出现「同一条 DSN，A 包能跑、B 包拒绝」的分裂，
// 而分裂的代价是有人为了跑通 A 包而绕开 B 包 ⇒ 那些判据继续一次都没跑过。
var scratchPrefixes = []string{"goacos_it_", "w038_"}

func hasScratchPrefix(name string) bool {
	for _, prefix := range scratchPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// localHosts 是允许的连接目标。
//
// 只允许回环与 Docker Desktop 的宿主机别名 ⇒ **一个远端主机名会被拒绝**，
// 这样「误指生产」需要一个有意为之的动作，而不是一次复制粘贴。
var localHosts = map[string]bool{
	"127.0.0.1":            true,
	"localhost":            true,
	"host.docker.internal": true,
}

func TestSchemaMigratesOnBothEngines(t *testing.T) {
	mysqlDSN := requireScratchDSN(t, "GOACOS_TEST_MYSQL_DSN", "mysql")
	pgDSN := requireScratchDSN(t, "GOACOS_TEST_PG_DSN", "postgres")

	ctx := context.Background()

	// Reset both scratch databases first.
	//
	// Without this, a previous run leaves the ledger at version 4 and the
	// migrations never run again — so the next run would "pass" without ever
	// executing them. That is the dirty-baseline trap: the check would stop
	// measuring the thing it claims to measure, and a mutation would look
	// like a catch for the wrong reason.
	//
	// The reset lands on a **derived** database, not on the configured one —
	// see resetScratchDatabase for why the configured one is shared.
	mysqlDSN = resetScratchDatabase(t, mysqlDSN)
	pgDSN = resetScratchDatabasePG(t, pgDSN)

	mysqlTables, mysqlLedger := openAndInspect(t, ctx, "mysql", mysqlDSN)
	pgTables, pgLedger := openAndInspect(t, ctx, "postgres", pgDSN)

	// (2) 迁移账本必须走完 2/3/4。
	for _, ledger := range []struct {
		engine string
		rows   []ledgerRow
	}{
		{"mysql", mysqlLedger},
		{"postgres", pgLedger},
	} {
		got := map[int]bool{}
		for _, row := range ledger.rows {
			got[row.version] = true
		}
		for _, want := range requiredMigrationVersions {
			if !got[want] {
				t.Errorf("%s：迁移账本里没有版本 %d，实际有 %v", ledger.engine, want, ledger.rows)
			}
		}
	}

	// (3) app model 那几张表必须在。
	for _, engine := range []struct {
		name   string
		tables map[string]bool
	}{{"mysql", mysqlTables}, {"postgres", pgTables}} {
		for _, table := range appModelTables {
			if !engine.tables[table] {
				t.Errorf("%s：缺表 %q", engine.name, table)
			}
		}
	}

	// (4) 004 的索引必须在 —— MySQL 那边是 `ALTER TABLE … ADD INDEX`，
	// 是全部迁移里唯一不幂等的写法。
	assertSweepIndex(t, ctx, mysqlDSN)

	// (5) 两个引擎必须落成同一张 schema。
	if diff := tableDiff(mysqlTables, pgTables); diff != "" {
		t.Errorf("两种引擎落出来的表集合不一样：\n%s", diff)
	}

	// (6) 幂等：再打开一次不能报错、不能改表集合。
	// 这一条是生产重启的安全带：goacos 容器每次启动都会重新 Open。
	mysqlTables2, _ := openAndInspect(t, ctx, "mysql", mysqlDSN)
	if diff := tableDiff(mysqlTables, mysqlTables2); diff != "" {
		t.Errorf("第二次 Open 之后表集合变了（迁移不幂等）：\n%s", diff)
	}
}

// TestUpgradeFromV112KeepsExistingRows rehearses the production upgrade.
//
// 252 today is goacos **v1.1.2** on MySQL: 15 tables, no app model, no migration
// ledger. The plan for that stack is "run the current binary against it". This
// test is what that plan looks like when it is rehearsed rather than assumed.
//
// The rehearsal has three steps, and each one exists because the alternative
// is finding out on the production box:
//
//  1. build a database the way **v1.1.2** built it — the base schema only, no
//     ledger, no app model;
//  2. put rows in it that the production database actually has (a naming
//     service, a naming instance, a config row);
//  3. open it with the **current** code and check that the migrations applied
//     and that **the rows are still there**.
//
// Step 1 uses the current base schema on purpose: v1.1.2's base schema is
// byte-identical to the current one (verified 2026-10-24 against the v1.1.2
// tag), so "the base schema" is the same thing in both versions. The part
// that differs — the app model — is exactly what the migrations add.
//
// Step 3 is the point. The migrations only ADD tables and one index; they do
// not touch the existing ones. If that ever stops being true, the upgrade
// starts rewriting production data, and this test is where that shows up.
func TestUpgradeFromV112KeepsExistingRows(t *testing.T) {
	legacyDSN := requireScratchDSN(t, "GOACOS_TEST_MYSQL_LEGACY_DSN", "mysql")
	ctx := context.Background()

	// 1. build the v1.1.2-shaped database.
	legacyDSN = resetScratchDatabase(t, legacyDSN)
	applyBaseSchema(t, legacyDSN)

	// 2. rows the production database actually has.
	seedLegacyRows(t, legacyDSN)

	// 3. open it with the current code.
	cfg := &config.Config{
		DBType:        "mysql",
		DBDSN:         legacyDSN,
		MaxOpenConns:  4,
		MaxIdleConns:  2,
		AuthEnabled:   true,
		AdminUsername: "goacos-it-admin",
		AdminPassword: scratchPassword(t, legacyDSN),
	}
	store, err := storage.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("在 v1.1.2 形状的库上 Open 失败（升级路径走不通）：%v", err)
	}
	defer store.Close()

	// The app model must be there now.
	tables := map[string]bool{}
	for _, table := range listTables(t, store.DB, "mysql") {
		tables[table] = true
	}
	for _, table := range appModelTables {
		if !tables[table] {
			t.Errorf("升级之后缺表 %q", table)
		}
	}
	if !tables["goacos_schema_version"] {
		t.Errorf("升级之后没有迁移账本 goacos_schema_version")
	}

	// The rows must still be there, unchanged.
	assertLegacyRowIntact(t, ctx, legacyDSN,
		"SELECT COUNT(*) FROM naming_service WHERE name = ?", "legacy-svc", 1)
	assertLegacyRowIntact(t, ctx, legacyDSN,
		"SELECT COUNT(*) FROM naming_instance WHERE service_name = ?", "legacy-svc", 1)
	assertLegacyRowIntact(t, ctx, legacyDSN,
		"SELECT COUNT(*) FROM config_info WHERE data_id = ?", "legacy-config", 1)
}

// assertLegacyRowIntact checks that one row survived the upgrade.
func assertLegacyRowIntact(t *testing.T, ctx context.Context, dsn, query, key string, want int) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败：%v", err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRowContext(ctx, query, key).Scan(&got); err != nil {
		t.Fatalf("读旧行失败（表没了？列变了？）：%v", err)
	}
	if got != want {
		t.Errorf("旧行没保住：%s（%s）= %d，应为 %d", query, key, got, want)
	}
}

// resetScratchDatabase drops and recreates a **dedicated** database derived
// from the configured one, and returns the DSN that points at it.
//
// ## Why a derived name, and not the configured one
//
// The configured DSN is **shared**: `internal/storage`, `internal/gateway` and
// `internal/api` all read `GOACOS_TEST_MYSQL_DSN`, and `go test` runs those
// packages **in parallel**. Dropping the configured database therefore does
// not merely clear it — it makes it **disappear**, and a package that was
// halfway through a query then fails with "database does not exist" instead
// of with a wrong answer.
//
// That is not hypothetical: the first version of this criterion dropped the
// configured database, and on that run `internal/api` went red with
// `ping database : pq: database "w038_test" does not exist (3D000)` while
// `internal/gateway` failed alongside it. Both failures were caused by
// another package's teardown, not by either package's own logic.
//
// So this criterion **owns** a database whose name is derived from the
// configured one (`<name>_mig`) and **never** drops the configured one.
//
// The guard in requireScratchDSN still applies to the configured DSN: its name
// must carry the scratch prefix and its host must be loopback. The derived
// name inherits that prefix, so it is inside the same guard rail.
//
// The account needs CREATE/DROP DATABASE rights — it always did; this change
// only moves the blast radius off the shared database.
func resetScratchDatabase(t *testing.T, dsn string) string {
	t.Helper()
	name := scratchDBName(t, dsn) + "_mig"
	db, err := sql.Open("mysql", serverDSN(dsn))
	if err != nil {
		t.Fatalf("打开 MySQL 服务器失败：%v", err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
		t.Fatalf("删临时库 %s 失败：%v", name, err)
	}
	if _, err := db.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatalf("建临时库 %s 失败：%v", name, err)
	}
	return dsnWithDBName(t, dsn, name)
}

// dsnWithDBName returns the same DSN pointing at a different database.
//
// MySQL's DSN form is `user:pass@tcp(host:port)/dbname?params` and
// PostgreSQL's is a URL, so the two need different handling.
func dsnWithDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		t.Fatalf("DSN 里没有库名：%s", redact(dsn))
	}
	rest := dsn[slash+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		return dsn[:slash+1] + name + rest[q:]
	}
	return dsn[:slash+1] + name
}

// applyBaseSchema applies the base schema and nothing else.
//
// This is what "the database as v1.1.2 left it" means: the base tables, no
// migration ledger, no app model.
func applyBaseSchema(t *testing.T, dsn string) {
	t.Helper()
	raw, err := os.ReadFile("schema/schema.sql")
	if err != nil {
		t.Fatalf("读基础 schema 失败：%v", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败：%v", err)
	}
	defer db.Close()
	for _, stmt := range splitScript(string(raw)) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("应用基础 schema 失败：%q: %v", truncate(stmt, 80), err)
		}
	}
}

// seedLegacyRows puts in the kind of rows the production database has.
//
// The naming service and naming instance rows are the ones the empty-shell
// investigation kept landing on; the config row is there because config_info
// is a different table with different columns, and "the migration did not
// touch it" should be checked per table, not per database.
func seedLegacyRows(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败：%v", err)
	}
	defer db.Close()

	stmts := []string{
		"INSERT INTO naming_service (namespace_id, group_name, name, " +
			"protect_threshold, metadata, gmt_create, gmt_modified) " +
			"VALUES ('public','DEFAULT_GROUP','legacy-svc',0.5,'','2026-01-01 00:00:00','2026-01-01 00:00:00')",
		"INSERT INTO naming_instance (namespace_id, group_name, service_name, " +
			"ip, port, healthy, ephemeral, weight, metadata, gmt_create, gmt_modified) " +
			"VALUES ('public','DEFAULT_GROUP','legacy-svc','10.0.0.1',8080,1,0,1.0,''," +
			"'2026-01-01 00:00:00','2026-01-01 00:00:00')",
		"INSERT INTO config_info (data_id, group_id, tenant_id, content, md5, gmt_create, gmt_modified) " +
			"VALUES ('legacy-config','DEFAULT_GROUP','public','legacy-content',MD5('legacy-content')," +
			"'2026-01-01 00:00:00','2026-01-01 00:00:00')",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("写旧行失败：%v", err)
		}
	}
}

// splitScript splits a SQL script into statements, the way the storage layer
// does: comment lines and empty lines are dropped, and a statement ends at a
// semicolon at the end of a line.
func splitScript(script string) []string {
	var out []string
	var cur strings.Builder
	for _, ln := range strings.Split(script, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		cur.WriteString(ln)
		cur.WriteString("\n")
		if strings.HasSuffix(t, ";") {
			if s := strings.TrimSpace(strings.TrimSuffix(cur.String(), ";")); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// resetScratchDatabasePG drops and recreates the PostgreSQL scratch database.
//
// PostgreSQL will not drop the database a session is connected to, so the
// drop goes through the maintenance database instead.
func resetScratchDatabasePG(t *testing.T, dsn string) string {
	t.Helper()
	name := scratchDBName(t, dsn) + "_mig"
	db, err := sql.Open("postgres", serverDSN(dsn))
	if err != nil {
		t.Fatalf("打开 PostgreSQL 服务器失败：%v", err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS " + pgQuoteIdent(name)); err != nil {
		t.Fatalf("删临时库 %s 失败：%v", name, err)
	}
	if _, err := db.Exec("CREATE DATABASE " + pgQuoteIdent(name)); err != nil {
		t.Fatalf("建临时库 %s 失败：%v", name, err)
	}
	return dsnWithDBName(t, dsn, name)
}

// pgQuoteIdent quotes a PostgreSQL identifier.
//
// The name comes from the DSN, and the guard in requireScratchDSN pins its
// prefix — but quoting is what makes an identifier safe to interpolate, and
// an interpolated identifier is only safe when it is quoted.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// scratchDBName returns the database name of a DSN (MySQL or PostgreSQL).
func scratchDBName(t *testing.T, dsn string) string {
	t.Helper()
	var name string
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		name = strings.TrimPrefix(u.Path, "/")
	} else {
		slash := strings.LastIndex(dsn, "/")
		if slash < 0 {
			t.Fatal("DSN 里没有库名")
		}
		name = dsn[slash+1:]
	}
	if q := strings.Index(name, "?"); q >= 0 {
		name = name[:q]
	}
	return name
}

// serverDSN returns a DSN that points at the server rather than at a database.
//
// For PostgreSQL that means the maintenance database; for MySQL it means no
// database at all, which is how CREATE/DROP DATABASE is issued.
func serverDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/postgres"
		return u.String()
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1]
}

// truncate shortens a statement for an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ledgerRow is one row of the migration ledger.
type ledgerRow struct {
	version int
	name    string
}

// openAndInspect opens a scratch database through the real storage.Open —
// which is what applies the base schema and every pending migration — and
// then reads back what actually landed.
func openAndInspect(t *testing.T, ctx context.Context, engine, dsn string) (map[string]bool, []ledgerRow) {
	t.Helper()

	cfg := &config.Config{
		DBType:        engine,
		DBDSN:         dsn,
		MaxOpenConns:  4,
		MaxIdleConns:  2,
		AuthEnabled:   true,
		AdminUsername: "goacos-it-admin",
		AdminPassword: scratchPassword(t, dsn),
	}
	store, err := storage.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("%s：Open 失败（基础 schema 或迁移没跑通）：%v", engine, err)
	}
	t.Cleanup(func() { store.Close() })

	tables := map[string]bool{}
	for _, table := range listTables(t, store.DB, engine) {
		tables[table] = true
	}

	var rows []ledgerRow
	ledger, err := store.DB.QueryContext(ctx, "SELECT version, name FROM goacos_schema_version ORDER BY version")
	if err != nil {
		t.Fatalf("%s：读迁移账本失败（账本表都没建出来）：%v", engine, err)
	}
	defer ledger.Close()
	for ledger.Next() {
		var v int
		var name string
		if err := ledger.Scan(&v, &name); err != nil {
			t.Fatalf("%s：读迁移账本行失败：%v", engine, err)
		}
		rows = append(rows, ledgerRow{version: v, name: name})
	}
	if err := ledger.Err(); err != nil {
		t.Fatalf("%s：迁移账本读取出错：%v", engine, err)
	}
	return tables, rows
}

// listTables reads the table set of the connected database.
func listTables(t *testing.T, db *sql.DB, engine string) []string {
	t.Helper()
	var query string
	switch engine {
	case "mysql":
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()"
	case "postgres":
		query = "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"
	default:
		t.Fatalf("未知引擎 %q", engine)
	}
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s：读表清单失败：%v", engine, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("%s：读表名失败：%v", engine, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s：读表清单出错：%v", engine, err)
	}
	sort.Strings(out)
	return out
}

// assertSweepIndex checks migration 004's index on the MySQL side.
//
// 004 is the one migration whose MySQL spelling is `ALTER TABLE … ADD INDEX` —
// which fails if the index is already there. The ledger keeps it from running
// twice, and that is exactly the property worth proving on the engine that
// will actually run it.
func assertSweepIndex(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败：%v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"SELECT index_name FROM information_schema.statistics "+
			"WHERE table_schema = DATABASE() AND table_name = 'naming_instance'")
	if err != nil {
		t.Fatalf("读 naming_instance 的索引失败：%v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读索引名失败：%v", err)
		}
		if name == "idx_instance_sweep" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读索引出错：%v", err)
	}
	if !found {
		t.Errorf("MySQL：naming_instance 上没有 idx_instance_sweep ⇒ 迁移 004 没落下来")
	}
}

// tableDiff renders the difference between two table sets.
func tableDiff(a, b map[string]bool) string {
	var missing, extra []string
	for name := range a {
		if !b[name] {
			missing = append(missing, name)
		}
	}
	for name := range b {
		if !a[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	return "  只在左边: " + strings.Join(missing, ", ") + "\n  只在右边: " + strings.Join(extra, ", ")
}

// requireScratchDSN reads a DSN from the environment and refuses anything that
// is not obviously a scratch database.
//
// Hard red, not a skip: a DSN that points somewhere else would make this
// criterion measure the wrong database — and the failure mode it guards is
// exactly the one that would damage a real one.
func requireScratchDSN(t *testing.T, key, engine string) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(key))
	if dsn == "" {
		t.Fatalf("%s 未设置：这条判据需要一个**本地临时库**，它要判的正是「迁移跑不跑得通」\n"+
			"（跑法见本文件顶部的注释）。用跳过代替硬红，会让这条判据变成恒真。", key)
	}
	if engine == "mysql" {
		// user:pass@tcp(host:port)/dbname?params
		at := strings.LastIndex(dsn, "@")
		slash := strings.LastIndex(dsn, "/")
		if at < 0 || slash < 0 || slash < at {
			t.Fatalf("%s 不是合法的 MySQL DSN：%s", key, redact(dsn))
		}
		hostPort := dsn[at+1:]
		if tcp := strings.Index(hostPort, "("); tcp >= 0 {
			if close := strings.Index(hostPort, ")"); close > tcp {
				hostPort = hostPort[tcp+1 : close]
			}
		}
		host := hostPort
		if colon := strings.Index(hostPort, ":"); colon >= 0 {
			host = hostPort[:colon]
		}
		if !localHosts[host] {
			t.Fatalf("%s 指向了 %q：只允许回环或 host.docker.internal", key, host)
		}
		dbName := dsn[slash+1:]
		if q := strings.Index(dbName, "?"); q >= 0 {
			dbName = dbName[:q]
		}
		if !hasScratchPrefix(dbName) {
			t.Fatalf("%s 的库名 %q 不以 %v 之一开头：这条判据只允许临时库", key, dbName, scratchPrefixes)
		}
		return dsn
	}

	// postgres://user:pass@host:port/db?params
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("%s 不是合法的 URL：%s", key, redact(dsn))
	}
	if !localHosts[u.Hostname()] {
		t.Fatalf("%s 指向了 %q：只允许回环或 host.docker.internal", key, u.Hostname())
	}
	if !hasScratchPrefix(strings.TrimPrefix(u.Path, "/")) {
		t.Fatalf("%s 的库名 %q 不以 %v 之一开头：这条判据只允许临时库",
			key, strings.TrimPrefix(u.Path, "/"), scratchPrefixes)
	}
	return dsn
}

// scratchPassword pulls the password out of the DSN so the admin account can
// be created with the same one. It is never printed.
func scratchPassword(t *testing.T, dsn string) string {
	t.Helper()
	if u, err := url.Parse(dsn); err == nil && u.Scheme == "postgres" {
		if pw, ok := u.User.Password(); ok {
			return pw
		}
	}
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		creds := dsn[:at]
		if colon := strings.Index(creds, ":"); colon >= 0 {
			return creds[colon+1:]
		}
	}
	t.Fatal("DSN 里找不到口令")
	return ""
}

// redact hides the credentials of a DSN before it is printed.
func redact(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User("redacted")
		return u.String()
	}
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		return "redacted@" + dsn[at+1:]
	}
	return "redacted"
}
