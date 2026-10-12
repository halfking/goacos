"""Tests for the goacos Python binding.

Run with:  python3 -m pytest goacos/sdk/registry-python -q
or simply: python3 goacos/sdk/registry-python/tests/test_registry.py

No third-party test runner is required: the file has a ``__main__``
fallback. A binding that needs pytest installed is a binding some container
will not be able to verify.
"""

import json
import os
import sys
import threading
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from goacos_registry import (  # noqa: E402
    BEAT_FAILURE_THRESHOLD,
    DEFAULT_CLUSTER_NAME,
    DEFAULT_GROUP_NAME,
    DEFAULT_NAMESPACE_ID,
    MAX_REGISTER_BACKOFF,
    PATH_BEAT,
    PATH_INSTANCE,
    AuthError,
    Config,
    DisabledError,
    RegistryClient,
    RegistryError,
    StatusError,
)


class FakeRegistry:
    """A scripted goacos.

    Records every call and replays canned answers. Scripted rather than a
    real HTTP server because the point of these tests is the client's
    decisions (retry, refresh, re-register), not the transport.
    """

    def __init__(self):
        self.calls = []
        self.register_status = 200
        self.register_bodies = []
        self.beat_status = 200
        self.beat_bodies = []
        self.login_status = 200
        self.login_body = {"accessToken": "tok-1", "tokenTtl": 3600}
        self.lock = threading.Lock()

    def __call__(self, method, url, payload, headers, timeout):
        with self.lock:
            self.calls.append(
                {
                    "method": method,
                    "url": url,
                    "path": url.split("?")[0],
                    "payload": payload,
                    "headers": dict(headers or {}),
                    "timeout": timeout,
                }
            )
        # Match on the PATH, not the full URL. An earlier version of this
        # fake matched `url.endswith(PATH_BEAT)` and therefore never matched
        # the beat call at all — beat URLs carry a query string — so it
        # silently answered beats with the register answer. The heartbeat
        # test then "passed" against a registry that had never been asked a
        # heartbeat question.
        path = url.split("?")[0]
        if path.endswith("/auth/users/login"):
            return self.login_status, json.dumps(self.login_body)
        if path.endswith(PATH_BEAT):
            body = self.beat_bodies.pop(0) if self.beat_bodies else {"clientBeatInterval": 5000}
            return self.beat_status, json.dumps({"code": 0, "data": body})
        if path.endswith(PATH_INSTANCE):
            body = self.register_bodies.pop(0) if self.register_bodies else {}
            return self.register_status, json.dumps({"code": 0, "data": body})
        return 404, "not found"

    def paths(self):
        return [call["url"] for call in self.calls]

    def paths_only(self):
        return [call["path"] for call in self.calls]


def make_client(fake, sleep=None, **overrides):
    """Build a client over a fake registry.

    ``sleep`` is a RegistryClient injection point, not a Config field —
    passing it through overrides would raise TypeError, which is how the
    lifecycle tests first failed.
    """
    kwargs = dict(
        service_name="llm-router",
        port=8080,
        ip="100.106.177.3",
        server_addr="http://goacos:8848",
    )
    kwargs.update(overrides)
    cfg = Config(**kwargs)
    if sleep is None:
        sleep = lambda _s: True
    return RegistryClient(cfg, opener=fake, sleep=sleep)


