#!/usr/bin/env bash
# Mutation check for the app-model migration path AND the runtime path.
#
# It guards two families of criteria:
#
#   internal/storage/appmodel_migration_integration_test.go
#     - TestSchemaMigratesOnBothEngines — the schema and every pending
#       migration must apply on BOTH engines (MySQL 8, the engine 252 runs,
#       and PostgreSQL, the engine 211 runs), land the same table set, and be
#       idempotent;
#     - TestUpgradeFromV112KeepsExistingRows — the production upgrade (a
#       v1.1.2 shaped database) must keep the rows it already had.
#
#   internal/gateway/*_db_test.go
#     - TestRefreshRoutesToWhatTheRegistryActuallyHolds
#     - TestRefreshKeepsInstancesApartByNamespace
#     - TestStaleEphemeralInstanceLeavesGatewayRouting
#     - TestStalePersistentInstanceLeavesGatewayRouting
#     - TestRegisteredInstancesShareTheTraffic
#     - TestRegisteredWeightsReachTheBalancer
#     - TestDeclaredAlgorithmReachesThePicker
#     - TestDeclaredLeastConnAvoidsTheBusyInstance
#
# A criterion that cannot fail proves nothing. This script mutates the
# migration files, the migration runner, the storage layer and the fixture,
# one mutation at a time, and requires that a NAMED criterion goes red for
# each one. A mutation nobody catches is reported as a survival and fails
# the run.
#
# The mutations are the realistic ways this path breaks:
#   * a table added to one dialect and forgotten in the other;
#   * the PostgreSQL spelling of "add an index" copied into the MySQL twin
#     (MySQL 8 has no CREATE INDEX IF NOT EXISTS);
#   * the migration ledger not being written, so every container restart
#     replays the migrations (and the non-idempotent one then fails);
#   * the fixture losing the heartbeat timeout — the exact breakage that
#     left this whole subsystem unexecuted for months;
#   * the instance query forgetting the namespace, the health re-derivation,
#     or the healthy predicate.
#
# ## Equivalence probes
#
# One mutation is deliberately NOT expected to be caught: M8 widens the SQL
# healthy predicate to a no-op. It is recorded as an **equivalent probe**,
# not as a caught mutation, and the harness proves the equivalence instead of
# asserting it: the named criterion has to stay green under the mutation.
#
# The equivalence is real and it is worth knowing: ListInstances applies the
# healthy filter **twice** — once in SQL (`healthy=1`) and once in Go
# (`if q.HealthyOnly && !i.Healthy { continue }`). The SQL half is redundant;
# the Go half is the only load-bearing gate. M7 and M9 break the Go half and
# are caught; M8 breaks the SQL half and is not, and that pairing is the
# evidence for which half is real.
#
# An equivalent probe is not a loophole: the harness still refuses to count it
# as a catch, still requires the source to have actually changed, still
# requires the named criterion to have actually run (a skip is not a green),
# and still prints it in the summary. It is a claim with a proof attached,
# not a way to make the run pass.
#
# Usage:
#   bash scripts/mutation-check-appmodel-migration.sh
#   SELF_TEST=1 bash scripts/mutation-check-appmodel-migration.sh
#
# Needs a Go toolchain, perl, and scratch databases. The databases are
# dropped and recreated by the criterion itself, so they must be scratch
# databases and nothing else:
#
#   docker run -d --name goacos-mysql-it -e MYSQL_ROOT_PASSWORD=goacos_it_pwd_12345 \
#     -e MYSQL_ROOT_HOST=% -e MYSQL_DATABASE=w038_test -p 33306:3306 mysql:8.0
#   docker run -d --name goacos-pg-it -e POSTGRES_PASSWORD=goacos_it_pwd_12345 \
#     -e POSTGRES_DB=w038_test -p 35432:5432 postgres:17-alpine
#
# Only `w038_test` has to exist. The databases the criteria **destroy** are
# derived from it (`w038_test_mig`, `w038_test_gw`) and are created by the
# criteria themselves, so nothing outside the two configured databases is
# touched.
#
#   GOACOS_TEST_MYSQL_DSN='root:goacos_it_pwd_12345@tcp(127.0.0.1:33306)/w038_test?parseTime=true' \
#   GOACOS_TEST_PG_DSN='postgres://postgres:goacos_it_pwd_12345@127.0.0.1:35432/w038_test?sslmode=disable' \
#   GOACOS_TEST_MYSQL_LEGACY_DSN='root:goacos_it_pwd_12345@tcp(127.0.0.1:33306)/w038_legacy?parseTime=true' \
#   bash scripts/mutation-check-appmodel-migration.sh
#
# ## Why the DSNs must look like THIS
#
# The database names and the loopback address are not decoration. Other
# criteria in this repository **refuse** anything else, with `t.Fatal` (not a
# skip):
#
#   internal/storage/config_owner_integration_test.go
#     - MySQL: DB name must start with `w038_`, the address must start with
#       `127.0.0.1:`, and the password must be at least 12 characters;
#   internal/storage/migration_sweep_index_test.go
#     - PostgreSQL: the address must be `127.0.0.1`/`localhost` and the
#       database name must contain `test`;
#   internal/api/*_integration_test.go
#     - PostgreSQL: the address must be loopback and the database name must
#       start with `w038_`.
#
# Those are deliberate self-guards — a criterion that can assert against a real
# registry is worse than no criterion. But a guard that nothing satisfies is
# a criterion that never runs, and this repository had **three** of them
# (two in `internal/storage`, one in `internal/api`) which could only be
# satisfied by a fixture nobody had built.
#
# So the harness uses the repository's own convention (`w038_*`, loopback) and
# asks for `--network host` on the Go container, which is what makes
# `127.0.0.1` inside the container reach the published port on the host.
# With that, one set of DSNs runs **the whole repository's** integration
# criteria, instead of a private subset.
#
# Deliberately NOT wired into `make test`: it needs two databases, and it is a
# checker of the checker.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/goacos-appmodel-mut.XXXXXX")"

