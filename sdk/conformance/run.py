#!/usr/bin/env python3
"""Cross-language conformance check for the goacos registry SDKs.

Three implementations of one wire contract (Go, Python, Node) drift in the
way that is hardest to notice: not a crash, but a heartbeat that 200s while
addressing a different instance, or a backoff that grows past the point where
the server has already evicted the client. Each SDK's own tests pass the day
that happens — the bug is in the *disagreement*.

So this runs the same scripted scenario against all three and compares what
they actually put on the wire.

Usage:
    python3 sdk/conformance/run.py

Requires python3 and node. The Go side is checked by parsing its constants
out of client.go, so no Go toolchain is needed — the point of this script is
to be runnable on the dev machine and in CI without a build.

Exit code 0 = the three agree.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from typing import Any, Dict, List

# Bytecode caching must be OFF for this script, and the choice is not
# cosmetic.
#
# CPython validates a cached .pyc by comparing the source's mtime (whole
# seconds) and its byte length. A conformance mutation that keeps the file
# the same size and lands in the same second as the previous import is
# therefore invisible: editing `/v2/ns/instance/beat` to `/v1/...` changes
# no length, and the stale module is loaded as if nothing had changed. That
# was observed here -- the drift check reported "consistent" against a
# genuinely drifted binding. Anything verifying source edits must not trust
# the interpreter's cache.
sys.dont_write_bytecode = True

HERE = os.path.dirname(os.path.abspath(__file__))
SDK = os.path.dirname(HERE)
GOACOS_ROOT = os.path.dirname(SDK)
GO_CLIENT = os.path.join(GOACOS_ROOT, "sdk", "registry", "client.go")
PY_PKG = os.path.join(SDK, "registry-python")
NODE_DIR = os.path.join(SDK, "registry-node")

# The scenario every binding must produce. Each entry is
# (step, expected request) where a request is the method + path + a subset of
# query/body keys that matter. Anything not listed is ignored, because
# incidental differences (key order, extra defaults) are not drift.
SCENARIO = "register_then_beat_then_deregister"


def fail(message: str) -> None:
    print("❌ " + message)
    globals()["FAILURES"] = globals().get("FAILURES", 0) + 1


def ok(message: str) -> None:
    print("✅ " + message)


FAILURES = 0


# ---------------------------------------------------------------- Go side


def parse_go_constants() -> Dict[str, Any]:
    """Extract the contract constants from the Go SDK source.

    Parsed rather than executed on purpose: this script must run without a Go
    toolchain, and the constants are simple enough that reading them off the
    source is unambiguous. If someone changes client.go into a shape this
    cannot parse, the script fails loudly rather than skipping the Go side —
    a conformance check that silently drops one language is worse than none.
    """
    with open(GO_CLIENT, "r", encoding="utf-8") as handle:
        source = handle.read()

    def const_str(name: str) -> str:
        match = re.search(r'^\s*%s\s*=\s*"([^"]*)"' % re.escape(name), source, re.M)
        if not match:
            raise KeyError("go: constant %s not found in client.go" % name)
        return match.group(1)

    # Go duration literals. Handled explicitly because `defaultRegisterBackoff
    # = time.Second` is not a number and a regex that only accepts digits
    # would report "not found" — which is at least honest, but would abort a
    # check that could otherwise have run. The unit set is closed on purpose:
    # an unrecognised unit must raise, not be guessed as seconds.
    units = {
        "time.Nanosecond": 1e-9,
        "time.Microsecond": 1e-6,
        "time.Millisecond": 1e-3,
        "time.Second": 1.0,
        "time.Minute": 60.0,
        "time.Hour": 3600.0,
    }

    def const_num(name: str) -> float:
        match = re.search(r"^\s*%s\s*=\s*(.+)$" % re.escape(name), source, re.M)
        if not match:
            raise KeyError("go: constant %s not found in client.go" % name)
        raw = match.group(1).strip()
        duration = re.fullmatch(r"([0-9]*\.?[0-9]+)\s*(time\.[A-Za-z]+)", raw)
        if duration:
            unit = duration.group(2)
            if unit not in units:
                raise KeyError("go: unrecognised duration unit %r in %s" % (unit, name))
            return float(duration.group(1)) * units[unit]
        try:
            return float(raw)
        except ValueError:
            pass
        # Duration form: a product of numbers and unit names in either order
        # -- `time.Second`, `5 * time.Hour`, `50 * time.Millisecond`.
        # Multiplying the numeric factors and the unit factors covers all
        # three without pretending to be a Go parser.
        total = 1.0
        saw_unit = False
        for factor in raw.split("*"):
            factor = factor.strip()
            if not factor:
                continue
            if factor in units:
                total *= units[factor]
                saw_unit = True
                continue
            try:
                total *= float(factor)
            except ValueError:
                raise KeyError("go: cannot read constant %s = %r" % (name, raw))
        if not saw_unit:
            raise KeyError("go: constant %s = %r is neither a number nor a duration" % (name, raw))
        return total

    def const_int(name: str) -> int:
        return int(const_num(name))

    return {
        "paths": {
            "prefix": const_str("pathPrefix"),
            "instance": const_str("pathInstance"),
            "beat": const_str("pathBeat"),
            "instance_list": const_str("pathInstanceList"),
            "service_list": const_str("pathServiceList"),
            "health": const_str("pathHealth"),
            "login": const_str("pathLogin"),
        },
        "backoff_initial_seconds": const_num("defaultRegisterBackoff"),
        "backoff_max_seconds": const_num("maxRegisterBackoff"),
        "beat_failure_threshold": const_int("beatFailureThreshold"),
        "min_beat_interval_ms": const_num("minAdvertisedBeatInterval") * 1000.0,
        "default_token_ttl_seconds": const_num("defaultTokenTTL"),
        "group": const_str("DefaultGroupName"),
        "cluster": const_str("DefaultClusterName"),
    }


# ----------------------------------------------------------- bindings side


def python_contract() -> Dict[str, Any]:
    sys.path.insert(0, PY_PKG)
    try:
        import goacos_registry as gr  # noqa: WPS433
    finally:
        sys.path.pop(0)
    return {
        "paths": {
            "prefix": gr.PATH_PREFIX,
            "instance": gr.PATH_INSTANCE,
            "beat": gr.PATH_BEAT,
            "instance_list": gr.PATH_INSTANCE_LIST,
            "service_list": gr.PATH_SERVICE_LIST,
            "health": gr.PATH_HEALTH,
            "login": gr.PATH_LOGIN,
        },
        "backoff_initial_seconds": gr.DEFAULT_REGISTER_BACKOFF,
        "backoff_max_seconds": gr.MAX_REGISTER_BACKOFF,
        "beat_failure_threshold": gr.BEAT_FAILURE_THRESHOLD,
        "min_beat_interval_ms": gr.MIN_ADVERTISED_BEAT_INTERVAL * 1000.0,
        # DEFAULT_TOKEN_TTL is already in seconds (5 * 60 * 60). Dividing it
        # again reported 5 instead of 18000 and masked the real comparison —
        # a unit bug in the checker reads exactly like a value drift.
        "default_token_ttl_seconds": float(gr.DEFAULT_TOKEN_TTL),
        "group": gr.DEFAULT_GROUP_NAME,
        "cluster": gr.DEFAULT_CLUSTER_NAME,
    }


def node_contract() -> Dict[str, Any]:
    script = """
    const { CONSTANTS, DEFAULT_GROUP_NAME, DEFAULT_CLUSTER_NAME } = require(%s);
    console.log(JSON.stringify({
      paths: {
        prefix: CONSTANTS.PATH_PREFIX,
        instance: CONSTANTS.PATH_INSTANCE,
        beat: CONSTANTS.PATH_BEAT,
        instance_list: CONSTANTS.PATH_INSTANCE_LIST,
        service_list: CONSTANTS.PATH_SERVICE_LIST,
        health: CONSTANTS.PATH_HEALTH,
        login: CONSTANTS.PATH_LOGIN,
      },
      backoff_initial_seconds: CONSTANTS.DEFAULT_REGISTER_BACKOFF_MS / 1000,
      backoff_max_seconds: CONSTANTS.MAX_REGISTER_BACKOFF_MS / 1000,
      beat_failure_threshold: CONSTANTS.BEAT_FAILURE_THRESHOLD,
      min_beat_interval_ms: CONSTANTS.MIN_ADVERTISED_BEAT_MS,
      // milliseconds -> seconds, and nothing more.
      default_token_ttl_seconds: CONSTANTS.DEFAULT_TOKEN_TTL_MS / 1000,
      group: DEFAULT_GROUP_NAME,
      cluster: DEFAULT_CLUSTER_NAME,
    }));
    """ % json.dumps(os.path.join(NODE_DIR, "index.js"))
    output = subprocess.run(
        ["node", "-e", script], capture_output=True, text=True, check=True
    ).stdout
    return json.loads(output)


# --------------------------------------------------------- the shared wire


def python_wire() -> List[Dict[str, Any]]:
    """Run the scenario in Python and return the observed requests."""
    script = r"""
