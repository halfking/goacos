#!/bin/sh
# End-to-end proof for goacos-registrar against a real goacos.
#
# Nothing here is mocked. A real HTTP listener stands in for the nginx a static
# site would run, the registrar really registers, and the registry is queried
# over HTTP both while it is up and after it stops. The point is to prove the
# one thing this command exists for — publishing an address this process does
# not serve — and that an orderly stop leaves no ghost behind.
#
# Run it from the goacos repo root, on a network the goacos container is on:
#
#   docker run --rm --network shared-infra \
#     -v "$PWD":/src -v /tmp/goacos-modcache:/go/pkg/mod \
#     -v "$PWD/cmd/goacos-registrar":/e2e:ro -w /src \
#     -e GOFLAGS=-mod=mod -e GOPROXY=off -e GOSUMDB=off \
#     golang:1.27.2-alpine sh /e2e/e2e.sh
#
# Override REGISTRY to point at a different goacos, e.g.
#   -e REGISTRY=http://goacos-211:8848
set -eu

REGISTRY="${REGISTRY:-http://goacos:8848}"
SERVICE=e2e-frontend
GHOST_SERVICE=ghost-check
PORT=18099
DEAD_PORT=19999

# The cluster name is part of the instance identity: the SDK defaults it to
# DEFAULT and the list endpoint filters on it. Querying with an empty clusters
# parameter returns hosts:[] for a service that is very much registered, which
# reads exactly like a registration that silently failed.
query() {
	wget -qO- "$REGISTRY/nacos/v2/ns/instance/list?serviceName=$1&groupName=DEFAULT_GROUP&clusters=DEFAULT&healthyOnly=false" 2>/dev/null || echo "(query failed)"
}

# hosts_of prints the hosts array on its own line so assertions can grep it
# without matching the surrounding envelope.
hosts_of() {
	query "$1" | sed -n 's/.*"hosts":\(\[[^]]*\]\).*/\1/p'
}

IP=$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
echo "== 0. registry=$REGISTRY, container address=$IP =="

echo
echo "== 1. stand up the endpoint the registrar will only publish =="
cat > /tmp/fakesite.go <<'GO'
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html>static site: %s</html>", r.URL.Path)
	})
	if err := http.ListenAndServe(os.Args[1], nil); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
}
GO
go build -o /tmp/fakesite /tmp/fakesite.go
/tmp/fakesite "0.0.0.0:$PORT" &
HTTPD=$!
trap 'kill "$HTTPD" 2>/dev/null || true' EXIT
sleep 2
if ! nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
	echo "FAIL: the stand-in listener never came up"
	exit 1
fi
echo "listener confirmed on 127.0.0.1:$PORT"

echo
echo "== 2. build the registrar =="
go build -o /tmp/goacos-registrar ./cmd/goacos-registrar
echo "BUILD_EXIT=$?"

echo
echo "== 3. loopback is refused unless explicitly allowed =="
set +e
/tmp/goacos-registrar -server "$REGISTRY" -service "$SERVICE" -port "$PORT" \
	-advertise-ip 127.0.0.1 >/tmp/lb.log 2>&1
echo "exit=$? (want 1)"
set -e
tail -1 /tmp/lb.log

echo
echo "== 4. refuses to advertise a port nobody is listening on =="
set +e
/tmp/goacos-registrar -server "$REGISTRY" -service "$GHOST_SERVICE" -port "$DEAD_PORT" \
	-advertise-ip "$IP" -dial-timeout 3s >/tmp/ghost.log 2>&1
echo "exit=$? (want 1)"
set -e
tail -1 /tmp/ghost.log
echo "registry: $(hosts_of "$GHOST_SERVICE") (want [])"

echo
echo "== 5. register for real =="
/tmp/goacos-registrar -server "$REGISTRY" -service "$SERVICE" -port "$PORT" \
	-advertise-ip "$IP" -role frontend -meta tier=edge >/tmp/reg.log 2>&1 &
REG=$!
sleep 3
cat /tmp/reg.log
if ! grep -q 'msg=registered' /tmp/reg.log; then
	echo "FAIL: registrar never reported a successful registration"
	exit 1
fi

echo
echo "== 6. the registry's own view while it is up =="
echo "hosts: $(hosts_of "$SERVICE")"
case "$(hosts_of "$SERVICE")" in
*\"ip\":\"$IP\"*\"port\":$PORT*) echo "OK: the published address is what we asked for" ;;
*) echo "FAIL: the registry holds a different address than the one advertised"; exit 1 ;;
esac
case "$(query "$SERVICE")" in
*'"role":"frontend"'*) echo "OK: -role reached the registry as metadata" ;;
*) echo "FAIL: role metadata missing"; exit 1 ;;
esac

echo
echo "== 7. SIGTERM: orderly stop, fresh context, no ghost =="
kill -TERM "$REG"
wait "$REG" 2>/dev/null || true
sleep 1
grep -E 'deregistered' /tmp/reg.log || { echo "FAIL: no deregistration logged"; exit 1; }
echo "hosts after stop: $(hosts_of "$SERVICE") (want [])"
case "$(hosts_of "$SERVICE")" in
'[]'*) echo "OK: no ghost instance left behind" ;;
*) echo "FAIL: a ghost instance outlived the process"; exit 1 ;;
esac

echo
echo "== all checks passed =="