HARNESS_FINISHED=0
EXIT_RC=0

cleanup() {
  if [ -d "$WORK" ]; then
    find "$WORK" -mindepth 1 -delete 2>/dev/null
    rmdir "$WORK" 2>/dev/null
  fi
}

# A harness that dies half way through proves nothing — and the worst version
# of that is one that dies *quietly* and leaves the caller reading the last
# line it printed. This trap turns any early exit into an explicit failure.
on_exit() {
  EXIT_RC=$?
  if [ "$HARNESS_FINISHED" -ne 1 ]; then
    echo
    echo "FATAL: the harness stopped before its summary (exit code $EXIT_RC)."
    echo "       A checker of the checker that does not finish checks nothing —"
    echo "       treat this run as FAILED, not as 'no failures found'."
    cleanup
    exit 1
  fi
  cleanup
}
trap on_exit EXIT

if ! command -v "$GO" >/dev/null 2>&1; then
  cat >&2 <<'EOF'
FATAL: no Go toolchain found (looked for '$GO').
Run this inside the golang image.
EOF
  exit 2
fi

if ! command -v perl >/dev/null 2>&1; then
  echo "FATAL: perl is required for the multiline edits." >&2
  exit 2
fi

for key in GOACOS_TEST_MYSQL_DSN GOACOS_TEST_PG_DSN GOACOS_TEST_MYSQL_LEGACY_DSN; do
  if [ -z "${!key:-}" ]; then
    cat >&2 <<EOF
FATAL: $key is not set.
This harness needs two scratch databases — the criterion drops and recreates
them, so they must be scratch databases and nothing else.
See the header of this script for the docker commands.
EOF
    exit 2
  fi
done

# ---------------------------------------------------------------- fixture ---
# The whole module is copied: the criterion lives in a package that builds
# against the rest of the module, and a partial copy would not compile.
mkdir -p "$WORK/src"
cp -R "$ROOT/." "$WORK/src/" 2>/dev/null
rm -rf "$WORK/src/.git" "$WORK/src/scripts"
cp "$ROOT/scripts/mutation-check-appmodel-migration.sh" "$WORK/src/" 2>/dev/null || true

