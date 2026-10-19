#!/usr/bin/env bash
# goacos one-command deployment with dynamic MySQL discovery.
#
# The script finds a reachable MySQL server (Docker containers first, then
# localhost / flags / optional subnet scan), verifies credentials, creates the
# goacos database (schema + seed are applied by the server itself on boot),
# and starts the goacos container attached to that database.
#
# Usage:
#   deploy/deploy.sh                       # auto-discover MySQL, deploy
#   deploy/deploy.sh --host 10.0.0.5 --port-db 3306 --user root --password secret
#   deploy/deploy.sh --cidr 192.168.1.0/24 # also scan a subnet for MySQL
#   deploy/deploy.sh --auth --admin-pass <12+ chars> --token-secret <32+ bytes>
#
# Useful flags: --image IMG --port 8848 --name goacos --db goacos --force --list-only
# Database engine: --db-type mysql|postgres|auto (default auto: discover either
# engine; the resolved engine is passed to the container as GOACOS_DB_TYPE).
set -euo pipefail

IMAGE="${GOACOS_IMAGE:-ghcr.io/halfking/goacos:latest}"
NAME=goacos
PORT=8848
DBNAME=goacos
DB_TYPE="${GOACOS_DB_TYPE:-auto}"   # mysql | postgres | auto
AUTH_ENABLED=false
ADMIN_USER="${GOACOS_ADMIN_USERNAME:-nacos}"
ADMIN_PASS="${GOACOS_ADMIN_PASSWORD:-nacos}"
AUTH_SECRET="${GOACOS_AUTH_TOKEN_SECRET:-}"
CIDR=""
FORCE=0
LIST_ONLY=0
DB_HOST=""; DB_PORT=3306; DB_USER=""; DB_PASS=""

say()  { printf '%s\n' "$*"; }
die()  { printf 'deploy: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2;;
    --port) PORT="$2"; shift 2;;
    --name) NAME="$2"; shift 2;;
    --db) DBNAME="$2"; shift 2;;
    --db-name) DBNAME="$2"; shift 2;;
    --db-type) DB_TYPE="$2"; shift 2;;
    --host) DB_HOST="$2"; shift 2;;
    --port-db) DB_PORT="$2"; shift 2;;
    --user) DB_USER="$2"; shift 2;;
    --password) DB_PASS="$2"; shift 2;;
    --cidr) CIDR="$2"; shift 2;;
    --auth) AUTH_ENABLED=true; shift;;
    --admin-user) ADMIN_USER="$2"; shift 2;;
    --admin-pass) ADMIN_PASS="$2"; shift 2;;
    --token-secret) AUTH_SECRET="$2"; shift 2;;
    --force) FORCE=1; shift;;
    --list-only) LIST_ONLY=1; shift;;
    -h|--help) sed -n '2,14p' "$0"; exit 0;;
    *) die "unknown flag: $1 (see --help)";;
  esac
done
case "$DB_TYPE" in mysql|postgres|auto) ;; *) die "invalid --db-type '$DB_TYPE' (mysql|postgres|auto)";; esac

# ---------------------------------------------------------------- auth preflight
# An auth-enabled server refuses to start on a public or short JWT key, or on a
# weak seeded admin password (config.ValidateAuth). Check the same contract
# here so the operator gets a readable message instead of a container that
# crash-loops on boot with the error buried in `docker logs`.
OLD_PUBLIC_KEY="goacos-default-secret-SecretKey012345678901234567890123456789012345678901234567890123456789"

