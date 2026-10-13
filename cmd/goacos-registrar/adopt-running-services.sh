#!/usr/bin/env bash
# Adopt already-running services into goacos without touching their code.
#
# # Why this exists
#
# The coverage audit found sixteen services with no registration code at all.
# Wiring each one in-process means publishing a new SDK tag, editing its
# repository, and re-testing it — a code change to a service that did nothing
# wrong and needs nothing changed.
#
# For a service that is simply *there*, listening on a port, none of that is
# necessary. This script runs goacos-registrar against it and the service joins
# the same load-balanced pool as everything else. The service is not restarted,
# reconfigured, or modified; the registrar only publishes an address something
# else already serves.
#
# That makes it the right tool for migration, and the wrong tool for a service
# whose lifecycle you want registration tied to. If the process dies, a sidecar
# registrar may outlive it and keep publishing a dead address; in-process
# registration cannot. Use this to adopt what exists, then move services to
# in-process registration as they are next touched.
#
# # Usage
#
#   ./adopt-running-services.sh <registry> <advertise-ip> <name:port> [name:port ...]
#
#   ./adopt-running-services.sh http://127.0.0.1:8848 192.168.31.19 \
#       memora:8091 marketing:3208 smm-web:8100
#
#   Add --dry-run to print the plan and exit without registering anything.
#
# # Per-service address override
#
#   <name>:<port>            → use the registry-wide advertise-ip
#   <name>:<port>@<ip>       → use <ip> for this service only
#
# Why this exists: on a host where services are Docker containers, the right
# address is NOT the same for every service. Measured on 211 (2026-10-14), of
# 14 business services only 8 publish a port on 0.0.0.0 — so the host's mesh
# address is reachable for those 8 and nothing for the rest:
#
#   publishes 0.0.0.0  (8)  acc-go-211, smm-web, memora-server, llm-gateway-211,
#                         official-portal, marketing, opencode-pocket-{frontend,pocketd}
#                         → 100.106.67.198:<hostPort>, reachable from any host
#   publishes 127.0.0.1 (1)  nbjl-app
#                         → 127.0.0.1:18081 only
#   publishes nothing  (5)  companion-211, smm, smm-mcp, memora-worker, acc-blue
#                         → the container's bridge address, same host only
#
# A single advertise-ip therefore either skips the five or publishes addresses
# nothing can dial. The @ form lets one invocation cover the mixed reality,
# and --dry-run lets you see the split before registering anything.
#
# Requires: curl, python3, and goacos-registrar on PATH (or REGISTRAR=<path>).
# Run it from the directory of a host where those services actually listen.
set -u

REGISTRAR="${REGISTRAR:-goacos-registrar}"

DRY_RUN=0
plan=()
for arg in "$@"; do
	if [ "$arg" = "--dry-run" ]; then DRY_RUN=1; continue; fi
	plan+=("$arg")
done
# bash 3.2 safe: the ${arr[@]+"${arr[@]}"} guard expands to nothing when the
# array is empty, which plain "${plan[@]}" would not.
set -- ${plan[@]+"${plan[@]}"}

if [ "$#" -lt 3 ]; then
	sed -n '3,45p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
fi

REGISTRY="$1"; shift
ADVERTISE_IP="$1"; shift

if [ "$#" -eq 0 ]; then
	echo "至少要给一个 <名字>:<端口>" >&2
	exit 2
fi

fail=0
note() { printf '%-52s %s\n' "$1" "$2"; }
ok()   { printf '  ✅ %s\n' "$1"; }
bad()  { printf '  ❌ %s\n' "$1"; fail=1; }

# hosts_of prints the instance list with compact separators.
#
# Compact matters: json.dumps defaults to `"k": v` with a space, and a shell
# pattern written against the compact form then matches nothing — the check
# fails on correct data and reports a defect that does not exist.
hosts_of() {
	curl -s --max-time 3 "$REGISTRY/nacos/v2/ns/instance/list?serviceName=$1&groupName=DEFAULT_GROUP&clusters=DEFAULT&healthyOnly=false" |
		python3 -c "import sys,json; print(json.dumps(json.load(sys.stdin)['data']['hosts'],separators=(',',':')))" 2>/dev/null
}