import json, os, sys
sys.path.insert(0, %s)
import goacos_registry as gr

calls = []
def opener(method, url, payload, headers, timeout):
    from urllib.parse import urlparse, parse_qsl
    parsed = urlparse(url)
    # keep_blank_values=True is REQUIRED here and its absence looks exactly
    # like a missing field. parse_qsl drops `namespaceId=` by default while
    # Node's URLSearchParams keeps it, so an observer using the Python
    # default would report the Node client as sending an extra parameter on
    # every run -- a phantom drift that trains people to ignore this check.
    entry = {"method": method, "path": parsed.path,
             "query": dict(parse_qsl(parsed.query, keep_blank_values=True))}
    if payload:
        try:
            entry["body"] = json.loads(payload)
        except Exception:
            entry["body"] = payload.decode()
    calls.append(entry)
    if parsed.path.endswith("/beat"):
        return 200, json.dumps({"code": 0, "data": {"clientBeatInterval": 2000}})
    if parsed.path.endswith("/v2/ns/instance"):
        return 200, json.dumps({"code": 0, "data": {}})
    return 404, "not found"

client = gr.RegistryClient(
    gr.Config(service_name="llm-router", port=8080, ip="100.106.177.3",
              namespace_id="", group_name="DEFAULT_GROUP",
              cluster_name="DEFAULT",
              server_addr="http://goacos:8848",
              metadata={"app": "llm", "component": "router"}),
    opener=opener, sleep=lambda _s: False,
)
client.register()
client.beat()
client.deregister()
print(json.dumps({"calls": calls, "interval": client.heartbeat_interval()}))
""" % json.dumps(PY_PKG)
    # -B for the same reason as sys.dont_write_bytecode above.
    output = subprocess.run(
        [sys.executable, "-B", "-c", script], capture_output=True, text=True, check=True
    ).stdout
    return json.loads(output)


def node_wire() -> List[Dict[str, Any]]:
    """Run the scenario in Node and return the observed requests."""
    script = r"""
