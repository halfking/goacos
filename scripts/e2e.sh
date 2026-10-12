#!/usr/bin/env bash
# goacos end-to-end acceptance: boots (or reuses) a MySQL 8 / PostgreSQL 16
# container, starts goacos against a fresh database, and drives the
# Nacos-compatible API surface.
# Usage: E2E_DB=mysql|postgres scripts/e2e.sh [--keep]
set -uo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

ENGINE="${E2E_DB:-mysql}"
case "$ENGINE" in
  mysql)    ;;
  postgres) ;;
  *) echo "E2E_DB must be mysql or postgres (got: $ENGINE)"; exit 2;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN=/tmp/goacos-e2e-server
HTTP_PORT=${HTTP_PORT:-18848}
DB="goacos_e2e_$(date +%s)"
PASS=0; FAIL=0

if [ "$ENGINE" = mysql ]; then
  DB_CONTAINER=goacos-e2e-mysql
  DB_PORT=${MYSQL_PORT:-13310}
  DB_PASS=${MYSQL_PASS:-e2eroot}
  DB_USER=root
  DB_IMAGE=${MYSQL_IMAGE:-mysql:8.0}
else
  DB_CONTAINER=goacos-e2e-pg
  DB_PORT=${PG_PORT:-15432}
  DB_PASS=${PG_PASS:-e2eroot}
  DB_USER=postgres
  DB_IMAGE=${PG_IMAGE:-postgres:16-alpine}
fi

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $1"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want '$3' got '$2')"; fi }

cleanup() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  [ -n "${SRV2_PID:-}" ] && kill "$SRV2_PID" 2>/dev/null
  [ -n "${SRV3_PID:-}" ] && kill "$SRV3_PID" 2>/dev/null
  [ -n "${UP_PID:-}" ] && kill "$UP_PID" 2>/dev/null
  [ -n "${BEATER_PID:-}" ] && kill "$BEATER_PID" 2>/dev/null
  if [ "$KEEP" != 1 ]; then
    docker rm -f "$DB_CONTAINER" >/dev/null 2>&1
    [ -n "${SRV_LOG:-}" ] && rm -f "$SRV_LOG"
  fi
}
trap cleanup EXIT

say "== build =="
( cd "$ROOT" && go build -o "$BIN" . ) || { say "build failed"; exit 1; }

# kill leftovers of previous e2e runs, then refuse to fight a busy port
pkill -f "$BIN" 2>/dev/null || true
pkill -f "goacos serve" 2>/dev/null || true
sleep 1
for P in "$HTTP_PORT" "$((HTTP_PORT+1))" "$((HTTP_PORT+2))" "$((HTTP_PORT+3))" "$((HTTP_PORT+4))" "$((HTTP_PORT+5))"; do
  if nc -z 127.0.0.1 "$P" 2>/dev/null; then
    say "port $P is busy — free it and retry"; exit 1
  fi
done

say "== database ($ENGINE) =="
if ! nc -z 127.0.0.1 "$DB_PORT" 2>/dev/null; then
  say "  starting $DB_IMAGE container on :$DB_PORT"
  if [ "$ENGINE" = mysql ]; then
    docker run -d --name "$DB_CONTAINER" -e MYSQL_ROOT_PASSWORD="$DB_PASS" \
      -p "127.0.0.1:$DB_PORT:3306" "$DB_IMAGE" >/dev/null || exit 1
    # 2026-10-09: mysqladmin ping 在首启的临时服务器阶段（root 密码未设）也返回 0，
    # 会过早 break 然后栽在下面的带密码检查上。改用真实带密码查询做就绪判定。
    for i in $(seq 1 90); do
      docker exec "$DB_CONTAINER" mysql -uroot -p"$DB_PASS" -e "SELECT 1" >/dev/null 2>&1 && break
      sleep 2
    done
  else
    docker run -d --name "$DB_CONTAINER" -e POSTGRES_PASSWORD="$DB_PASS" \
      -p "127.0.0.1:$DB_PORT:5432" "$DB_IMAGE" >/dev/null || exit 1
    for i in $(seq 1 60); do
      docker exec "$DB_CONTAINER" pg_isready -U postgres >/dev/null 2>&1 && break
      sleep 1
    done
  fi
fi
if [ "$ENGINE" = mysql ]; then
  docker exec "$DB_CONTAINER" mysql -uroot -p"$DB_PASS" -e "SELECT 1" >/dev/null 2>&1 || {
    say "mysql not reachable; container status/logs:"
    docker ps -a --format '{{.Names}}\t{{.Status}}' | grep "$DB_CONTAINER" || true
    docker logs --tail 20 "$DB_CONTAINER" 2>&1 || true
    exit 1
  }
