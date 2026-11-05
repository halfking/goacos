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
# # Per-service identity check
#
#   <name>:<port>[@<ip>][#<identity>]
#
# The optional #<identity> says which container the declaration claims this
# service is. When it is present, the script checks that this port is
# published by a container OF THAT IDENTITY and skips the entry otherwise.
#
# Why this exists: the reachability probe only proves that SOMETHING answers
# on the port. A 404 from a different service is still an answer, and a port
# published by another container is still a reachable port — so without the
# identity check the registrar publishes a healthy instance under the
# declared name that points at somebody else's service.
#
# Measured 2026-10-24 on 252: nbjl-app declares port 18081, and
# netbird-management publishes 127.0.0.1:18081. The container answers 404, so
# the probe passed, and the entry would have been registered against the
# netbird management port. The two tools disagreed about the same collision:
# the reconciliation report called it PORT_ONLY and refused to adopt it,
# while this script would have adopted it silently.
#
# # An identity may be a LITERAL or a FAMILY
#
# A literal names one container. A family uses * and ? and matches a set of
# containers:
#
#   acc-go-211                  literal: exactly that container
#   acc-go-*                    family:  acc-go-211, acc-go-252, acc-go-blue
#   redclaw-*-redclaw-api-1     family:  the api container of any redclaw stack
#
# WHY THE FAMILY FORM EXISTS
#
# A literal is the container's name **on the machine the declaration was
# written for**, and deployments rename containers per machine. So a literal
# declaration only works on its own machine:
#
#   declared  redclaw-local-stack-redclaw-api-1   (the itestu/211 stack)
#   live      redclaw-252-redclaw-api-1           (252)
#   declared  acc-go-211           live  acc-go-252
#   declared  kx-marketing-local   live  kx-marketing
#
# Measured 2026-10-24 on 252: of the 15 declarations that hold on that host,
# only 3 get past a literal comparison — two because the names match
# verbatim, one (acc-go, a host-network container) because `ps` shows no port
# holder at all and the check therefore never runs. The other 12 are skipped.
#
# That is the opposite of what a service registry is for. The requirement is
# that services may run on MANY instances across MANY hosts, and that
# requests are split across whatever is alive — a declaration that pins the
# instance to one machine's container name cannot express that: it describes
# one host, not a service.
#
# The family form keeps the safety property — the port's publisher must
# match the declared identity, so somebody else's port is still refused —
# while making ONE declaration usable on every host. A literal with no
# wildcard behaves exactly as before, character for character.
#
# The family must still contain literal characters. A bare * would match any
# container, which is not an identity check — it is the absence of one, and
# the script reports the absence of a check rather than pretending it ran.
#
# The per-machine rename itself is an OPEN decision — see
# evidence/DECLARATION-PATH-UNSCHEDULED-AND-NAME-DIVERGENCE-2026-10-16.md
# §3.9 in the myvpn repo. The family form is what lets one declaration be
# used on both sides of that decision without duplicating the file.
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
	# 按**标记**切片，不按行号。
	#
	# 原来是 `sed -n '3,45p'` —— 那是行号，而这个文件的头部会随说明增长。
	# 一旦头部变长，切片就从句子里截断（实测：头部加长后用法提示停在
	# "only proves that SOMETHING answers"），于是最需要看清用法的那一次，
	# 看到的是半句话。
	awk '/^# # Usage/{f=1} /^# Requires:/{f=0} f{sub(/^# ?/,""); print}' "$0"
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

# container_publishing_port prints the names of the containers that publish
# <port> on THIS host, one per line. Nothing is printed when no container
# publishes it — the port may belong to a host process instead.
#
# Both runtimes are asked, because which one is in use is a property of the
# host, not of this script: 252 runs podman, 211 runs docker.
#
# The pattern is deliberately specific about a PUBLISHED port. The two
# runtimes print ports differently:
#
#   podman:  80/tcp -> 127.0.0.1:18081
#   docker:  0.0.0.0:18081->8080/tcp
#
# so the host port is the one preceded by an address. A container port
# (18081/tcp) has no address before it and must NOT match: a container that
# merely exposes 18081 internally publishes nothing on the host.
#
# The address part is "anything up to whitespace" rather than an IPv4
# pattern, so an IPv6 publication ([::1]:18081) is recognised too.
#
# ONE runtime per host. The first available runtime is the one that is
# queried, and the other is never consulted. Mixing two runtimes' views of
# the same host produces nonsense: a port that podman does not publish would
# be attributed to a container of the other runtime, and the entry would be
# refused for a reason that has nothing to do with it. (Seen on a machine
# with both installed: podman published nothing, docker published
# everything, and every entry was refused against the docker containers.)
container_publishing_port() {
	local port="$1" out="" cli
	for cli in podman docker; do
		if command -v "$cli" >/dev/null 2>&1; then
			out="$("$cli" ps --format '{{.Names}}|{{.Ports}}' 2>/dev/null |
				grep -E "[^[:space:]]+:${port}([^0-9]|$)" | cut -d'|' -f1)"
			printf '%s\n' "$out"
			return 0
		fi
	done
	return 0
}