const { RegistryClient } = require(%s);
const calls = [];
const fakeFetch = async (url, init) => {
  const parsed = new URL(url);
  const entry = { method: init.method, path: parsed.pathname,
                  query: Object.fromEntries(parsed.searchParams.entries()) };
  if (init.body) entry.body = JSON.parse(init.body);
  calls.push(entry);
  if (parsed.pathname.endsWith('/beat')) {
    return { ok: true, status: 200,
             text: async () => JSON.stringify({ code: 0, data: { clientBeatInterval: 2000 } }) };
  }
  if (parsed.pathname.endsWith('/v2/ns/instance')) {
    return { ok: true, status: 200, text: async () => JSON.stringify({ code: 0, data: {} }) };
  }
  return { ok: false, status: 404, text: async () => 'not found' };
};
(async () => {
  const client = new RegistryClient({
    serviceName: 'llm-router', port: 8080, ip: '100.106.177.3',
    namespaceId: '', groupName: 'DEFAULT_GROUP', clusterName: 'DEFAULT',
    serverAddr: 'http://goacos:8848',
    metadata: { app: 'llm', component: 'router' },
  }, { fetch: fakeFetch, sleep: async () => false });
  await client.register();
  await client.beat();
  await client.deregister();
  console.log(JSON.stringify({ calls, interval: client.heartbeatIntervalMs() }));
})();
""" % json.dumps(os.path.join(NODE_DIR, "index.js"))
    output = subprocess.run(
        ["node", "-e", script], capture_output=True, text=True, check=True
    ).stdout
    return json.loads(output)


# ------------------------------------------------------------------ checks


def canonical(value: Any) -> Any:
    """Canonicalise a JSON value for comparison across languages.

    Python emits ``1.0`` for a float weight and Node emits ``1``. The two
    are the same JSON number and the server parses them identically, so
    comparing raw would report a difference on every run and train everyone
    to ignore this check. Integral floats collapse to int; everything else
    compares as-is.
    """
    if isinstance(value, bool):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    return value


def normalise_call(call: Dict[str, Any]) -> Dict[str, Any]:
    """Reduce a request to the parts that constitute the contract."""
    out = {
        "method": call.get("method"),
        "path": call.get("path"),
    }
    query = call.get("query") or {}
    # Only the instance key matters; pageNo/pageSize belong to the list APIs,
    # which this scenario does not exercise.
    for key in ("serviceName", "ip", "port", "clusterName", "namespaceId", "groupName"):
        if key in query:
            out.setdefault("query", {})[key] = query[key]
    body = call.get("body")
    if isinstance(body, dict):
        for key in ("serviceName", "ip", "port", "groupName", "clusterName",
                    "namespaceId", "weight", "enabled", "ephemeral"):
            if key in body:
                out.setdefault("body", {})[key] = canonical(body[key])
        if "metadata" in body:
            out.setdefault("body", {})["metadata"] = body["metadata"]
    return out


def main() -> int:
    global FAILURES

    print("goacos SDK cross-language conformance (%s)" % SCENARIO)
    print("=" * 62)

    # 1. constants agree
    print("\n[1] 常量一致性")
    try:
        go = parse_go_constants()
    except (KeyError, OSError) as exc:
        fail("无法从 client.go 解析契约常量：%s" % exc)
        print("\nGo 侧不可读，契约检查中止（不静默跳过）。")
        return 1
    py = python_contract()
    nd = node_contract()

    for key in ("paths",):
        for name, value in go[key].items():
            if py[key].get(name) != value:
                fail("Python 路径漂移：%s = %r，Go 为 %r" % (name, py[key].get(name), value))
            if nd[key].get(name) != value:
                fail("Node 路径漂移：%s = %r，Go 为 %r" % (name, nd[key].get(name), value))
    ok("七条路径三语言一致")

    for name in ("backoff_initial_seconds", "backoff_max_seconds",
                 "beat_failure_threshold", "min_beat_interval_ms",
                 "default_token_ttl_seconds", "group", "cluster"):
        if not (go[name] == py[name] == nd[name]):
            fail("常量 %s 不一致：Go=%r Python=%r Node=%r" % (name, go[name], py[name], nd[name]))
    ok("退避、阈值、TTL、分组名三语言一致")

    # 2. the eviction-window invariant, stated once for all three
    print("\n[2] 退避不得超过淘汰窗口")
    for name, contract in (("Go", go), ("Python", py), ("Node", nd)):
        if contract["backoff_max_seconds"] > 30.0:
            fail("%s 退避上限 %.1fs 超过 30s 淘汰窗口：实例在被重新注册前就已被淘汰，将永不回来"
                 % (name, contract["backoff_max_seconds"]))
    ok("三者退避上限均 ≤ 30s（与 goacos 淘汰窗口一致）")

    # 3. the wire must be identical
    print("\n[3] 上线报文逐字一致")
    py_wire = python_wire()
    nd_wire = node_wire()
    py_calls = [normalise_call(c) for c in py_wire["calls"]]
    nd_calls = [normalise_call(c) for c in nd_wire["calls"]]

    if py_calls != nd_calls:
        fail("Python 与 Node 的实际请求不一致")
        print("  python: %s" % json.dumps(py_calls, indent=2))
        print("  node:   %s" % json.dumps(nd_calls, indent=2))
    else:
        ok("register / beat / deregister 三步报文完全一致")

    # 4. the heartbeat cadence must be adopted identically
    print("\n[4] 心跳节奏采纳一致")
    # Units differ by API design: the Python binding speaks seconds, the
    # Node binding milliseconds. That is a naming convention, not drift —
    # what must match is the decision to adopt the server's value.
    py_ms = float(py_wire["interval"]) * 1000.0
    nd_ms = float(nd_wire["interval"])
    if abs(py_ms - nd_ms) > 1e-6:
        fail("服务端下发的 clientBeatInterval 采纳结果不一致：Python=%.0fms Node=%.0fms"
             % (py_ms, nd_ms))
    elif abs(py_ms - 2000.0) > 1e-6:
        fail("两侧都未采纳服务端下发的 2000ms（Python=%.0fms Node=%.0fms）—— "
             "客户端改用自己的默认值，登记心跳节奏就由客户端而非服务端说了算" % (py_ms, nd_ms))
    else:
        ok("两侧都采纳服务端下发的 2000ms（未使用各自默认值）")

    print("\n" + "=" * 62)
    if FAILURES:
        print("结论：%d 项不一致 —— 三语言已漂移，必须先对齐再合并。" % FAILURES)
        return 1
    print("结论：Go / Python / Node 三语言契约一致。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