else
  docker exec "$DB_CONTAINER" psql -U postgres -c "SELECT 1" >/dev/null 2>&1 || {
    say "postgres not reachable; container status/logs:"
    docker ps -a --format '{{.Names}}\t{{.Status}}' | grep "$DB_CONTAINER" || true
    docker logs --tail 20 "$DB_CONTAINER" 2>&1 || true
    exit 1
  }
fi

say "== start goacos (fast sweep for lifecycle test) =="
SRV_LOG=/tmp/goacos-e2e-server.log
GOACOS_DB_TYPE=$ENGINE \
GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_PORT=$DB_PORT GOACOS_MYSQL_DB=$DB \
GOACOS_MYSQL_USER=$DB_USER GOACOS_MYSQL_PASSWORD="$DB_PASS" GOACOS_PORT=$HTTP_PORT \
GOACOS_HEARTBEAT_TIMEOUT_MS=2000 GOACOS_EPHEMERAL_DELETE_AFTER_MS=4000 \
GOACOS_SWEEP_INTERVAL_MS=1000 \
"$BIN" serve >"$SRV_LOG" 2>&1 &
SRV_PID=$!
B="http://127.0.0.1:$HTTP_PORT"

for i in $(seq 1 20); do curl -sf "$B/nacos/actuator/health" >/dev/null && break; sleep 0.5; done
check "health" "$(curl -s -o /dev/null -w '%{http_code}' "$B/nacos/actuator/health")" "200"
if [ "$ENGINE" = mysql ]; then
  docker exec "$DB_CONTAINER" mysql -uroot -p"$DB_PASS" -e "USE $DB; SHOW TABLES" 2>/dev/null | grep -q config_info \
    && ok "auto-created database + schema" || bad "auto-created database + schema"
else
  CNT=$(docker exec "$DB_CONTAINER" psql -U postgres -d "$DB" -tAc "SELECT count(*) FROM information_schema.tables WHERE table_name='config_info'" 2>/dev/null)
  [ "$CNT" = "1" ] && ok "auto-created database + schema" || bad "auto-created database + schema (count=$CNT)"
fi

say "== config center =="
check "v1 publish" "$(curl -s -X POST "$B/nacos/v1/cs/configs" --data-urlencode 'dataId=app.yaml' --data-urlencode 'group=DEFAULT_GROUP' --data-urlencode 'content=key: v1' --data-urlencode 'type=yaml')" "true"
check "v1 get raw" "$(curl -s "$B/nacos/v1/cs/configs?dataId=app.yaml&group=DEFAULT_GROUP")" "key: v1"
check "v1 update"  "$(curl -s -X POST "$B/nacos/v1/cs/configs" --data-urlencode 'dataId=app.yaml' --data-urlencode 'group=DEFAULT_GROUP' --data-urlencode 'content=key: v2')" "true"
check "v1 delete"  "$(curl -s -X DELETE "$B/nacos/v1/cs/configs?dataId=app.yaml&group=DEFAULT_GROUP")" "true"
check "v1 get deleted -> 404" "$(curl -s -o /dev/null -w '%{http_code}' "$B/nacos/v1/cs/configs?dataId=app.yaml&group=DEFAULT_GROUP")" "404"

V2=$(curl -s -X POST "$B/nacos/v2/cs/config" -H 'Content-Type: application/json' -d '{"dataId":"svc.json","group":"DEFAULT_GROUP","content":"{}","type":"json"}')
check "v2 publish envelope" "$(echo "$V2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["code"])')" "0"
V2G=$(curl -s "$B/nacos/v2/cs/config?dataId=svc.json&group=DEFAULT_GROUP&namespaceId=")
check "v2 get" "$(echo "$V2G" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["data"]["dataId"], d["data"]["type"])')" "svc.json json"

LIST=$(curl -s "$B/nacos/v1/cs/configs?search=blur&pageNo=1&pageSize=10")
check "v1 list total" "$(echo "$LIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)["totalCount"])')" "1"

HIST=$(curl -s "$B/nacos/v1/cs/history?search=blur&pageNo=1&pageSize=10&dataId=app")
check "history rows (I/U/D)" "$(echo "$HIST" | python3 -c 'import json,sys; print(",".join(sorted(i["opType"] for i in json.load(sys.stdin)["pageItems"])))')" "D,I,U"