# identity_is_valid says whether an identity may be used as a match pattern.
#
# The charset is restricted on purpose: `case` reads [ as the start of a
# bracket expression, so an identity carrying one would silently mean
# something the author never wrote. Refusing it beats matching the wrong
# containers.
identity_is_valid() {
	case "$1" in
		'') return 1 ;;
		*[!A-Za-z0-9._?*-]*) return 1 ;;
		*) return 0 ;;
	esac
}

# identity_has_literal says whether the identity keeps at least one
# character that is not a wildcard.
#
# Without this, `*` would match every container and the identity check would
# pass for anything — which is not a check, it is the absence of one. The
# script reports the absence of a check; it must not be able to smuggle one
# in wearing a pattern's clothes.
identity_has_literal() {
	case "$1" in
		*[A-Za-z0-9._]*) return 0 ;;
		*) return 1 ;;
	esac
}

# identity_matches says whether a container name belongs to an identity.
#
# A literal identity (no wildcard) matches exactly that name; an identity
# with wildcards matches by glob. The pattern is deliberately NOT quoted —
# quoting would make every wildcard literal and turn a family back into a
# single machine's container name.
identity_matches() {
	case "$1" in
		$2) return 0 ;;
		*) return 1 ;;
	esac
}

# identity_subject words an identity as a subject of a sentence. A family is
# not one container, and saying "容器 acc-go-*" would be a lie about how
# many things it can be.
identity_subject() {
	case "$1" in
		*'*'*|*'?'*) printf '身份族 %s' "$1" ;;
		*) printf '容器 %s' "$1" ;;
	esac
}

# identity_claim words what the declaration claimed. Kept separate from
# identity_subject so the literal case keeps its original wording exactly.
identity_claim() {
	case "$1" in
		*'*'*|*'?'*) printf '声明的身份族是 %s' "$1" ;;
		*) printf '声明的容器是 %s' "$1" ;;
	esac
}

# published_ports_of_identity prints the host ports published by every
# container that belongs to <identity>, one per line. Nothing is printed when
# none of them publishes one.
#
# It is the family-aware twin of container_publishing_port: that one answers
# "who publishes this port?", this one answers "which ports does this
# identity publish?" — and the second question is the one the registration
# contract actually needs.
#
# The declaration carries a literal hostPort. A literal is wrong the moment the
# service moves, and services do move: llm-gateway's active blue-green slot
# rotates between 8781 and 8782 (the repo says so itself), so "register the
# declared port" is right today and wrong tomorrow, with nothing in between to
# notice.
#
# The extraction is the mirror of the one above: the HOST port is the number
# that follows an address. A container port (18081/tcp) has no address before
# it and must NOT match — a container that merely exposes a port internally
# publishes nothing on the host.
#
# With a literal identity exactly one container matches, so this behaves
# exactly like the per-container version it replaces. With a family the ports
# of every member are collected, because the service may have moved to any of
# them.
published_ports_of_identity() {
	local pattern="$1" name ports cli
	for cli in podman docker; do
		if command -v "$cli" >/dev/null 2>&1; then
			"$cli" ps --format '{{.Names}}|{{.Ports}}' 2>/dev/null |
			while IFS='|' read -r name ports; do
				[ -z "$name" ] && continue
				identity_matches "$name" "$pattern" || continue
				printf '%s\n' "$ports" | grep -oE ':[0-9]+' | cut -d: -f2
			done
			return 0
		fi
	done
	return 0
}

note "注册器" "$(command -v "$REGISTRAR" || echo "$REGISTRAR")"
note "注册中心" "$REGISTRY"
note "通告地址" "$ADVERTISE_IP"

pids=(); names=(); ports=(); ips=()