reachable() { [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://$1/" 2>/dev/null)" != "000" ]; }

note "注册器" "$(command -v "$REGISTRAR" || echo "$REGISTRAR")"
note "注册中心" "$REGISTRY"
note "通告地址" "$ADVERTISE_IP"

pids=(); names=(); ports=(); ips=()

echo
echo "=== 1. 逐个启动注册器 ==="
for spec in "$@"; do
	# name:port[@ip] — the @ip form overrides the registry-wide address for
	# this service only. Split on @ first, then on the LAST colon so a name
	# containing one is not truncated.
	svc_ip="$ADVERTISE_IP"
	body="$spec"
	case "$spec" in
	*@*) svc_ip="${spec##*@}"; body="${spec%@*}" ;;
	esac
	name="${body%%:*}"; port="${body##*:}"

	if [ -z "$name" ] || [ -z "$port" ] || [ "$name" = "$body" ]; then
		bad "$spec 格式不对，要 <名字>:<端口>[@地址]"
		continue
	fi
	if ! reachable "$svc_ip:$port"; then
		bad "$name:$port 在 $svc_ip 上不响应，跳过（先别发布一个没人服务的端口）"
		continue
	fi
	if [ "$DRY_RUN" = "1" ]; then
		ok "$name:$port → $svc_ip:$port"
		names+=("$name"); ports+=("$port"); ips+=("$svc_ip")
		continue
	fi
	"$REGISTRAR" -server "$REGISTRY" -service "$name" -port "$port" \
		-advertise-ip "$svc_ip" -role backend -meta tier=adopted \
		>"/tmp/adopt-$name.log" 2>&1 &
	pids+=("$!"); names+=("$name"); ports+=("$port"); ips+=("$svc_ip")
	echo "  $name -> $svc_ip:$port (pid $!)"
done

if [ "$DRY_RUN" = "1" ]; then
	echo
	echo "DRY_RUN：不会启动任何注册器，注册中心未做任何变更。"
	echo "上面 ${#names[@]} 个服务将被注册；去掉 --dry-run 才会真正执行。"
	[ "$fail" -eq 0 ] && echo "计划可行。" || echo "计划中有跳过项，见上方 ❌。"
	exit "$fail"
fi

sleep 3

echo
echo "=== 2. 注册中心视角 ==="
for i in "${!names[@]}"; do
	name="${names[$i]}"; port="${ports[$i]}"; svc_ip="${ips[$i]}"
	if ! grep -q "msg=registered" "/tmp/adopt-$name.log" 2>/dev/null; then
		bad "$name 未注册成功：$(tail -1 "/tmp/adopt-$name.log" 2>/dev/null)"
		continue
	fi
	h="$(hosts_of "$name")"
	case "$h" in
	*"\"ip\":\"$svc_ip\""*"\"port\":$port"*) ok "$name 上线的是 $svc_ip:$port" ;;
	*) bad "$name 上线地址不符：$h" ;;
	esac
	case "$h" in
	*'"metadata":{"role":"backend","tier":"adopted"}'*) ok "$name 元数据正确" ;;
	*) bad "$name 元数据不符：$h" ;;
	esac
done

echo
echo "=== 3. 服务本身照常服务（注册器没有接管任何流量）==="
for i in "${!names[@]}"; do
	if reachable "${ips[$i]}:${ports[$i]}"; then ok "${names[$i]} 仍在响应" ; else bad "${names[$i]} 不响应了"; fi
done

echo
echo "=== 4. 停机：注销且不留幽灵 ==="
for pid in "${pids[@]}"; do kill -TERM "$pid" 2>/dev/null; done
sleep 2
for name in "${names[@]}"; do
	if grep -q "msg=deregistered" "/tmp/adopt-$name.log" 2>/dev/null; then ok "$name 已注销"; else bad "$name 没看到注销"; fi
	case "$(hosts_of "$name")" in
	'[]'*) ok "$name 无幽灵实例" ;;
	*) bad "$name 有残留：$(hosts_of "$name")" ;;
	esac
done

echo
[ "$fail" -eq 0 ] && echo "全部通过。" || echo "有失败项，见上方 ❌。"
exit "$fail"