check "namespace create" "$(curl -s -X POST "$B/nacos/v1/console/namespaces" -d 'customNamespaceId=dev&customNamespaceName=Dev' | python3 -c 'import json,sys; print(json.load(sys.stdin)["code"])')" "200"
check "ns-scoped publish" "$(curl -s -X POST "$B/nacos/v1/cs/configs" --data-urlencode 'dataId=d.yaml' --data-urlencode 'group=DEFAULT_GROUP' --data-urlencode 'tenant=dev' --data-urlencode 'content=env: dev')" "true"

say "== long polling =="
LP=$(python3 - "$B" <<'EOF'
import sys, urllib.request, urllib.parse, threading, time, json
b = sys.argv[1]
body = urllib.parse.urlencode({"Listening-Configs": "svc.json\x02DEFAULT_GROUP\x02stale-md5\x02\x01"}).encode()
req = urllib.request.Request(b+"/nacos/v1/cs/configs/listener", data=body,
    headers={"Long-Pulling-Timeout":"29000","Content-Type":"application/x-www-form-urlencoded"})
result = {}
def listener():
    t0 = time.time()
    r = urllib.request.urlopen(req, timeout=35)
    result["body"] = r.read().decode(); result["dt"] = time.time()-t0
th = threading.Thread(target=listener); th.start()
time.sleep(1.0)
urllib.request.urlopen(urllib.request.Request(b+"/nacos/v2/cs/config",
    data=json.dumps({"dataId":"svc.json","group":"DEFAULT_GROUP","content":"{\"k\":2}","type":"json"}).encode(),
    headers={"Content-Type":"application/json"}))
th.join(35)
print("NOTIFY %.1fs %s" % (result.get("dt",99), "svc.json" in result.get("body","")))
EOF
)
case "$LP" in
  NOTIFY*1.0s*True|NOTIFY*True*) ok "v1 long-poll notified on change ($LP)";;
  *) bad "v1 long-poll did not notify ($LP)";;
esac

say "== naming =="
check "register #1" "$(curl -s -X POST "$B/nacos/v1/ns/instance" -d 'serviceName=order-svc&ip=10.0.0.1&port=8081&metadata={"v":"1"}')" "ok"
check "register #2" "$(curl -s -X POST "$B/nacos/v1/ns/instance" -d 'serviceName=order-svc&ip=10.0.0.2&port=8082')" "ok"
IL=$(curl -s "$B/nacos/v1/ns/instance/list?serviceName=order-svc&groupName=DEFAULT_GROUP")
check "instance count" "$(echo "$IL" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["hosts"]))')" "2"
check "serviceInfo name" "$(echo "$IL" | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])')" "DEFAULT_GROUP@@order-svc"
SG=$(curl -s "$B/nacos/v1/ns/instance?serviceName=order-svc&ip=10.0.0.1&port=8081")
check "single get metadata" "$(echo "$SG" | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["v"])')" "1"
BEAT=$(curl -s "$B/nacos/v1/ns/instance/beat?serviceName=order-svc&ip=10.0.0.2&port=8082")
check "beat code" "$(echo "$BEAT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["code"])')" "10200"
check "v2 register" "$(curl -s -X POST "$B/nacos/v2/ns/instance" -H 'Content-Type: application/json' -d '{"serviceName":"pay-svc","ip":"10.0.0.9","port":9090}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"])')" "ok"
check "service list" "$(curl -s "$B/nacos/v1/ns/service/list?pageNo=1&pageSize=10" | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])')" "2"

say "== ephemeral lifecycle (fast sweep: 2s unhealthy / 4s delete) =="
# instance 10.0.0.2 keeps beating every second; 10.0.0.1 is abandoned
( for i in $(seq 1 6); do
    curl -s "$B/nacos/v1/ns/instance/beat?serviceName=order-svc&ip=10.0.0.2&port=8082" >/dev/null
    sleep 1
  done ) &
BEATER_PID=$!
sleep 5
IL2=$(curl -s "$B/nacos/v1/ns/instance/list?serviceName=order-svc")
check "abandoned deleted, beating instance stays healthy" \
  "$(echo "$IL2" | python3 -c 'import json,sys; hs=json.load(sys.stdin)["hosts"]; print(hs[0]["ip"], hs[0]["healthy"])')" "10.0.0.2 True"
kill $BEATER_PID 2>/dev/null
check "deregister" "$(curl -s -X DELETE "$B/nacos/v1/ns/instance?serviceName=order-svc&ip=10.0.0.2&port=8082")" "ok"
sleep 1.5
check "deregistered gone" "$(curl -s "$B/nacos/v1/ns/instance/list?serviceName=order-svc" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["hosts"]))')" "0"