class TestConfigValidation(unittest.TestCase):
    def test_requires_service_name(self):
        with self.assertRaises(ValueError):
            Config(service_name="", port=8080)

    def test_rejects_impossible_port(self):
        # A port of 0 or a string is the shape of a mis-wired env var, and
        # the server would answer "serviceName/ip/port required" — an error
        # that points at the registry instead of at the service's own config.
        for bad in (0, -1, 70000):
            with self.assertRaises(ValueError):
                Config(service_name="svc", port=bad)

    def test_defaults_group_and_weight(self):
        cfg = Config(service_name="svc", port=80)
        self.assertEqual(cfg.group_name, DEFAULT_GROUP_NAME)
        self.assertEqual(cfg.weight, 1.0)
        cfg2 = Config(service_name="svc", port=80, weight=-3)
        self.assertEqual(cfg2.weight, 1.0)

    def test_none_means_default_not_the_string_none(self):
        """`or None` 是调用方的常规写法，但它会把 dataclass 默认值覆盖掉。

        实测后果（不是假设）：sidecar 传 None 之后，同一个实例在库里
        留下了两行——group 一次字面量 "null"、一次 "DEFAULT_GROUP"，
        cluster 一次 "null"、一次 "None"。注销只删得掉其中一行。
        而且这两行都不在 DEFAULT_GROUP 下，网关按默认组取实例时看不到它。
        """
        cfg = Config(service_name="svc", port=80,
                     namespace_id=None, group_name=None, cluster_name=None)
        self.assertEqual(cfg.group_name, DEFAULT_GROUP_NAME)
        self.assertEqual(cfg.cluster_name, DEFAULT_CLUSTER_NAME)
        self.assertEqual(cfg.namespace_id, DEFAULT_NAMESPACE_ID)

    def test_none_never_reaches_the_wire(self):
        """归一必须在**发出去之前**生效，而不是只在字段上看一眼。

        用 FakeRegistry 已记录的 payload 判定——那是真正上线的 body，
        比断言 Config 字段更接近问题本身：出问题的就是上线的那份数据。
        """
        fake = FakeRegistry()
        cfg = Config(service_name="llm-router", port=8080, ip="100.106.177.3",
                     server_addr="http://goacos:8848",
                     group_name=None, cluster_name=None, namespace_id=None)
        client = RegistryClient(cfg, opener=fake, sleep=lambda _s: True)
        client.register()

        raw = [c["payload"] for c in fake.calls
               if c["path"].endswith(PATH_INSTANCE) and c["method"] == "POST"]
        self.assertTrue(raw, "没有记录到注册请求，用例本身失效")
        for blob in raw:
            payload = json.loads(blob.decode() if isinstance(blob, bytes) else blob)
            for key in ("groupName", "clusterName", "namespaceId"):
                value = payload.get(key)
                self.assertNotEqual(value, None, "%s 以 None 上了线" % key)
                self.assertNotIn("None", str(value or ""),
                                 "%s 变成了字符串 'None'" % key)
            self.assertEqual(payload.get("groupName"), DEFAULT_GROUP_NAME)
            self.assertEqual(payload.get("clusterName"), DEFAULT_CLUSTER_NAME)

    def test_rejects_wrong_type_for_names(self):
        """None 可以（= 用默认值），但 8080 这种类型不行——
        静默接受会让错误更晚、更难查地暴露出来。"""
        for attr in ("group_name", "cluster_name", "namespace_id"):
            with self.assertRaises(TypeError):
                Config(service_name="svc", port=80, **{attr: 8080})


