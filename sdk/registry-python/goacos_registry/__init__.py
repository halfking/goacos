# SPDX-License-Identifier: Apache-2.0
"""goacos_registry — Python client for the goacos service registry.

This is the Python binding of the Go SDK (``goacos/sdk/registry``). Both
implement the same wire contract; see ``CONTRACT.md`` next to this file for
the invariants they share and the test that holds them together.

Why a binding exists at all
---------------------------
The platform decided that every service registers itself ("each service
carries its own SDK"). RedClaw alone ships seven Python services. Without a
Python binding that decision is unimplementable for them, and the
alternative — hand-written ``requests.post`` in each service — is how seven
services end up with seven subtly different retry, heartbeat and shutdown
behaviours, none of which match the server's eviction policy.

Zero dependencies, on purpose
-----------------------------
Standard library only (``urllib``, ``threading``, ``json``). This client has
to be droppable into a container whose dependency set is already pinned and
argued about; a client that pulls in ``requests`` is a client that some
service will fork to avoid a dependency bump. ``urllib`` is slower and that
does not matter: this process makes one registration call and one heartbeat
every thirty seconds.

The invariants that matter
--------------------------
1. **Registration never breaks the host application.** A registry that is
   down, misconfigured or not yet deployed must not stop the service from
   serving. Everything here logs and retries; nothing raises into the
   caller's startup path unless it explicitly asks.
2. **The advertised address is never guessed.** If ``ip`` is not
   configured, ``register()`` raises rather than picking an interface. A
   guessed address that happens to be wrong puts the service in the registry
   at an address nothing can reach, and the failure looks like "the service
   is registered but traffic doesn't arrive".
3. **The server owns the heartbeat cadence.** ``clientBeatInterval`` from
   the beat response is adopted, because the server also owns the eviction
   policy (unhealthy at 15s, removed at 30s). A client with its own timer
   either beats too often or, worse, too rarely for a tighter window.
4. **Retry never slows past eviction.** Backoff caps at 30 seconds, the same
   number as the eviction window. A client whose backoff grows past the
   point where the server has already deleted it will never come back.
5. **Three failed beats mean re-register.** The server may have evicted the
   instance; beating on is pointless until we announce ourselves again.
"""

from __future__ import annotations

import json
import logging
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

__all__ = [
    "Config",
    "RegistryClient",
    "RegistryError",
    "StatusError",
    "AuthError",
    "DisabledError",
    "DEFAULT_GROUP_NAME",
    "DEFAULT_CLUSTER_NAME",
    "DEFAULT_NAMESPACE_ID",
]

DEFAULT_GROUP_NAME = "DEFAULT_GROUP"
DEFAULT_CLUSTER_NAME = "DEFAULT"
DEFAULT_NAMESPACE_ID = ""

# Wire paths. Every path is relative to the "/nacos" context path; see
# CONTRACT.md for why v2 endpoints are the contract and not an accident.
PATH_PREFIX = "/nacos"
PATH_INSTANCE = "/v2/ns/instance"
PATH_BEAT = "/v2/ns/instance/beat"
PATH_INSTANCE_LIST = "/v2/ns/instance/list"
PATH_SERVICE_LIST = "/v2/ns/service/list"
PATH_HEALTH = "/actuator/health"
PATH_LOGIN = "/v1/auth/users/login"

# Lifecycle tuning. These mirror the Go SDK exactly; changing one side
# without the other is caught by the conformance test.
DEFAULT_REGISTER_BACKOFF = 1.0
MAX_REGISTER_BACKOFF = 30.0
BEAT_FAILURE_THRESHOLD = 3
MIN_ADVERTISED_BEAT_INTERVAL = 0.05
DEFAULT_TOKEN_TTL = 5 * 60 * 60
DEFAULT_REQUEST_TIMEOUT = 5.0
DEREGISTER_TIMEOUT = 2.0

_LOG = logging.getLogger("goacos.registry")


class RegistryError(Exception):
    """Any failure talking to the registry."""


class DisabledError(RegistryError):
    """The client is disabled; every network method raises this."""