say "== application model (goacos-specific) =="
# The application model is what lets a gateway stop keeping its own
# domain -> host:port table, so the thing under test here is ResolveRoute:
# declare policy, register instances, then ask "who serves this domain".
curl -s -X POST "$B/goacos/v1/apps" -H 'Content-Type: application/json' \
  -d '{"name":"acc","displayName":"ACC","env":"itestu"}' >/dev/null
check "app create" "$(curl -s "$B/goacos/v1/apps" | python3 -c 'import json,sys; print(",".join(a["name"] for a in json.load(sys.stdin)["data"]))')" "acc"
curl -s -X POST "$B/goacos/v1/apps/acc/components" -H 'Content-Type: application/json' \
  -d '{"name":"api","kind":"api","protocol":"http","serviceName":"acc-api","healthPath":"/healthz"}' >/dev/null
curl -s -X POST "$B/goacos/v1/apps/acc/components" -H 'Content-Type: application/json' \
  -d '{"name":"web","kind":"web","protocol":"static","serviceName":"acc-web","healthPath":"/"}' >/dev/null
check "components" "$(curl -s "$B/goacos/v1/apps/acc" | python3 -c 'import json,sys; print(",".join(sorted(c["name"] for c in json.load(sys.stdin)["data"]["components"])))')" "api,web"
check "component defaults applied" "$(curl -s "$B/goacos/v1/apps/acc" | python3 -c 'import json,sys; c=[x for x in json.load(sys.stdin)["data"]["components"] if x["name"]=="api"][0]; print(c["weight"], c["lbAlgorithm"], c["groupName"])')" "1 round_robin DEFAULT_GROUP"

# Two components of the same application must be independently addressable —
# this is the frontend+backend case the naming model alone cannot express.
curl -s -X POST "$B/nacos/v1/ns/instance" -d 'serviceName=acc-api&ip=10.106.175.168&port=4100&metadata={"lane":"stable","app":"acc"}' >/dev/null
curl -s -X POST "$B/nacos/v1/ns/instance" -d 'serviceName=acc-api&ip=10.106.175.169&port=4100&metadata={"lane":"canary","app":"acc"}' >/dev/null
curl -s -X POST "$B/nacos/v1/ns/instance" -d 'serviceName=acc-web&ip=10.106.175.168&port=4175' >/dev/null
curl -s -X POST "$B/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"acc.itestu.cn","pathPrefix":"/","appName":"acc","componentName":"api","upstreamName":"acc_api"}' >/dev/null
curl -s -X POST "$B/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"acc-api.itestu.cn","pathPrefix":"/","appName":"acc","componentName":"api","upstreamName":"acc_api"}' >/dev/null

RES=$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&path=/api/v2/health")
check "resolve api domain" "$(echo "$RES" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["appName"], d["componentName"], len(d["instances"]))')" "acc api 2"
check "resolve carries policy not address" "$(echo "$RES" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["upstreamName"], d["protocol"], d["lbAlgorithm"])')" "acc_api http round_robin"
check "resolve instance metadata" "$(echo "$RES" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(",".join(sorted(i["metadata"].get("lane","-") for i in d["instances"])))')" "canary,stable"

# Metadata routing is how a canary or lane is selected without touching the
# instance table; an unmatched filter must return nothing rather than fall back.
check "resolve canary filter" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&metaFilter=lane=canary" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(len(d["instances"]), d["instances"][0]["ip"])')" "1 10.106.175.169"
check "resolve unmatched filter -> empty" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&metaFilter=lane=nope" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]["instances"]))')" "0"

# Domain routing must actually separate frontend from backend.
check "frontend component resolves separately" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["instances"][0]["port"])')" "4100"
curl -s -X POST "$B/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"acc.itestu.cn","pathPrefix":"/ui","appName":"acc","componentName":"web","upstreamName":"acc_web"}' >/dev/null
check "longest prefix wins" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&path=/ui/app.js" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["componentName"], d["instances"][0]["port"])')" "web 4175"
check "prefix does not match across segment boundary" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&path=/uiabc" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["componentName"])')" "api"

check "undeclared domain -> null, not error" "$(curl -s "$B/goacos/v1/route/resolve?domain=nope.example.com" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["code"], d["data"])')" "200 None"
check "route list" "$(curl -s "$B/goacos/v1/routes" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')" "3"