class TestRegistrationContract(unittest.TestCase):
    def test_wire_paths_match_the_go_sdk(self):
        fake = FakeRegistry()
        client = make_client(fake)
        client.register()
        client.beat()
        self.assertTrue(
            fake.paths()[0].startswith("http://goacos:8848/nacos/v2/ns/instance"),
            "register must hit /nacos/v2/ns/instance, got %s" % fake.paths()[0],
        )
        self.assertIn("/nacos/v2/ns/instance/beat", fake.paths()[1])

    def test_register_body_carries_the_instance_metadata_contract(self):
        # The platform's routing depends on these metadata keys; a binding
        # that spells them differently registers an instance the gateway
        # cannot place.
        fake = FakeRegistry()
        client = make_client(
            fake,
            metadata={"app": "llm", "component": "router", "role": "primary", "tier": "edge"},
        )
        client.register()
        body = json.loads(fake.calls[0]["payload"].decode())
        for key in ("serviceName", "ip", "port", "namespaceId", "groupName",
                    "clusterName", "weight", "enabled", "ephemeral", "metadata"):
            self.assertIn(key, body, "register body missing %s" % key)
        self.assertEqual(body["metadata"]["app"], "llm")
        self.assertEqual(body["metadata"]["component"], "router")
        self.assertEqual(body["ephemeral"], True)
        self.assertEqual(body["port"], 8080)

    def test_refuses_to_guess_the_advertise_ip(self):
        # The trap this prevents: sniffing an interface on a host with
        # netbird + LAN + docker bridge picks an address no caller can
        # reach, producing "registered but traffic never arrives".
        client = make_client(FakeRegistry(), ip="")
        with self.assertRaises(RegistryError) as ctx:
            client.register()
        self.assertIn("advertise IP", str(ctx.exception))
        self.assertIn("100.x", str(ctx.exception))

    def test_disabled_client_refuses_every_network_call(self):
        client = make_client(FakeRegistry(), enabled=False)
        client.start()  # must not raise, must not spawn a thread
        with self.assertRaises(DisabledError):
            client.register()
        with self.assertRaises(DisabledError):
            client.beat()
        with self.assertRaises(DisabledError):
            client.ping()


class TestHeartbeatCadence(unittest.TestCase):
    def test_adopts_the_servers_advertised_interval(self):
        # The server owns the eviction policy (unhealthy 15s, removed 30s).
        # A client with its own hardcoded timer is a client that will
        # eventually be evicted while believing it is healthy.
        fake = FakeRegistry()
        fake.beat_bodies = [{"clientBeatInterval": 2000}]
        client = make_client(fake, heartbeat_interval=5.0)
        self.assertEqual(client.heartbeat_interval(), 5.0)
        client.beat()
        self.assertEqual(client.heartbeat_interval(), 2.0)

    def test_clamps_an_absurd_advertised_interval(self):
        # 0ms advertised would otherwise become a busy loop that hammers
        # the registry from every registered instance at once.
        fake = FakeRegistry()
        fake.beat_bodies = [{"clientBeatInterval": 0}]
        client = make_client(fake)
        client.beat()
        self.assertGreaterEqual(client.heartbeat_interval(), 0.05)

    def test_ignores_a_missing_interval(self):
        fake = FakeRegistry()
        fake.beat_bodies = [{}]
        client = make_client(fake, heartbeat_interval=7.0)
        client.beat()
        self.assertEqual(client.heartbeat_interval(), 7.0)

    def test_beat_carries_the_full_instance_key(self):
        # The server keys an instance by (serviceName, ip, port,
        # clusterName). A beat missing `ip` addresses a *different*
        # instance, so it returns 200 while the real instance ages out and
        # is evicted — a silent loss of registration that looks like the
        # registry is healthy.
        #
        # A mutation run dropped `ip` from this query and no test noticed,
        # which is why the key is now asserted verbatim.
        import urllib.parse

        fake = FakeRegistry()
        client = make_client(fake)
        client.beat()
        url = fake.calls[-1]["url"]
        query = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(url).query))
        self.assertEqual(
            query,
            {
                "serviceName": "llm-router",
                "ip": "100.106.177.3",
                "port": "8080",
                "clusterName": "DEFAULT",
            },
        )

    def test_deregister_carries_the_namespace_and_group(self):
        # Deregister addresses a wider key than beat (it includes
        # namespaceId and groupName). Dropping either leaves the instance
        # registered in a namespace nothing will ever clean up.
        import urllib.parse

        fake = FakeRegistry()
        client = make_client(fake, namespace_id="prod", group_name="EDGE")
        client.deregister()
        url = fake.calls[-1]["url"]
        query = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(url).query))
        self.assertEqual(query.get("namespaceId"), "prod")
        self.assertEqual(query.get("groupName"), "EDGE")
        self.assertEqual(query.get("ip"), "100.106.177.3")
        self.assertEqual(query.get("serviceName"), "llm-router")


