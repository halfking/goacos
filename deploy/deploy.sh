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
#   deploy/deploy.sh --auth --admin-pass S3cret
#
# Useful flags: --image IMG --port 8848 --name goacos --db goacos --force --list-only
set -euo pipefail

IMAGE="${GOACOS_IMAGE:-ghcr.io/halfking/goacos:latest}"
NAME=goacos
PORT=8848
DBNAME=goacos
AUTH_ENABLED=false
ADMIN_USER="${GOACOS_ADMIN_USERNAME:-nacos}"
ADMIN_PASS="${GOACOS_ADMIN_PASSWORD:-nacos}"
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
    --host) DB_HOST="$2"; shift 2;;
    --port-db) DB_PORT="$2"; shift 2;;
    --user) DB_USER="$2"; shift 2;;
    --password) DB_PASS="$2"; shift 2;;
    --cidr) CIDR="$2"; shift 2;;
    --auth) AUTH_ENABLED=true; shift;;
    --admin-user) ADMIN_USER="$2"; shift 2;;
    --admin-pass) ADMIN_PASS="$2"; shift 2;;
    --force) FORCE=1; shift;;
    --list-only) LIST_ONLY=1; shift;;
    -h|--help) sed -n '2,14p' "$0"; exit 0;;
    *) die "unknown flag: $1 (see --help)";;
  esac
done

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
  say "==> discovering MySQL servers (docker containers, localhost${CIDR:+, subnet $CIDR})"
  DISCOVER_ARGS=(db discover --best)
  [ -n "$CIDR" ] && DISCOVER_ARGS+=(--cidr "$CIDR")
  OUT=$("$BIN" "${DISCOVER_ARGS[@]}") || die "no matching MySQL server found (pass --host/--user/--password, or start one: docker run -d -e MYSQL_ROOT_PASSWORD=... -p 3306:3306 mysql:8.0)"
  [ "$LIST_ONLY" = 1 ] && { say "$OUT"; exit 0; }
  DB_HOST=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["host"])')
  DB_PORT=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["port"])')
  DB_USER=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["user"])')
  DB_PASS=$(echo "$OUT"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["password"])')
fi

say "==> attaching to mysql $DB_HOST:$DB_PORT (db=$DBNAME, user=$DB_USER)"
"$BIN" db verify --host "$DB_HOST" --port "$DB_PORT" --user "$DB_USER" --password "$DB_PASS" --db "$DBNAME"
"$BIN" db init   --host "$DB_HOST" --port "$DB_PORT" --user "$DB_USER" --password "$DB_PASS" --db "$DBNAME"

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
  -e MYSQL_HOST="$CONTAINER_HOST" -e MYSQL_PORT="$DB_PORT" \
  -e MYSQL_DB="$DBNAME" -e MYSQL_USER="$DB_USER" -e MYSQL_PASSWORD="$DB_PASS" \
  -e GOACOS_AUTH_ENABLED="$AUTH_ENABLED" \
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
    say "  mysql        $DB_HOST:$DB_PORT/$DBNAME"
    say "  logs         docker logs -f $NAME"
    exit 0
  fi
  printf '.'
  sleep 1
done
say ""
die "container did not become healthy; check: docker logs $NAME"