TOTAL=0
FAILED=0
CAUGHT=0
EQUIV=0

# Every file any mutation touches. The pristine copy of each is taken once,
# and every mutation restores ALL of them — a mutation that is not undone
# leaks into the next run, and a mutation check that cannot tell whose change
# it is measuring measures nothing.
MUTATED_FILES="
internal/storage/schema/migrations/002_application.mysql.sql
internal/storage/schema/migrations/003_gateway.mysql.sql
internal/storage/schema/migrations/004_instance_sweep.mysql.sql
internal/storage/migrate.go
internal/storage/namingstore.go
internal/storage/store.go
internal/gateway/dbtest_test.go
internal/gateway/lb.go
internal/gateway/routetable.go
"

restore_pristine() {
  for f in $MUTATED_FILES; do
    if [ -f "$WORK/src/$f.pristine" ]; then
      cp "$WORK/src/$f.pristine" "$WORK/src/$f"
    fi
  done
}

# run_suite_test <suite> <test-name> — runs one named criterion and echoes
# its output. suite is "storage" or "gateway".
run_suite_test() {
  local suite="$1" expect="$2"
  case "$suite" in
    storage)
      (cd "$WORK/src" && "$GO" test -tags integration ./internal/storage/... \
        -count=1 -run "^${expect}\$" 2>&1)
      ;;
    gateway)
      (cd "$WORK/src" && "$GO" test ./internal/gateway/ \
        -count=1 -run "^${expect}\$" 2>&1)
      ;;
    *)
      echo "unknown suite $suite" >&2
      return 3
      ;;
  esac
}

# apply_mutation <file> <perl-expression> — applies the mutation and proves
# it changed the source. A mutation that changes nothing would make the whole
# run meaningless, so a no-op is a hard error, never a silent pass.
apply_mutation() {
  local file="$1" expr="$2" src="$WORK/src/$1"
  if ! perl -0pi -e "$expr" "$src"; then
    return 1
  fi
  if cmp -s "$src" "$WORK/src/$file.pristine"; then
    return 2
  fi
  return 0
}

# run_mutation <id> <file> <description> <expected-test> <suite> <perl-expression>
#
# Verifies three things, in this order:
#   1. the mutation actually changed the source;
#   2. the named test went red;
#   3. the named test — not some other test — is what went red.
#
# Exit codes: 0 caught, 1 survived (nobody caught it), 2 something else.
run_mutation() {
  local id="${1:-}" file="${2:-}" desc="${3:-}" expect="${4:-}" suite="${5:-}" expr="${6:-}"
  local out rc
  TOTAL=$((TOTAL + 1))

  if [ -z "$id" ] || [ -z "$file" ] || [ -z "$desc" ] || [ -z "$expect" ] ||
     [ -z "$suite" ] || [ -z "$expr" ]; then
    printf '  [%-4s] ERROR    run_mutation needs 6 arguments: id file desc expect suite expr\n' "${id:-?}"
    FAILED=$((FAILED + 1))
    return 2
  fi

  restore_pristine

  local src="$WORK/src/$file"
  perl -0pi -e "$expr" "$src"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '  [%-4s] ERROR    perl failed\n' "$id"
    FAILED=$((FAILED + 1))
    return 2
  fi
  if cmp -s "$src" "$WORK/src/$file.pristine"; then
    printf '  [%-4s] ERROR    mutation did not change the source — the run would count a no-op\n' "$id"
    FAILED=$((FAILED + 1))
    return 2
  fi

  out="$(run_suite_test "$suite" "$expect")"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '  [%-4s] SURVIVED %s\n            (expected %s to go red)\n' "$id" "$desc" "$expect"
    FAILED=$((FAILED + 1))
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q -- "--- FAIL: ${expect}"; then
    printf '  [%-4s] WRONG    %s\n            (expected %s to go red; something else did)\n' "$id" "$desc" "$expect"
    printf '%s\n' "$out" | sed 's/^/            /' | head -25
    FAILED=$((FAILED + 1))
    return 2
  fi

  printf '  [%-4s] caught   %s\n            (by %s)\n' "$id" "$desc" "$expect"
  CAUGHT=$((CAUGHT + 1))
  return 0
}