# Deleting a component must take its routes with it, so the model can never be
# left pointing at something that no longer exists.
curl -s -X DELETE "$B/goacos/v1/apps/acc/components/web" >/dev/null
check "component delete cascades to route" "$(curl -s "$B/goacos/v1/routes" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')" "2"
check "deleted component route no longer resolves" "$(curl -s "$B/goacos/v1/route/resolve?domain=acc.itestu.cn&path=/ui/app.js" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["componentName"])')" "api"

# Migration ledger: the application tables must be recorded as applied, and a
# restart must not re-run them or fail.
if [ "$ENGINE" = mysql ]; then
  MV=$(docker exec "$DB_CONTAINER" mysql -uroot -p"$DB_PASS" -N -B -e "SELECT version FROM $DB.goacos_schema_version ORDER BY version" 2>/dev/null | tr '\n' ',')
else
  MV=$(docker exec "$DB_CONTAINER" psql -U postgres -d "$DB" -tAc "SELECT version FROM goacos_schema_version ORDER BY version" 2>/dev/null | tr '\n' ',')
fi
case "$MV" in 2,*) ok "migration 002 recorded ($MV)";; *) bad "migration ledger (got '$MV')";; esac

say "== gateway data plane =="
# Everything above proves goacos can DECLARE a route. Everything below proves
# it can SERVE one: real listeners on real ports, real TLS handshakes, real
# bytes to a registered instance.
#
# This runs as a third instance rather than by restarting the first, so the
# gateway-disabled default stays covered by the tests above.
GW_HTTP=$((HTTP_PORT+2)); GW_HTTPS=$((HTTP_PORT+3)); GW_API=$((HTTP_PORT+5)); UP_PORT=$((HTTP_PORT+4))
UP_PY=/tmp/goacos-e2e-upstream.py
cat >"$UP_PY" <<'PYEOF'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def _reply(self):
        body = json.dumps({"upstream": "acc-api", "path": self.path,
                           "xForwardedProto": self.headers.get("X-Forwarded-Proto", ""),
                           "xForwardedFor": self.headers.get("X-Forwarded-For", ""),
                           "xGoacosComponent": self.headers.get("X-Goacos-Component", ""),
                           "host": self.headers.get("Host", "")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    do_GET = do_POST = do_HEAD = _reply
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PYEOF
python3 "$UP_PY" "$UP_PORT" & UP_PID=$!
for i in $(seq 1 20); do nc -z 127.0.0.1 "$UP_PORT" 2>/dev/null && break; sleep 0.25; done

GW_ROOT=/tmp/goacos-e2e-static
GW_CERTS=/tmp/goacos-e2e-certs
rm -rf "$GW_ROOT" "$GW_CERTS"; mkdir -p "$GW_ROOT/e2e-web" "$GW_CERTS"
printf '<!doctype html><title>e2e spa</title>' > "$GW_ROOT/e2e-web/index.html"
mkdir -p "$GW_ROOT/e2e-web/ui"
printf 'console.log("hashed")' > "$GW_ROOT/e2e-web/ui/app.4f3a2b1c.js"

GOACOS_DB_TYPE=$ENGINE \
GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_PORT=$DB_PORT GOACOS_MYSQL_DB=$DB \
GOACOS_MYSQL_USER=$DB_USER GOACOS_MYSQL_PASSWORD="$DB_PASS" GOACOS_PORT=$GW_API \
GOACOS_GATEWAY_ENABLED=true \
GOACOS_GATEWAY_HTTP_ADDR="127.0.0.1:$GW_HTTP" GOACOS_GATEWAY_HTTPS_ADDR="127.0.0.1:$GW_HTTPS" \
GOACOS_GATEWAY_NAMESPACE=public GOACOS_GATEWAY_REFRESH_SECONDS=1 \
GOACOS_GATEWAY_STATIC_ROOT="$GW_ROOT" GOACOS_GATEWAY_ACME_ENABLED=false \
GOACOS_GATEWAY_ACME_DATA_DIR=/tmp/goacos-e2e-acme GOACOS_GATEWAY_SELFSIGNED_DIR=/tmp/goacos-e2e-selftest \
GOACOS_GATEWAY_CERT_DIR="$GW_CERTS" GOACOS_GATEWAY_CERT_SCAN_SECONDS=1 \
"$BIN" serve >/tmp/goacos-e2e-gw.log 2>&1 &
SRV3_PID=$!
G="http://127.0.0.1:$GW_API"
for i in $(seq 1 30); do curl -sf "$G/nacos/actuator/health" >/dev/null && break; sleep 0.5; done

check "gateway enabled in status" "$(curl -s "$G/goacos/v1/gateway/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["enabled"])')" "True"

curl -s -X POST "$G/goacos/v1/apps" -H 'Content-Type: application/json' -d '{"name":"e2e","env":"test"}' >/dev/null
curl -s -X POST "$G/goacos/v1/apps/e2e/components" -H 'Content-Type: application/json' \
  -d '{"name":"api","kind":"api","protocol":"http","serviceName":"e2e-api"}' >/dev/null
curl -s -X POST "$G/goacos/v1/apps/e2e/components" -H 'Content-Type: application/json' \
  -d '{"name":"web","kind":"web","protocol":"static","serviceName":"e2e-web"}' >/dev/null
curl -s -X POST "$G/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"e2e.test","pathPrefix":"/","appName":"e2e","componentName":"api"}' >/dev/null
curl -s -X POST "$G/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"e2e.test","pathPrefix":"/ui","appName":"e2e","componentName":"web"}' >/dev/null
curl -s -X POST "$G/nacos/v1/ns/instance" -d "serviceName=e2e-api&ip=127.0.0.1&port=$UP_PORT&healthy=true&enabled=true&ephemeral=false" >/dev/null
sleep 2

# --- the request actually reaches a registered instance ---
GWREQ() { curl -sk --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "$@"; }
GWREQI() { curl -skI --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "$@"; }
RES=$(GWREQ "https://e2e.test:$GW_HTTPS/api/v2/hello")
check "gateway proxies to instance" "$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["upstream"])')" "acc-api"
check "upstream sees full path" "$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["path"])')" "/api/v2/hello"
check "gateway adds component header" "$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["xGoacosComponent"])')" "e2e/api"
check "upstream sees real Host" "$(echo "$RES" | python3 -c 'import json,sys; h=json.load(sys.stdin)["host"]; print(h.startswith("127.0.0.1:") or h.startswith("localhost:"))')" "True"
check "undeclared domain 404s" "$(curl -sk -o /dev/null -w '%{http_code}' --resolve "nope.test:$GW_HTTPS:127.0.0.1" "https://nope.test:$GW_HTTPS/x")" "404"
check "longest prefix routes to static" "$(GWREQ "https://e2e.test:$GW_HTTPS/ui/thing" | head -c 15)" "<!doctype html>"
check "SPA deep link falls back" "$(GWREQ "https://e2e.test:$GW_HTTPS/ui/deep/route" | head -c 15)" "<!doctype html>"
check "fingerprinted asset is served" "$(GWREQ "https://e2e.test:$GW_HTTPS/ui/app.4f3a2b1c.js")" 'console.log("hashed")'
check "fingerprinted asset is immutable" "$(GWREQI "https://e2e.test:$GW_HTTPS/ui/app.4f3a2b1c.js" | grep -ci 'cache-control:.*immutable')" "1"
check "missing asset 404s" "$(GWREQ -o /dev/null -w '%{http_code}' "https://e2e.test:$GW_HTTPS/ui/app.deadbeef.js")" "404"

# --- TLS: real handshake, real SNI, real certificate ---
TLS=$(curl -sk --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "https://e2e.test:$GW_HTTPS/api/tls" 2>/dev/null)
check "https reaches instance" "$(echo "$TLS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["upstream"])' 2>/dev/null)" "acc-api"
check "tls says it is a fallback cert" "$(curl -skI --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "https://e2e.test:$GW_HTTPS/api/tls" 2>/dev/null | grep -i '^x-goacos-cert-source' | tr -d '\r' | awk '{print $2}')" "self_signed"
# The certificate presented must actually carry the domain it was requested
# for. A gateway that hands every connection the same fallback certificate
# would pass every other check here and still break every real browser.
check "cert covers the served domain" "$(echo | openssl s_client -connect "127.0.0.1:$GW_HTTPS" -servername e2e.test 2>/dev/null | openssl x509 -noout -text 2>/dev/null | grep -c 'DNS:e2e.test')" "1"