class StatusError(RegistryError):
    """A non-2xx answer, carrying the status so callers branch on the code.

    Carrying the status as a field — rather than only in the message — is
    what lets the client distinguish "the registry rejected my credentials"
    (refresh and retry) from "the registry is broken" (back off). Matching
    on the prose cannot tell those apart, and this codebase has a history of
    a 500 whose body happened to mention the word "unauthorized".
    """

    def __init__(self, method: str, path: str, status: int, body: str = "") -> None:
        self.method = method
        self.path = path
        self.status = status
        self.body = body
        detail = body.strip()[:400]
        super().__init__(
            "registry %s %s: status %d%s" % (method, path, status, ": " + detail if detail else "")
        )


class AuthError(RegistryError):
    """The registry rejected the credentials even after a refresh."""


@dataclass
class Config:
    """Client configuration.

    Field names mirror the Go ``registry.Config`` so a config translated
    between the two SDKs reads the same.
    """

    service_name: str
    port: int

    enabled: bool = True
    server_addr: str = "http://127.0.0.1:8848"
    namespace_id: str = DEFAULT_NAMESPACE_ID
    group_name: str = DEFAULT_GROUP_NAME
    cluster_name: str = DEFAULT_CLUSTER_NAME

    #: The address other services dial. Deliberately NOT auto-detected — see
    #: the module docstring. ``advertise_ip()`` is the opt-in fallback.
    ip: str = ""

    weight: float = 1.0
    metadata: Dict[str, str] = field(default_factory=dict)

    username: str = ""
    password: str = ""
    #: A pre-obtained accessToken. When set, the login round trip is skipped.
    token: str = ""

    heartbeat_interval: float = 5.0
    request_timeout: float = DEFAULT_REQUEST_TIMEOUT

    def __post_init__(self) -> None:
        if not self.service_name:
            raise ValueError("config: service_name is required")
        if not isinstance(self.port, int) or self.port <= 0 or self.port > 65535:
            raise ValueError("config: port must be a TCP port, got %r" % (self.port,))
        if self.weight <= 0:
            self.weight = 1.0
        if self.heartbeat_interval <= 0:
            self.heartbeat_interval = 5.0
        self._normalize_optional_names()

    def _normalize_optional_names(self) -> None:
        """把 None 与「空白的名字」归一成「用默认值」，而不是把它们发到线上。

        这是实测踩出来的坑，不是防御性编程。调用方常见写法是
        ``cluster_name=env("GOACOS_CLUSTER") or None``——本意是
        「没配就别管，交给服务端取默认」。但 ``None`` 在这里是**赋值**，
        它把 dataclass 自己的默认值覆盖掉了，于是发出去的是字面量
        ``clusterName=None``（或 JSON 的 ``null``）。

        后果不是「多了个怪名字」这么简单：
          * 实例落在 cluster ``"None"`` 而不是 ``"DEFAULT"``；
          * goacos 对空 cluster 归一成 ``DEFAULT``，对 ``"None"`` 不归一，
            于是注册与查询可能落在不同分桶；
          * 网关同步脚本按 ``DEFAULT_GROUP`` 取实例时**看不到它**——
            服务在注册中心里存在，数据面却当它不存在。
        同一实例因此还可能写出两行（group 一次 ``null``、一次
        ``DEFAULT_GROUP``），注销只删得掉其中一行，另一行成为永久幽灵。

        归一放在 SDK 而不是只修调用方，是因为**每一个** Python 调用方
        都会踩这个 ``or None``，而不是只有第一个写它的人。

        ``strip()`` 那一支是后来补的：``None`` 之外的「看起来没设置」还有两种，
        而且都比想象中常见——``os.environ.get(k, "")`` 给出的空串，以及 compose /
        .env 里带游离空格的 ``"   "``。它们不是空的名字，是**另一个不可达的名字**：
        实例落进没人查询的分桶，每一层却都报成功。Go 用
        ``strings.TrimSpace(v) == ""``、Node 用 ``trim() || 默认值``，
        只有这里不归一，于是同一份配置下三语言的服务落进不同分桶。
        判据在 ``sdk/conformance/run.py`` 第 [5] 节，变异验证过。

        namespace 刻意不 strip：它不参与「数据面按默认名取实例」这条路径，
        Go 侧是原样透传的（Node 会 trim，见 CONTRACT.md 记下的已知差异）。
        这里跟着 Go，而不是让三边各行其是。
        """
        for attr, default in (("namespace_id", DEFAULT_NAMESPACE_ID),
                              ("group_name", DEFAULT_GROUP_NAME),
                              ("cluster_name", DEFAULT_CLUSTER_NAME)):
            value = getattr(self, attr)
            if value is None:
                setattr(self, attr, default)
            elif not isinstance(value, str):
                raise TypeError(
                    "config: %s must be a str or None, got %r"
                    % (attr, type(value).__name__))
            elif attr != "namespace_id" and not value.strip():
                setattr(self, attr, default)