# run_probe_equivalent <id> <file> <description> <expected-test> <suite> <perl-expression>
#
# The mirror image of run_mutation: the named criterion has to STAY GREEN
# under the mutation. That is the proof of the equivalence claim — it is not
# taken on faith.
#
# A skip is not a green: if the criterion did not run, the probe proves
# nothing, and the harness says so.
run_probe_equivalent() {
  local id="${1:-}" file="${2:-}" desc="${3:-}" expect="${4:-}" suite="${5:-}" expr="${6:-}"
  local out rc
  TOTAL=$((TOTAL + 1))

  if [ -z "$id" ] || [ -z "$file" ] || [ -z "$desc" ] || [ -z "$expect" ] ||
     [ -z "$suite" ] || [ -z "$expr" ]; then
    printf '  [%-4s] ERROR    run_probe_equivalent needs 6 arguments: id file desc expect suite expr\n' "${id:-?}"
    FAILED=$((FAILED + 1))
    return 2
  fi

  restore_pristine

  local src="$WORK/src/$file"
  perl -0pi -e "$expr" "$src"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '  [%-4s] ERROR    perl failed\n' "$id"
    FAILED=$((FAILED + 1))
    return 2
  fi
  if cmp -s "$src" "$WORK/src/$file.pristine"; then
    printf '  [%-4s] ERROR    probe did not change the source — it would prove nothing\n' "$id"
    FAILED=$((FAILED + 1))
    return 2
  fi

  out="$(run_suite_test "$suite" "$expect")"
  rc=$?
  if printf '%s\n' "$out" | grep -q -- "--- SKIP"; then
    printf '  [%-4s] ERROR    the criterion skipped, so the equivalence claim is unproven\n' "$id"
    FAILED=$((FAILED + 1))
    return 2
  fi
  if [ "$rc" -ne 0 ]; then
    printf '  [%-4s] NOT-EQUIV %s\n            (the criterion went red, so this is NOT an equivalent mutation)\n' "$id" "$desc"
    printf '%s\n' "$out" | sed 's/^/            /' | head -25
    FAILED=$((FAILED + 1))
    return 2
  fi

  printf '  [%-4s] equivalent %s\n            (proved: %s stayed green — recorded, not counted as a catch)\n' \
    "$id" "$desc" "$expect"
  EQUIV=$((EQUIV + 1))
  return 0
}

echo "== mutation check: app-model migration path + runtime path (MySQL + PostgreSQL) =="
echo "   workdir: $WORK"
echo

# Save the pristine copies of every file the mutations touch.
for f in $MUTATED_FILES; do
  cp "$WORK/src/$f" "$WORK/src/$f.pristine"
done

# ------------------------------------------------------------- baseline ---
# The harness measures differences, so it needs a green starting point. A red
# baseline would turn every later "caught" into a coincidence.
echo "== baseline: the criteria must be green before any mutation =="
base_out="$(cd "$WORK/src" && "$GO" test -tags integration ./internal/storage/... -count=1 2>&1)"
base_rc=$?
base_out2="$(cd "$WORK/src" && "$GO" test ./internal/gateway/ -count=1 2>&1)"
base_rc2=$?
if [ "$base_rc" -ne 0 ] || [ "$base_rc2" -ne 0 ]; then
  echo "  [base] FAIL: the criteria are not green before any mutation is applied."
  printf '%s\n' "$base_out" | grep -- "--- FAIL" | sed 's/^/            /' | head -10
  printf '%s\n' "$base_out2" | grep -- "--- FAIL" | sed 's/^/            /' | head -10
  echo "FATAL: a mutation check on a red baseline measures nothing."
  exit 1