# Trim leading/trailing whitespace the same way the server's strings.TrimSpace does.
_trim() { local v="$1"; v="${v#"${v%%[![:space:]]*}"}"; printf '%s' "${v%"${v##*[![:space:]]}"}"; }

if [ "$AUTH_ENABLED" = true ]; then
  [ -n "$AUTH_SECRET" ] || die "--auth requires --token-secret (or GOACOS_AUTH_TOKEN_SECRET): an explicit private JWT key of at least 32 bytes"
  [ "${#AUTH_SECRET}" -ge 32 ] || die "--token-secret must be at least 32 bytes (got ${#AUTH_SECRET})"
  [ "$AUTH_SECRET" != "$OLD_PUBLIC_KEY" ] || die "--token-secret must not be the old public repository default"
  [ "$(_trim "$AUTH_SECRET")" = "$AUTH_SECRET" ] || die "--token-secret must not start or end with whitespace"
  [ -n "$ADMIN_USER" ] || die "--admin-user must be nonblank when --auth is used"
  [ "$(_trim "$ADMIN_USER")" = "$ADMIN_USER" ] || die "--admin-user must not start or end with whitespace"
  [ "${#ADMIN_PASS}" -ge 12 ] || die "--admin-pass must be at least 12 characters when --auth is used (got ${#ADMIN_PASS})"
  [ "$(printf '%s' "$ADMIN_PASS" | wc -c | tr -d ' ')" -le 72 ] || die "--admin-pass must be at most 72 bytes when --auth is used"
  [ "$(_trim "$ADMIN_PASS")" = "$ADMIN_PASS" ] || die "--admin-pass must not start or end with whitespace"
fi

# ---------------------------------------------------------------- goacos binary
# (used host-side for DB discovery & initialization; multi-arch, no deps)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
BIN="${GOACOS_BIN:-}"
if [ -z "$BIN" ]; then
  if [ -x "$REPO_DIR/goacos" ]; then
    BIN="$REPO_DIR/goacos"
  elif command -v go >/dev/null 2>&1; then
    say "==> building host helper binary (goacos)"
    (cd "$REPO_DIR" && go build -o "$REPO_DIR/goacos" .) || die "go build failed"
    BIN="$REPO_DIR/goacos"
  else
    die "need the goacos binary (or Go toolchain) on the host for DB discovery; set GOACOS_BIN=/path/to/goacos"
  fi
fi

# ---------------------------------------------------------------- discovery
if [ -z "$DB_HOST" ]; then
  say "==> discovering database servers (docker containers, localhost${CIDR:+, subnet $CIDR}; engine=$DB_TYPE)"
  DISCOVER_ARGS=(db discover --best)
  case "$DB_TYPE" in
    mysql|postgres) DISCOVER_ARGS+=(--db-type "$DB_TYPE");;
  esac
  [ -n "$CIDR" ] && DISCOVER_ARGS+=(--cidr "$CIDR")
  OUT=$("$BIN" "${DISCOVER_ARGS[@]}") || die "no matching database server found (pass --host/--user/--password, or start one: docker run -d -e MYSQL_ROOT_PASSWORD=... -p 3306:3306 mysql:8.0)"
  [ "$LIST_ONLY" = 1 ] && { say "$OUT"; exit 0; }
  DB_TYPE=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["type"])')
  DB_HOST=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["host"])')
  DB_PORT=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["port"])')
  DB_USER=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["user"])')
  DB_PASS=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["password"])')
fi
case "$DB_TYPE" in mysql|postgres) ;; *) DB_TYPE=mysql ;; esac

say "==> attaching to $DB_TYPE $DB_HOST:$DB_PORT (db=$DBNAME, user=$DB_USER)"
"$BIN" db verify --db-type "$DB_TYPE" --host "$DB_HOST" --port "$DB_PORT" --user "$DB_USER" --password "$DB_PASS" --db "$DBNAME"
"$BIN" db init   --db-type "$DB_TYPE" --host "$DB_HOST" --port "$DB_PORT" --user "$DB_USER" --password "$DB_PASS" --db "$DBNAME"

# ---------------------------------------------------------------- reachability
# The goacos container must reach MySQL. When the host address is loopback,
# rewrite it to host.docker.internal (built into Docker Desktop; on Linux we
# add the host-gateway mapping).
CONTAINER_HOST="$DB_HOST"
EXTRA_RUN_ARGS=()
case "$DB_HOST" in
  127.0.0.1|localhost|::1)
    CONTAINER_HOST=host.docker.internal
    if [ "$(uname -s)" = "Linux" ]; then
      EXTRA_RUN_ARGS+=(--add-host=host.docker.internal:host-gateway)
    fi
    ;;
esac

# ---------------------------------------------------------------- run
if [ "$(docker ps -aq -f "name=^/$NAME$" 2>/dev/null)" ]; then
  if [ "$FORCE" = 1 ]; then
    say "==> removing existing container $NAME (--force)"
    docker rm -f "$NAME" >/dev/null
  else
    die "container '$NAME' already exists (use --force to recreate)"
  fi
fi

say "==> starting $NAME ($IMAGE) on :$PORT"
docker run -d --name "$NAME" \
  -p "$PORT:8848" \
  -e GOACOS_DB_TYPE="$DB_TYPE" \
  -e MYSQL_HOST="$CONTAINER_HOST" -e MYSQL_PORT="$DB_PORT" \
  -e MYSQL_DB="$DBNAME" -e MYSQL_USER="$DB_USER" -e MYSQL_PASSWORD="$DB_PASS" \
  -e GOACOS_AUTH_ENABLED="$AUTH_ENABLED" \
  -e GOACOS_AUTH_TOKEN_SECRET="$AUTH_SECRET" \
  -e GOACOS_ADMIN_USERNAME="$ADMIN_USER" -e GOACOS_ADMIN_PASSWORD="$ADMIN_PASS" \
  --restart unless-stopped \
  ${EXTRA_RUN_ARGS+"${EXTRA_RUN_ARGS[@]}"} \
  "$IMAGE" >/dev/null

say "==> waiting for health"
for i in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:$PORT/nacos/actuator/health" >/dev/null 2>&1; then
    say ""
    say "goacos is UP"
    say "  console      http://127.0.0.1:$PORT/nacos/index.html"
    say "  open api     http://127.0.0.1:$PORT/nacos/v1/... /nacos/v2/..."
    say "  health       http://127.0.0.1:$PORT/nacos/actuator/health"
    say "  admin        $ADMIN_USER / $ADMIN_PASS (change via GOACOS_ADMIN_PASSWORD)"
    say "  database     $DB_TYPE $DB_HOST:$DB_PORT/$DBNAME"
    say "  logs         docker logs -f $NAME"
    exit 0
  fi
  printf '.'
  sleep 1
done
say ""
die "container did not become healthy; check: docker logs $NAME"
