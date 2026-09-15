#!/usr/bin/env bash
# goacos end-to-end acceptance: boots (or reuses) a MySQL 8 container, starts
# goacos against a fresh database, and drives the Nacos-compatible API surface.
# Usage: scripts/e2e.sh [--keep]
set -uo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN=/tmp/goacos-e2e-server
MYSQL_NAME=goacos-e2e-mysql
MYSQL_PORT=${MYSQL_PORT:-13310}
MYSQL_PASS=${MYSQL_PASS:-e2eroot}
HTTP_PORT=${HTTP_PORT:-18848}
DB="goacos_e2e_$(date +%s)"
PASS=0; FAIL=0

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $1"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want '$3' got '$2')"; fi }

cleanup() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  [ -n "${SRV2_PID:-}" ] && kill "$SRV2_PID" 2>/dev/null
  if [ "$KEEP" != 1 ]; then
    docker rm -f "$MYSQL_NAME" >/dev/null 2>&1
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
for P in "$HTTP_PORT" "$((HTTP_PORT+1))"; do
  if nc -z 127.0.0.1 "$P" 2>/dev/null; then
    say "port $P is busy — free it and retry"; exit 1
  fi
done

say "== mysql =="
if ! nc -z 127.0.0.1 "$MYSQL_PORT" 2>/dev/null; then
  say "  starting mysql container on :$MYSQL_PORT"
  docker run -d --name "$MYSQL_NAME" -e MYSQL_ROOT_PASSWORD="$MYSQL_PASS" \
    -p "127.0.0.1:$MYSQL_PORT:3306" mysql:8.0 >/dev/null || exit 1
  for i in $(seq 1 60); do
    docker exec "$MYSQL_NAME" mysqladmin ping -uroot -p"$MYSQL_PASS" --silent >/dev/null 2>&1 && break
    sleep 2
  done
fi
docker exec "$MYSQL_NAME" mysql -uroot -p"$MYSQL_PASS" -e "SELECT 1" >/dev/null 2>&1 || {
  say "mysql not reachable; container status/logs:"
  docker ps -a --format '{{.Names}}\t{{.Status}}' | grep "$MYSQL_NAME" || true
  docker logs --tail 20 "$MYSQL_NAME" 2>&1 || true
  exit 1
}

say "== start goacos (fast sweep for lifecycle test) =="
SRV_LOG=/tmp/goacos-e2e-server.log
GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_PORT=$MYSQL_PORT GOACOS_MYSQL_DB=$DB \
GOACOS_MYSQL_USER=root GOACOS_MYSQL_PASSWORD="$MYSQL_PASS" GOACOS_PORT=$HTTP_PORT \
GOACOS_HEARTBEAT_TIMEOUT_MS=2000 GOACOS_EPHEMERAL_DELETE_AFTER_MS=4000 \
GOACOS_SWEEP_INTERVAL_MS=1000 \
"$BIN" serve >"$SRV_LOG" 2>&1 &
SRV_PID=$!
B="http://127.0.0.1:$HTTP_PORT"

for i in $(seq 1 20); do curl -sf "$B/nacos/actuator/health" >/dev/null && break; sleep 0.5; done
check "health" "$(curl -s -o /dev/null -w '%{http_code}' "$B/nacos/actuator/health")" "200"
docker exec "$MYSQL_NAME" mysql -uroot -p"$MYSQL_PASS" -e "USE $DB; SHOW TABLES" 2>/dev/null | grep -q config_info \
  && ok "auto-created database + schema" || bad "auto-created database + schema"

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

say "== auth (second instance, same DB) =="
GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_PORT=$MYSQL_PORT GOACOS_MYSQL_DB=$DB \
GOACOS_MYSQL_USER=root GOACOS_MYSQL_PASSWORD="$MYSQL_PASS" GOACOS_PORT=$((HTTP_PORT+1)) GOACOS_AUTH_ENABLED=true \
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