class RegistryClient:
    """A goacos client.

    Typical use::

        client = RegistryClient(Config(service_name="llm-router", port=8080,
                                       ip="100.106.177.3",
                                       metadata={"app": "llm", "component": "router"}))
        client.start()          # background thread; returns immediately
        try:
            serve_forever()
        finally:
            client.stop()

    ``start()`` never blocks on the registry. If it is unreachable the
    thread retries with backoff while the application serves traffic
    un-regdistered — which is exactly the state we want during an outage:
    the service is up, callers use their last-known address, and the
    instance reappears in the registry when it recovers.
    """

    def __init__(
        self,
        config: Config,
        logger: Optional[logging.Logger] = None,
        opener: Optional[Callable[..., Any]] = None,
        sleep: Optional[Callable[[float], bool]] = None,
    ) -> None:
        self.cfg = config
        self.log = logger or _LOG
        # Injection points. The defaults are the real ones; tests replace
        # them so the lifecycle can be driven deterministically without a
        # server, a clock or a sleep.
        self._opener = opener or _urlopen
        self._sleep = sleep if sleep is not None else _interruptible_sleep

        self._lock = threading.RLock()
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._heartbeat_interval = config.heartbeat_interval
        self._token = config.token or ""
        self._token_expires_at = 0.0
        self._registered = False

    # ------------------------------------------------------------------
    # introspection
    # ------------------------------------------------------------------

    @property
    def enabled(self) -> bool:
        return self.cfg.enabled

    @property
    def registered(self) -> bool:
        """Whether a registration has succeeded since the last failure."""
        with self._lock:
            return self._registered

    def heartbeat_interval(self) -> float:
        with self._lock:
            return self._heartbeat_interval

    def base_url(self) -> str:
        return self.cfg.server_addr.rstrip("/") + PATH_PREFIX

    # ------------------------------------------------------------------
    # lifecycle
    # ------------------------------------------------------------------

    def start(self) -> None:
        """Begin registering in the background. Returns immediately.

        Calling start twice is a no-op rather than an error: a service that
        starts its HTTP server and its background jobs from two places
        should not have to coordinate.
        """
        if not self.cfg.enabled:
            self.log.info("registry disabled; %s will not register", self.cfg.service_name)
            return
        with self._lock:
            if self._thread is not None and self._thread.is_alive():
                return
            self._stop.clear()
            self._thread = threading.Thread(
                target=self._run,
                name="goacos-registry-%s" % self.cfg.service_name,
                daemon=True,
            )
            self._thread.start()

    def stop(self, timeout: float = DEREGISTER_TIMEOUT + 1.0) -> None:
        """Deregister and stop the background thread. Safe to call twice.

        Deregistration is best-effort: the server evicts ephemeral instances
        that stop beating anyway, so a registry that is down must not turn
        shutdown into a hang. It only turns a 15-second window of phantom
        routing into an immediate one.
        """
        self._stop.set()
        thread = None
        with self._lock:
            thread = self._thread
            self._thread = None
        if thread is not None and thread.is_alive():
            thread.join(timeout=timeout)
        if self.cfg.enabled:
            try:
                self.deregister()
            except RegistryError as exc:
                self.log.warning("registry deregister failed (best effort): %s", exc)
        with self._lock:
            self._registered = False

    def register(self) -> None:
        """Register this instance. Idempotent on the server.

        Raises ``RegistryError`` on failure — this is the explicit,
        synchronous entry point. The background loop catches; an operator
        calling this by hand wants to see the error.
        """
        self._require_enabled()
        address = self.advertise_ip()
        body = {
            "serviceName": self.cfg.service_name,
            "ip": address,
            "port": self.cfg.port,
            "namespaceId": self.cfg.namespace_id,
            "groupName": self.cfg.group_name,
            "clusterName": self.cfg.cluster_name,
            "weight": self.cfg.weight,
            "enabled": True,
            "ephemeral": True,
            "metadata": self.cfg.metadata or {},
        }
        self._do("POST", PATH_INSTANCE, None, body)
        with self._lock:
            self._registered = True
        self.log.info(
            "registry instance registered",
            extra={"service": self.cfg.service_name, "ip": address, "port": self.cfg.port},
        )

    def beat(self) -> None:
        """Send one heartbeat and adopt the server's advertised cadence.

        Raises ``RegistryError`` on failure.
        """
        self._require_enabled()
        address = self.advertise_ip()
        query = {
            "serviceName": self.cfg.service_name,
            "ip": address,
            "port": str(self.cfg.port),
            "clusterName": self.cfg.cluster_name,
        }
        data = self._do("POST", PATH_BEAT, query, None)
        payload = data if isinstance(data, dict) else {}
        client_beat_ms = 0
        if isinstance(payload, dict):
            raw = payload.get("clientBeatInterval", 0)
            try:
                client_beat_ms = int(raw)
            except (TypeError, ValueError):
                client_beat_ms = 0
        if client_beat_ms > 0:
            interval = max(client_beat_ms / 1000.0, MIN_ADVERTISED_BEAT_INTERVAL)
            # No upper clamp on purpose: the server owns its own eviction
            # policy, and beating faster than it asks is only noise.
            with self._lock:
                previous = self._heartbeat_interval
                self._heartbeat_interval = interval
            if previous != interval:
                self.log.info(
                    "registry heartbeat interval adopted from server",
                    extra={"interval": interval, "previous": previous},
                )

    def deregister(self) -> None:
        """Remove this instance from the registry. Best-effort by design.

        Raises ``RegistryError`` so a caller can log it; ``stop()`` does.
        """
        self._require_enabled()
        address = self.advertise_ip()
        query = {
            "serviceName": self.cfg.service_name,
            "ip": address,
            "port": str(self.cfg.port),
            "clusterName": self.cfg.cluster_name,
            "namespaceId": self.cfg.namespace_id,
            "groupName": self.cfg.group_name,
        }
        self._do("DELETE", PATH_INSTANCE, query, None)
        with self._lock:
            self._registered = False

    def list_instances(self, service_name: str, healthy_only: bool = True) -> List[Dict[str, Any]]:
        """List a service's instances. Used for debug endpoints and probes."""
        self._require_enabled()
        query = {
            "serviceName": service_name,
            "namespaceId": self.cfg.namespace_id,
            "groupName": self.cfg.group_name,
            "clusterName": self.cfg.cluster_name,
        }
        if healthy_only:
            query["healthyOnly"] = "true"
        data = self._do("GET", PATH_INSTANCE_LIST, query, None)
        return _as_list(data)

    def list_services(self, page_no: int = 1, page_size: int = 100) -> List[str]:
        self._require_enabled()
        query = {
            "pageNo": str(page_no),
            "pageSize": str(page_size),
            "namespaceId": self.cfg.namespace_id,
        }
        data = self._do("GET", PATH_SERVICE_LIST, query, None)
        return [str(name) for name in _as_list(data)]

    def ping(self) -> None:
        """Raise if the registry is not answering. For health endpoints."""
        self._require_enabled()
        self._do("GET", PATH_HEALTH, None, None)

    # ------------------------------------------------------------------
    # address
    # ------------------------------------------------------------------

    def advertise_ip(self) -> str:
        """The address to register, or raise.

        Deliberately does not sniff a network interface. The tempting
        implementation picks the first non-loopback IPv4 and registers it,
        which on a multi-homed host (netbird + LAN + docker bridge — the
        normal case here) usually picks an address no caller can reach. The
        resulting state — registered, but traffic never arrives — is far
        harder to diagnose than a loud failure at startup.
        """
        if self.cfg.ip:
            return self.cfg.ip
        raise RegistryError(
            "registry: no advertise IP configured for service %r. "
            "Set Config.ip explicitly (e.g. the netbird 100.x address) — "
            "guessing an interface would register an address callers cannot reach."
            % self.cfg.service_name
        )

    # ------------------------------------------------------------------
    # transport
    # ------------------------------------------------------------------

    def _require_enabled(self) -> None:
        if not self.cfg.enabled:
            raise DisabledError("registry client is disabled")

    def _do(
        self,
        method: str,
        path: str,
        query: Optional[Dict[str, str]],
        body: Optional[Dict[str, Any]],
    ) -> Any:
        """One registry call, with one transparent token refresh on 401/403.

        The envelope is unwrapped here, for every call, so no caller can
        forget to. goacos answers some rejected registrations with HTTP 200
        and ``{"code":400,"message":"..."}`` in the body; a client that only
        checks the status code reads that as success, and the instance
        "registers" without ever appearing in the registry. Unwrapping once,
        at the single choke point every call goes through, is what makes that
        impossible rather than merely unlikely.
        """
        try:
            raw = self._send(method, path, query, body)
        except StatusError as exc:
            if exc.status not in (401, 403):
                raise
            self.log.warning(
                "registry rejected credentials; refreshing token and retrying once",
                extra={"path": path, "method": method, "status": exc.status},
            )
            self._invalidate_token()
            try:
                raw = self._send(method, path, query, body)
            except StatusError as retry_exc:
                if retry_exc.status in (401, 403):
                    raise AuthError(str(retry_exc)) from retry_exc
                raise
        return _decode_envelope(raw)

    def _send(
        self,
        method: str,
        path: str,
        query: Optional[Dict[str, str]],
        body: Optional[Dict[str, Any]],
    ) -> Any:
        url = self.base_url() + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        payload = None
        headers = {"Accept": "application/json"}
        token = self._ensure_token()
        if token:
            headers["Authorization"] = "Bearer " + token
        if body is not None:
            payload = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"

        status, raw = self._opener(method, url, payload, headers, self.cfg.request_timeout)
        if status < 200 or status > 299:
            raise StatusError(method, path, status, raw)
        return raw

    def _ensure_token(self) -> str:
        with self._lock:
            if self._token and not self._token_expired():
                return self._token
            if self.cfg.token:
                # A static token is never refreshed or expired locally: the
                # server decides. Re-logging in would fail, since there is
                # no username/password to log in with.
                self._token = self.cfg.token
                return self._token
        return self._login()

    def _token_expired(self) -> bool:
        # Refresh 5 minutes early so a token never expires mid-flight.
        return time.time() >= (self._token_expires_at - 300)

    def _invalidate_token(self) -> None:
        with self._lock:
            self._token = ""
            self._token_expires_at = 0.0

    def _login(self) -> str:
        if not self.cfg.username or not self.cfg.password:
            # No credentials configured is the default deployment (auth
            # disabled), not an error.
            return ""
        url = self.base_url() + PATH_LOGIN
        form = urllib.parse.urlencode(
            {"username": self.cfg.username, "password": self.cfg.password}
        ).encode("utf-8")
        headers = {
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
        }
        status, raw = self._opener("POST", url, form, headers, self.cfg.request_timeout)
        if status < 200 or status > 299:
            raise AuthError(
                "registry login failed: status %d: %s" % (status, (raw or "").strip()[:400])
            )
        try:
            payload = json.loads(raw or "{}")
        except ValueError as exc:
            raise AuthError("registry login returned non-JSON: %s" % (raw or "")[:200]) from exc

        token = payload.get("accessToken") or ""
        ttl = int(payload.get("tokenTtl") or 0)
        data = payload.get("data") or {}
        if not token and isinstance(data, dict):
            token = data.get("accessToken") or ""
            ttl = int(data.get("tokenTtl") or 0)
        if not token:
            raise AuthError("registry login returned no accessToken: %s" % (raw or "")[:200])
        if ttl <= 0:
            # A registry that authenticates but advertises no TTL leaves no
            # basis for a refresh schedule. Assume a conservative lifetime
            # rather than cache forever — caching forever is precisely the
            # failure this exists to prevent.
            ttl = DEFAULT_TOKEN_TTL
            self.log.warning(
                "registry login advertised no token TTL; assuming the default",
                extra={"assumed_ttl": DEFAULT_TOKEN_TTL},
            )
        with self._lock:
            self._token = token
            self._token_expires_at = time.time() + ttl
        return token

    # ------------------------------------------------------------------
    # background loop
    # ------------------------------------------------------------------

    def _sleep_wait(self, seconds: float) -> bool:
        """Wait ``seconds``, returning False the moment stop is requested.

        ``Event.wait`` rather than ``time.sleep`` so that ``stop()`` returns
        promptly instead of blocking for a full heartbeat interval — a
        shutdown that takes up to 30 seconds is a shutdown an operator
        learns to hate. When a custom ``sleep`` is injected (tests,
        deterministic drivers) it is used instead, and its return value
        decides whether to continue.
        """
        if self._sleep is not _interruptible_sleep:
            return self._sleep(seconds)
        return not self._stop.wait(seconds)

    def _run(self) -> None:
        backoff = DEFAULT_REGISTER_BACKOFF
        registered = False
        consecutive_beat_failures = 0

        while not self._stop.is_set():
            if not registered:
                try:
                    self.register()
                    registered = True
                    consecutive_beat_failures = 0
                    backoff = DEFAULT_REGISTER_BACKOFF
                except RegistryError as exc:
                    if self._stop.is_set():
                        return
                    self.log.warning(
                        "registry registration failed; will retry",
                        extra={"service": self.cfg.service_name, "attempt_in": backoff, "error": str(exc)},
                    )
                    if not self._sleep(backoff):
                        return
                    backoff = min(backoff * 2, MAX_REGISTER_BACKOFF)
                    continue

            # Sleep first: a beat immediately after registration is
            # redundant, and it is the interval the server asked for.
            if not self._sleep_wait(self.heartbeat_interval()):
                return

            try:
                self.beat()
                consecutive_beat_failures = 0
                continue
            except RegistryError as exc:
                if self._stop.is_set():
                    return
                consecutive_beat_failures += 1
                self.log.warning(
                    "registry heartbeat failed",
                    extra={"consecutive_failures": consecutive_beat_failures, "error": str(exc)},
                )
                if consecutive_beat_failures >= BEAT_FAILURE_THRESHOLD:
                    # The server may have evicted us (a missed beat past
                    # its 15s/30s thresholds removes the instance outright),
                    # in which case beating is pointless until we register
                    # again. Re-register rather than assume our own state is
                    # still authoritative.
                    self.log.warning(
                        "registry beat failing repeatedly; re-registering",
                        extra={"service": self.cfg.service_name},
                    )
                    registered = False
                    consecutive_beat_failures = 0