class TestTokenHandling(unittest.TestCase):
    def test_login_then_authorizes(self):
        fake = FakeRegistry()
        client = make_client(fake, username="u", password="p")
        client.register()
        auth_headers = [c["headers"].get("Authorization") for c in fake.calls]
        self.assertIn("Bearer tok-1", auth_headers)

    def test_static_token_skips_login(self):
        fake = FakeRegistry()
        client = make_client(fake, token="preset-token")
        client.register()
        self.assertFalse(
            any("/auth/users/login" in url for url in fake.paths()),
            "a static token must not trigger a login round trip",
        )
        self.assertEqual(fake.calls[0]["headers"].get("Authorization"), "Bearer preset-token")

    def test_refreshes_once_on_401_then_gives_up(self):
        # One transparent retry is right (tokens expire mid-flight). A
        # second failure must surface, not loop — an infinite refresh loop
        # against a registry with the wrong password is a self-inflicted
        # denial of service.
        fake = FakeRegistry()
        fake.login_status = 401
        client = make_client(fake, username="u", password="wrong")
        with self.assertRaises(AuthError):
            client.register()
        # The point is that it gives up: an unbounded refresh loop against a
        # registry with the wrong password is a self-inflicted DoS.
        self.assertLessEqual(
            len([p for p in fake.paths_only() if p.endswith("/auth/users/login")]), 4
        )

    def test_missing_token_in_login_response_is_an_error(self):
        # Caching "no token" would make every later call silently
        # unauthenticated and produce a 401 the caller cannot explain.
        fake = FakeRegistry()
        fake.login_body = {"tokenTtl": 3600}
        client = make_client(fake, username="u", password="p")
        with self.assertRaises(AuthError):
            client.register()

    def test_login_without_ttl_assumes_a_conservative_lifetime(self):
        fake = FakeRegistry()
        fake.login_body = {"accessToken": "tok-2"}
        client = make_client(fake, username="u", password="p")
        client.register()
        self.assertEqual(client._token, "tok-2")
        self.assertGreater(client._token_expires_at, time.time())

    def test_expired_token_is_refreshed_before_the_next_call(self):
        # The gap a mutation run exposed: every other token test passed
        # even with _token_expired() hard-wired to False, because nothing
        # drove the clock across the TTL boundary. This one does.
        #
        # Without it, a client whose token had expired would keep sending
        # it until the server answered 401 — and the whole point of caching
        # the TTL is to refresh *before* that rejection, not after.
        fake = FakeRegistry()
        client = make_client(fake, username="u", password="p")
        client.register()
        self.assertEqual(client._token, "tok-1")
        first_logins = len([p for p in fake.paths_only() if p.endswith("/auth/users/login")])

        # Pretend the cached token has aged out.
        client._token_expires_at = time.time() - 1
        client.register()

        logins = [p for p in fake.paths_only() if p.endswith("/auth/users/login")]
        self.assertGreater(
            len(logins), first_logins, "an expired token must trigger a fresh login, not reuse"
        )

    def test_refreshes_five_minutes_early(self):
        # The window matters: a token that expires between two 30-second
        # heartbeats would otherwise 401 mid-flight. Refresh early instead.
        client = make_client(FakeRegistry(), username="u", password="p")
        ttl = 600.0
        with client._lock:
            client._token = "tok-3"
            client._token_expires_at = time.time() + ttl
        self.assertFalse(client._token_expired(), "a token with 10 minutes left is still valid")
        with client._lock:
            client._token_expires_at = time.time() + 120.0
        self.assertTrue(
            client._token_expired(), "a token with 2 minutes left must count as expiring"
        )