# Certificates are listed with provenance and validity, never with key material.
CERTS=$(curl -s "$G/goacos/v1/certs")
check "cert list reports source" "$(echo "$CERTS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["source"])')" "self_signed"
check "cert list leaks no key" "$(echo "$CERTS" | grep -c 'keyPem')" "0"
check "cert list warns about self-signed" "$(echo "$CERTS" | python3 -c 'import json,sys; print("self-signed" in json.load(sys.stdin)["data"][0]["sourceWarning"])')" "True"

# --- per-domain policy ---
curl -s -X POST "$G/goacos/v1/vhosts" -H 'Content-Type: application/json' -d '{"domain":"e2e.test","forceHttps":false,"denyIps":"127.0.0.1"}' >/dev/null
sleep 2
check "denied client is refused" "$(GWREQ -o /dev/null -w '%{http_code}' "https://e2e.test:$GW_HTTPS/api/x")" "403"
curl -s -X POST "$G/goacos/v1/vhosts" -H 'Content-Type: application/json' -d '{"domain":"e2e.test","allowIps":"127.0.0.1","denyIps":""}' >/dev/null
sleep 2
check "allowed client passes again" "$(GWREQ -o /dev/null -w '%{http_code}' "https://e2e.test:$GW_HTTPS/api/x")" "200"
curl -s -X DELETE "$G/goacos/v1/vhosts/e2e.test" >/dev/null