fi
if printf '%s\n' "$base_out2" | grep -q -- "--- SKIP"; then
  echo "  [base] FAIL: a database-backed criterion skipped even though both DSNs were set."
  printf '%s\n' "$base_out2" | grep -- "--- SKIP" | sed 's/^/            /' | head -10
  exit 1
fi
echo "  [base] OK: the criteria are green"
echo

# ------------------------------------------------------------- mutations ---
# M1: a table exists in the PostgreSQL twin and is forgotten in the MySQL one.
# This is the failure the two-twin rule exists to catch, and it is invisible
# to a unit test that never opens a MySQL.
run_mutation M1 internal/storage/schema/migrations/002_application.mysql.sql \
  "the MySQL twin of the application migration loses route_binding" \
  TestSchemaMigratesOnBothEngines storage \
  's@CREATE TABLE IF NOT EXISTS route_binding \(.*?;\n@@s'

# M2: same shape, in the gateway migration.
run_mutation M2 internal/storage/schema/migrations/003_gateway.mysql.sql \
  "the MySQL twin of the gateway migration loses gateway_vhost" \
  TestSchemaMigratesOnBothEngines storage \
  's@CREATE TABLE IF NOT EXISTS gateway_vhost \(.*?;\n@@s'

# M3: the PostgreSQL spelling of "add an index" copied into the MySQL twin.
# MySQL 8 has no CREATE INDEX IF NOT EXISTS, so the migration fails there —
# and 004 is the migration the production upgrade would run.
run_mutation M3 internal/storage/schema/migrations/004_instance_sweep.mysql.sql \
  "the MySQL twin of the sweep-index migration uses the PostgreSQL index spelling" \
  TestSchemaMigratesOnBothEngines storage \
  's@ALTER TABLE naming_instance ADD INDEX idx_instance_sweep \([^;]*\);@CREATE INDEX IF NOT EXISTS idx_instance_sweep ON naming_instance (ip);@'

# M4: the migration ledger records the wrong version. Every container restart
# then replays the migrations, and the non-idempotent one (004's ALTER) fails
# on the second boot. This is the production restart safety belt.
run_mutation M4 internal/storage/migrate.go \
  "the migration ledger records the baseline version instead of the applied one" \
  TestSchemaMigratesOnBothEngines storage \
  's@"INSERT INTO goacos_schema_version \(version, name\) VALUES \(\?, \?\)"@"INSERT INTO goacos_schema_version (version, name) VALUES (1, ?)"@'

# --- the runtime path (declare → component → route → instance → dispatch) ---
#
# The mutations below are the ones that make the data plane lie:
# an instance that is not healthy still gets traffic, one namespace's
# instances show up under another namespace's route, and the fixture stops
# being able to represent a live instance at all.
#
# M5: the shared fixture loses its heartbeat timeout.
#
# This is the historical breakage, reproduced on purpose: a zero-valued
# config makes every ephemeral instance read back unhealthy, and then every
# route resolves to no instances — which is exactly what happened to these
# tests the day they were first run.
run_mutation M5 internal/gateway/dbtest_test.go \
  "the test fixture loses its heartbeat timeout (the historical breakage)" \
  TestRefreshRoutesToWhatTheRegistryActuallyHolds gateway \
  's@\t\tHeartbeatTimeoutMs: testHeartbeatTimeoutMs,\n@@'

# M6: the namespace filter is dropped from the instance query.
#
# This is the multi-stack isolation property: kxpms and itestu are two
# namespaces of one registry, and a query that forgets the namespace hands
# one stack's traffic to the other.
run_mutation M6 internal/storage/namingstore.go \
  "the instance query stops filtering by namespace" \
  TestRefreshKeepsInstancesApartByNamespace gateway \
  's@where := \[\]string\{"namespace_id=\?", "group_name=\?", "service_name=\?"\}@where := []string{"group_name=?", "service_name=?"}@; s@args := \[\]any\{q\.NamespaceId, q\.GroupName, q\.ServiceName\}@args := []any{q.GroupName, q.ServiceName}@'