# ----------------------------------------------------------------------
# helpers
# ----------------------------------------------------------------------


def _decode_envelope(raw: Any) -> Any:
    """Unwrap the ``{code, message, data}`` envelope.

    A non-zero ``code`` is a registry error even on a 2xx: goacos answers a
    rejected registration with 200 and ``{"code":400,...}`` in some paths,
    and treating that as success is how an instance "registers" and never
    appears.
    """
    if raw is None:
        return None
    if isinstance(raw, (bytes, bytearray)):
        text = raw.decode("utf-8", "replace")
    elif isinstance(raw, str):
        text = raw
    else:
        return raw
    text = text.strip()
    if not text:
        return None
    try:
        envelope = json.loads(text)
    except ValueError:
        return text
    if isinstance(envelope, dict) and "code" in envelope:
        code = envelope.get("code")
        try:
            code = int(code)
        except (TypeError, ValueError):
            code = -1
        if code != 0:
            raise RegistryError(
                "registry error code %s: %s" % (code, envelope.get("message") or text[:200])
            )
        return envelope.get("data")
    return envelope


def _as_list(payload: Any) -> List[Any]:
    if payload is None:
        return []
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict):
        # goacos returns {hosts: [...]} for instance list and {doms: [...]}
        # for service list depending on the endpoint version.
        for key in ("hosts", "doms", "items", "data"):
            value = payload.get(key)
            if isinstance(value, list):
                return value
    return []


def _urlopen(
    method: str,
    url: str,
    payload: Optional[bytes],
    headers: Dict[str, str],
    timeout: float,
) -> "tuple":
    """The real transport. Returns ``(status, body_text)``."""
    request = urllib.request.Request(url=url, data=payload, method=method)
    for key, value in headers.items():
        request.add_header(key, value)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, response.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        # An HTTPError IS a response; its body carries goacos's plain-text
        # reason ("serviceName/ip/port required"), which is far more useful
        # to an operator than the status alone.
        try:
            body = exc.read().decode("utf-8", "replace")
        except Exception:  # pragma: no cover - defensive
            body = ""
        return exc.code, body


def _interruptible_sleep(seconds: float) -> bool:
    """Default sleep: returns True after ``seconds``, False never.

    The cancellable path is :meth:`RegistryClient._sleep_wait`, which the
    loop actually uses. This function exists so an injected ``sleep`` with
    no event of its own still behaves — the return value means "keep going".
    """
    time.sleep(seconds)
    return True