class TestEnvelopeDecoding(unittest.TestCase):
    def test_non_zero_code_is_an_error_even_on_http_200(self):
        # goacos answers some rejected registrations with 200 and a non-zero
        # code in the body. Treating that as success is how an instance
        # "registers" and never appears in the registry.
        calls = []

        def opener(method, url, payload, headers, timeout):
            calls.append(url)
            return 200, json.dumps({"code": 400, "message": "serviceName/ip/port required"})

        client = make_client(opener)
        with self.assertRaises(RegistryError) as ctx:
            client.register()
        self.assertIn("400", str(ctx.exception))

    def test_status_error_keeps_the_status_for_branching(self):
        attempts = []

        def opener(method, url, payload, headers, timeout):
            attempts.append(url)
            return 500, "internal error"

        client = make_client(opener)
        with self.assertRaises(StatusError) as ctx:
            client.register()
        self.assertEqual(ctx.exception.status, 500)
        # 500 is not an auth rejection, so exactly one attempt was made —
        # no token refresh, no retry, no backoff storm against a broken
        # registry.
        self.assertEqual(len(attempts), 1)


class TestLifecycleDecisions(unittest.TestCase):
    def test_registers_before_the_first_sleep(self):
        # If the loop slept first, ACC/registry would have no view of this
        # instance for a full interval — which is exactly the window an
        # operator checks when asking "is this service there at all".
        fake = FakeRegistry()
        slept = []

        def fake_sleep(seconds):
            slept.append(seconds)
            return len(slept) < 2

        client = make_client(fake, sleep=fake_sleep)
        client._run()
        registers = [p for p in fake.paths_only() if p.endswith("/v2/ns/instance")]
        beats = [p for p in fake.paths_only() if p.endswith(PATH_BEAT)]
        self.assertTrue(registers, "expected a registration")
        self.assertEqual(
            fake.paths_only()[0].split("/nacos")[-1],
            "/v2/ns/instance",
            "registration must happen before any sleep, so the registry has a view of this instance immediately",
        )
        self.assertGreaterEqual(len(beats), 1)

    def test_three_failed_beats_trigger_re_registration(self):
        # The server may have evicted the instance outright; beating on is
        # pointless until we announce ourselves again.
        fake = FakeRegistry()
        state = {"n": 0}

        def flaky_sleep(_seconds):
            state["n"] += 1
            return state["n"] <= BEAT_FAILURE_THRESHOLD + 2

        client = make_client(fake, sleep=flaky_sleep)
        # Patch the PUBLIC method the loop actually calls. An earlier
        # version patched `_beat`, a name `_run` never references, so every
        # beat silently succeeded and the test failed for the right reason
        # only by accident.
        client.beat = _AlwaysFailBeat()
        client._run()
        registers = [p for p in fake.paths_only() if p.endswith("/v2/ns/instance")]
        self.assertGreaterEqual(
            len(registers), 2, "expected a re-registration after repeated beat failures"
        )

    def test_backoff_never_exceeds_the_eviction_window(self):
        # Backoff capped past 30s means the instance is gone before the next
        # attempt, so it would never come back.
        self.assertEqual(MAX_REGISTER_BACKOFF, 30.0)

    def test_stop_deregisters_and_is_idempotent(self):
        fake = FakeRegistry()
        client = make_client(fake)
        client.start()
        time.sleep(0.2)
        client.stop()
        client.stop()  # must not raise
        self.assertTrue(
            any(c["method"] == "DELETE" for c in fake.calls),
            "stop must deregister so the 15s phantom-routing window is closed",
        )


class _AlwaysFailBeat:
    """Callable stand-in assigned to a client instance.

    Both binding rules bite here, and the test only passes once both are
    right: a plain function assigned to an instance attribute is NOT
    rebound (``self.beat = _always_fail`` raises ``TypeError: missing 1
    required positional argument: 'self'``), and a callable OBJECT is also
    not rebound (``__call__`` therefore takes no arguments). The reported
    error names whichever one got it wrong, which is why this comment
    exists.
    """

    def __call__(self):
        raise StatusError("POST", PATH_BEAT, 500, "registry down")


if __name__ == "__main__":
    unittest.main(verbosity=2)