# M7: the Go-level health filter stops filtering.
#
# ListInstances applies the healthy filter twice: once in SQL (`healthy=1`)
# and once in Go (`if q.HealthyOnly && !i.Healthy { continue }`). This
# mutation removes the Go half.
#
# The gateway does not look at `Healthy` anywhere else — `grep -rn Healthy
# internal/gateway/ --include=*.go` returns exactly one hit, the
# `HealthyOnly: true` argument itself — so if this filter stops dropping
# rows, nothing downstream will: the gateway keeps sending traffic to a host
# that stopped answering.
run_mutation M7 internal/storage/namingstore.go \
  "the instance query stops dropping unhealthy rows at the read boundary" \
  TestStaleEphemeralInstanceLeavesGatewayRouting gateway \
  's@\t\tif q\.HealthyOnly && !i\.Healthy \{\n\t\t\tcontinue\n\t\t\}\n@@'

# M8: the SQL healthy predicate stops filtering — an EQUIVALENCE PROBE.
#
# The SQL half of the healthy filter is redundant: the Go half (M7) drops
# the same rows. This probe proves that rather than asserting it — the named
# criterion has to stay green under the mutation.
#
# It is deliberately `1=1` and not `healthy=1 OR 1=1`: in SQL, AND binds
# tighter than OR, so `... AND healthy=1 OR 1=1` would evaluate as
# `(namespace AND group AND service AND healthy) OR TRUE` and silently widen
# the **namespace** scoping too. That is a real trap for whoever edits this
# query next, but it is a different defect, and M6 already covers the
# namespace filter directly.
#
# The pairing M7 + M8 is the evidence for which half of the healthy filter
# is load-bearing: break the Go half and a named criterion goes red; break
# the SQL half and nothing does.
run_probe_equivalent M8 internal/storage/namingstore.go \
  "the SQL healthy predicate stops filtering (equivalent: the Go filter already drops those rows)" \
  TestStaleEphemeralInstanceLeavesGatewayRouting gateway \
  's@where = append\(where, "healthy=1"\)@where = append(where, "1=1")@'

# M9: the health re-derivation itself is broken.
#
# `effectiveInstanceHealth` re-derives an ephemeral instance's health from
# its heartbeat at the read boundary. If it stops doing that, every instance
# that stopped beating stays in rotation until the sweeper catches up — and
# the sweeper runs every SWEEP_INTERVAL_MS (5s by default), so the window
# is short but real.
run_mutation M9 internal/storage/namingstore.go \
  "the heartbeat re-derivation at the read boundary stops rejecting stale instances" \
  TestStaleEphemeralInstanceLeavesGatewayRouting gateway \
  's@return nowMS-i\.LastHeartbeatMS <= s\.cfg\.HeartbeatTimeoutMs@return true@'

# M10: the component's declared algorithm stops reaching the route target.
#
# `Target.LBAlgorithm` is what the data plane hands to the picker
# (`proxy.go`: `picker.Pick(key, t.LBAlgorithm, …)`), so losing it here means
# every component silently falls back to round robin — an ip_hash frontend
# would stop pinning a client to one instance, and that is the failure this
# algorithm exists to prevent.
run_mutation M10 internal/gateway/routetable.go \
  "the declared load-balancing algorithm stops reaching the route target" \
  TestDeclaredAlgorithmReachesThePicker gateway \
  's@(LBAlgorithm:\s+)comp\.LBAlgorithm,@$1"",@'

# M11: the instance weight stops mattering.
#
# The weight a registering client wrote into `naming_instance.weight` has to
# survive the trip through `scanInstance` → `weightOf` → the picker. This
# mutation cuts it at the last hop: every instance looks equally weighted, so
# a 3:1 registration comes out 1:1.
run_mutation M11 internal/gateway/lb.go \
  "the instance weight stops mattering to the balancer" \
  TestRegisteredWeightsReachTheBalancer gateway \
  's@return i\.Weight\n\}@return 1\n}@'