# --- force HTTPS: the HTTP listener must redirect, not serve ---
curl -s -X POST "$G/goacos/v1/vhosts" -H 'Content-Type: application/json' -d '{"domain":"e2e.test","forceHttps":true}' >/dev/null
sleep 2
check "http redirects to https" "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: e2e.test' "http://127.0.0.1:$GW_HTTP/api/x")" "301"
check "redirect preserves path" "$(curl -sI -H 'Host: e2e.test' "http://127.0.0.1:$GW_HTTP/api/x?y=1" | grep -i '^location' | tr -d '\r' | awk '{print $2}')" "https://e2e.test:$GW_HTTPS/api/x?y=1"
check "acme challenge is never redirected" "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: e2e.test' "http://127.0.0.1:$GW_HTTP/.well-known/acme-challenge/nope")" "404"

# --- registration drives traffic with no gateway restart ---
curl -s -X DELETE "$G/nacos/v1/ns/instance?serviceName=e2e-api&ip=127.0.0.1&port=$UP_PORT" >/dev/null
sleep 2
check "no instances -> 503, not a hang" "$(curl -sk -o /dev/null -w '%{http_code}' --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "https://e2e.test:$GW_HTTPS/api/x")" "503"
curl -s -X POST "$G/nacos/v1/ns/instance" -d "serviceName=e2e-api&ip=127.0.0.1&port=$UP_PORT&healthy=true&enabled=true&ephemeral=false" >/dev/null
sleep 2
check "instance re-registration restores traffic" "$(curl -sk --resolve "e2e.test:$GW_HTTPS:127.0.0.1" "https://e2e.test:$GW_HTTPS/api/back" | python3 -c 'import json,sys; print(json.load(sys.stdin)["upstream"])')" "acc-api"

# --- body limit and websocket policy are per-domain ---
curl -s -X POST "$G/goacos/v1/vhosts" -H 'Content-Type: application/json' -d '{"domain":"e2e.test","clientMaxBodyBytes":16}' >/dev/null
sleep 2
check "oversized body rejected" "$(curl -sk -o /dev/null -w '%{http_code}' --resolve "e2e.test:$GW_HTTPS:127.0.0.1" -X POST -d 'this body is definitely longer than sixteen bytes' "https://e2e.test:$GW_HTTPS/api/upload")" "413"

check "gateway status reports routing" "$(curl -s "$G/goacos/v1/gateway/status" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["enabled"], d["routing"]["domainCount"]>0)')" "True True"
check "gateway status reports acme off" "$(curl -s "$G/goacos/v1/gateway/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["acme"]["enabled"])')" "False"

if [ "$ENGINE" = mysql ]; then
  GT=$(docker exec "$DB_CONTAINER" mysql -uroot -p"$DB_PASS" -N -B -e "SHOW TABLES FROM $DB LIKE 'gateway%'" 2>/dev/null | tr '\n' ',')
else
  GT=$(docker exec "$DB_CONTAINER" psql -U postgres -d "$DB" -tAc "SELECT table_name FROM information_schema.tables WHERE table_name LIKE 'gateway%' ORDER BY table_name" 2>/dev/null | tr '\n' ',')
fi
case "$GT" in *gateway_certificate*) ok "gateway tables created ($GT)";; *) bad "gateway tables (got '$GT')";; esac

say "== gateway certificate directory =="
# A certificate dropped into the mounted directory must be served without ever
# being copied into the database. This is the whole point of the feature: it
# lets an existing certbot or internal CA keep ownership of the material.
mkdir -p "$GW_CERTS/ext.example.com"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$GW_CERTS/ext.example.com/privkey.pem" \
  -out "$GW_CERTS/ext.example.com/fullchain.pem" -days 2 -subj "/CN=ext.example.com" \
  -addext "subjectAltName=DNS:ext.example.com" >/dev/null 2>&1