echo
echo "=== 1. 逐个启动注册器 ==="
for spec in "$@"; do
	# name:port[@ip][#identity] — the @ip form overrides the registry-wide
	# address for this service only, the #identity form names the container
	# (or the container family) the declaration says this service is. Split on
	# # first, then on the LAST colon, so a name containing one is not
	# truncated.
	svc_ip="$ADVERTISE_IP"
	body="$spec"
	expect_identity=""
	case "$spec" in
	*#*) expect_identity="${spec##*#}"; body="${spec%#*}" ;;
	esac
	case "$body" in
	*@*) svc_ip="${body##*@}"; body="${body%@*}" ;;
	esac
	name="${body%%:*}"; port="${body##*:}"

	if [ -z "$name" ] || [ -z "$port" ] || [ "$name" = "$body" ]; then
		bad "$spec 格式不对，要 <名字>:<端口>[@地址][#容器]"
		continue
	fi

	# The identity must be usable as a pattern, and it must actually constrain
	# something. Both refusals happen BEFORE anything is registered: a
	# malformed pattern would silently match something the author never wrote,
	# and a pattern with no literal characters is the absence of a check
	# wearing a check's clothes.
	if [ -n "$expect_identity" ]; then
		if ! identity_is_valid "$expect_identity"; then
			bad "$spec 的容器身份 '$expect_identity' 含非法字符（只允许字母数字与 . _ - * ?）—— 跳过（不猜它该匹配谁）"
			continue
		fi
		if ! identity_has_literal "$expect_identity"; then
			bad "$spec 的容器身份 '$expect_identity' 没有任何字面量字符 —— 跳过（它匹配所有容器，等于没有核对）"
			continue
		fi
	fi

	# The port comes from the container's CURRENT published port, not from the
	# declaration's literal.
	#
	# The declaration's hostPort is a snapshot of one moment. A service that
	# moves (llm-gateway's active slot rotates between 8781 and 8782) makes
	# that snapshot wrong, and a literal that is wrong produces an instance
	# that registers healthy and answers nothing.
	#
	# Three rules, in order:
	#   * the declared port is still published → nothing changed, keep it;
	#   * exactly one other port is published → the port moved unambiguously,
	#     use the live one and say so;
	#   * several ports are published → **do not guess**. Which one is the
	#     service port is not knowable from here, and registering the wrong one
	#     produces a healthy instance pointing at somebody else's service.
	#     Skip it and let a human look.
	#
	# Nothing is derived when the container publishes no port at all: that is
	# what host-network containers look like (measured 2026-10-24 on 252:
	# `acc-go-252|` — empty Ports column, although it listens on 4101). There
	# the declaration stays authoritative and the note below says the identity
	# could not be checked.
	declared_port="$port"
	if [ -n "$expect_identity" ]; then
		published="$(published_ports_of_identity "$expect_identity" | sort -u)"
		if [ -n "$published" ]; then
			if printf '%s\n' "$published" | grep -qx "$declared_port"; then
				: # 声明端口仍在发布 ⇒ 端口没漂
			elif [ "$(printf '%s\n' "$published" | wc -l | tr -d ' ')" = "1" ]; then
				port="$(printf '%s\n' "$published" | head -1)"
				note "$name 的端口已漂移" "声明 ${declared_port}，$(identity_subject "$expect_identity") 当前发布 $port ⇒ 按实际发布端口注册"
			else
				bad "$name 声明端口 $declared_port 已不在发布列表，$(identity_subject "$expect_identity") 发布了 $(printf '%s' "$published" | tr '\n' ' ') —— 跳过（不猜哪个才是服务端口）"
				continue
			fi
		fi
	fi

	if ! reachable "$svc_ip:$port"; then
		bad "$name:$port 在 $svc_ip 上不响应，跳过（先别发布一个没人服务的端口）"
		continue
	fi

	# The identity check.
	#
	# Reachability only proves that SOMETHING answers on the port. A 404 from
	# a different service is still an answer, and a port published by another
	# container is still a reachable port — so without this check the
	# registrar publishes a healthy instance under the declared name that
	# points at somebody else's service, and nothing in the registry shows
	# that anything is wrong.
	#
	# Measured 2026-10-24 on 252: nbjl-app declares port 18081 and
	# netbird-management publishes 127.0.0.1:18081; the container answers
	# 404, so the probe passes.
	if [ -n "$expect_identity" ]; then
		publishers="$(container_publishing_port "$port")"
		if [ -n "$publishers" ]; then
			# The publisher must belong to the declared identity.
			#
			# A literal identity matches one container exactly; a family matches
			# any container of that family. Either way the question is the same:
			# is the thing holding this port the service the declaration claims?
			#
			# The publisher list is read with a here-string so the loop runs in
			# THIS shell: in a pipeline it would run in a subshell and the
			# verdict it computed would be lost on the way out.
			matched=""
			while IFS= read -r publisher; do
				[ -z "$publisher" ] && continue
				if identity_matches "$publisher" "$expect_identity"; then
					matched="$matched $publisher"
				fi
			done <<< "$publishers"
			if [ -z "$matched" ]; then
				bad "$name:$port 由容器 $(printf '%s' "$publishers" | tr '\n' ' ') 占用，$(identity_claim "$expect_identity") —— 跳过（别把别人的服务注册成自己的名字）"
				continue
			fi
		else
			# 查不到占用者 ≠ 核对通过。
			#
			# host 网络模式的容器在 `ps` 的 Ports 列里**是空的**
			# （实测 2026-10-24 252：`acc-go-252|`，尽管它监听 4101）
			# ⇒ 这一关根本没有执行。沉默地放行 = 假装核对过了。
			note "$name:$port 的容器身份未核对" "ps 视图里看不到该端口的占用者（host 网络模式的容器不显示发布端口），无法确认占用者是谁"
		fi
	elif [ -n "$(container_publishing_port "$port")" ]; then
		note "$name:$port 的容器身份未核对" "这条没带 #<容器>，无法确认占用者是谁"
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