# M12: the balancer stops rotating.
#
# "按当前可用来进行均衡调度" collapses into "always the first instance" — the
# registry's instance set is still right, and the traffic still flows, but it
# stops being balanced. The criterion has to notice the **distribution**, not
# just the set, or this failure is invisible.
run_mutation M12 internal/gateway/lb.go \
  "the balancer always picks the first instance" \
  TestRegisteredInstancesShareTheTraffic gateway \
  's@\ts\.current\[addr\(instances\[best\]\)\] -= total@\tbest = 0\n\ts.current[addr(instances[best])] -= total@'

# M13: the admin seed stops refusing an empty admin username.
#
# The seed is guarded by "the users table is empty", so the first row it
# writes decides the fate of the database forever: a row whose primary key is
# the empty string means the real admin is never seeded, and it never will be.
# A goacos started without ADMIN_USERNAME does exactly that — and nothing says
# so, because ValidateAuthState only checks that no *active* admin still has
# the public password, not that an admin exists.
run_mutation M13 internal/storage/store.go \
  "the admin seed stops refusing an empty admin username" \
  TestSeedNeverCreatesAnEmptyAdmin storage \
  's@if s\.cfg\.AdminUsername == "" \{@if false {@'

echo
if [ "${SELF_TEST:-0}" = "1" ]; then
  # Reverse guard for the harness itself: aim a mutation at a criterion it
  # cannot affect. A harness that cannot report a survival would report every
  # mutation as "caught" and prove nothing.
  echo "== self-test: the harness must be able to report a survival =="
  saved_total=$TOTAL
  saved_failed=$FAILED
  saved_caught=$CAUGHT
  # A comment change alters the source and no behaviour.
  run_mutation SELF internal/storage/schema/migrations/002_application.mysql.sql \
    "self-test: a mutation aimed at a criterion it cannot affect" \
    TestSchemaMigratesOnBothEngines \
    storage \
    's@^-- Migration 002@-- self-test\n-- Migration 002@'
  self_rc=$?
  TOTAL=$saved_total
  FAILED=$saved_failed
  CAUGHT=$saved_caught
  if [ "$self_rc" -eq 1 ]; then
    echo "  [self] OK: the harness reported a survival for a mutation nobody caught"
  else
    echo "  [self] BROKEN: the harness did not report a survival (rc=$self_rc)"
    exit 1
  fi
  echo
fi

echo
echo "== anti-darkness guard: a DSN was given, so nothing may skip =="
#
# The whole reason these tests were dead for months is that they skipped
# silently when no DSN was set. This harness always sets both DSNs, so a
# SKIP here means the fixture is skipping **despite** being configured — which
# is the same failure wearing a different hat.
  skip_out="$(cd "$WORK/src" && "$GO" test ./internal/gateway/ -count=1 -v \
        -run 'TestRefresh|TestFullApplication|TestStalePersistent|TestStaleEphemeral|TestRoutingTableNeverCarries|TestRegistered|TestDeclared' 2>&1)"
skip_rc=$?
if printf '%s\n' "$skip_out" | grep -q -- "--- SKIP"; then
  echo "  [guard] FAIL: a database-backed test skipped even though both DSNs were set"
  printf '%s\n' "$skip_out" | grep -- "--- SKIP" | sed 's/^/            /' | head -10
  FAILED=$((FAILED + 1))
elif [ "$skip_rc" -ne 0 ]; then
  echo "  [guard] FAIL: the database-backed runtime tests did not pass"
  printf '%s\n' "$skip_out" | tail -20 | sed 's/^/            /'
  FAILED=$((FAILED + 1))
else
  echo "  [guard] OK: every database-backed runtime test actually ran"
fi

echo
echo "== summary =="
echo "  mutations and probes: $TOTAL   caught: $CAUGHT   equivalent probes: $EQUIV   failed: $FAILED"
if [ "$EQUIV" -ne 0 ]; then
  echo "  (the equivalent probes are recorded, not counted as catches — see the header)"
fi
HARNESS_FINISHED=1
if [ "$FAILED" -ne 0 ]; then
  echo "  RESULT: FAIL — a mutation was not caught by its named criterion"
  exit 1
fi
echo "  RESULT: PASS — every mutation was caught by its named criterion"