curl -s -X POST "$G/goacos/v1/apps" -H 'Content-Type: application/json' -d '{"name":"ext","env":"test"}' >/dev/null
curl -s -X POST "$G/goacos/v1/apps/ext/components" -H 'Content-Type: application/json' \
  -d '{"name":"api","kind":"api","protocol":"http","serviceName":"ext-api"}' >/dev/null
curl -s -X POST "$G/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"ext.example.com","appName":"ext","componentName":"api"}' >/dev/null
curl -s -X POST "$G/nacos/v1/ns/instance" -d "serviceName=ext-api&ip=127.0.0.1&port=$UP_PORT&healthy=true&enabled=true&ephemeral=false" >/dev/null
sleep 4

check "cert dir certificate is served" "$(echo | openssl s_client -connect "127.0.0.1:$GW_HTTPS" -servername ext.example.com 2>/dev/null | openssl x509 -noout -subject 2>/dev/null | grep -c 'ext.example.com')" "1"
check "cert dir source is reported" "$(curl -s "$G/goacos/v1/gateway/status" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print("ext.example.com" in d["certDir"]["servedFromDir"])')" "True"
check "cert dir request reaches instance" "$(curl -sk --resolve "ext.example.com:$GW_HTTPS:127.0.0.1" "https://ext.example.com:$GW_HTTPS/api/x" | python3 -c 'import json,sys; print(json.load(sys.stdin)["upstream"])')" "acc-api"

# A wildcard certificate on disk must serve a single-label subdomain. Without
# the wildcard lookup it would load successfully and then never be used.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$GW_CERTS/wild.pem" \
  -out "$GW_CERTS/wild.crt" -days 2 -subj "/CN=*.wild.example.com" \
  -addext "subjectAltName=DNS:*.wild.example.com" >/dev/null 2>&1
curl -s -X POST "$G/goacos/v1/routes" -H 'Content-Type: application/json' \
  -d '{"domain":"api.wild.example.com","appName":"ext","componentName":"api"}' >/dev/null
sleep 3
check "wildcard cert from dir is served" "$(echo | openssl s_client -connect "127.0.0.1:$GW_HTTPS" -servername api.wild.example.com 2>/dev/null | openssl x509 -noout -subject 2>/dev/null | grep -c 'wild.example.com')" "1"

# Deleting the database row must say plainly that a file may still be serving.
check "cert delete mentions the file" "$(curl -s -X DELETE "$G/goacos/v1/certs/ext.example.com" | python3 -c 'import json,sys; print("file" in json.load(sys.stdin)["data"]["note"])')" "True"

say "== auth (second instance, same DB) =="
GOACOS_DB_TYPE=$ENGINE \
GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_PORT=$DB_PORT GOACOS_MYSQL_DB=$DB \
GOACOS_MYSQL_USER=$DB_USER GOACOS_MYSQL_PASSWORD="$DB_PASS" GOACOS_PORT=$((HTTP_PORT+1)) GOACOS_AUTH_ENABLED=true \
"$BIN" serve >/dev/null 2>&1 &
SRV2_PID=$!
B2="http://127.0.0.1:$((HTTP_PORT+1))"
for i in $(seq 1 20); do curl -sf "$B2/nacos/actuator/health" >/dev/null && break; sleep 0.5; done
check "auth: no token -> 403" "$(curl -s -o /dev/null -w '%{http_code}' "$B2/nacos/v1/cs/configs?search=blur&pageNo=1&pageSize=1")" "403"
check "auth: bad login -> 401" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B2/nacos/v1/auth/users/login" -d 'username=nacos&password=nope')" "401"
TOKEN=$(curl -s -X POST "$B2/nacos/v1/auth/users/login" -d 'username=nacos&password=nacos' | python3 -c 'import json,sys; print(json.load(sys.stdin).get("accessToken",""))')
[ -n "$TOKEN" ] && ok "auth: admin login issued JWT" || bad "auth: admin login"
check "auth: token accepted" "$(curl -s -o /dev/null -w '%{http_code}' "$B2/nacos/v1/cs/configs?search=blur&pageNo=1&pageSize=1&accessToken=$TOKEN")" "200"

say "== console =="
check "console page" "$(curl -s "$B/nacos/index.html" | head -c 15)" "<!DOCTYPE html>"
check "server state" "$(curl -s "$B/nacos/v1/console/server/state" | python3 -c 'import json,sys; print(json.load(sys.stdin)["functionMode"])')" "ALL"

say ""
say "RESULT: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = 0 ] || exit 1
