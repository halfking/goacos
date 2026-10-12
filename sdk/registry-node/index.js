// SPDX-License-Identifier: Apache-2.0
/**
 * goacos-registry — Node/TypeScript client for the goacos service registry.
 *
 * The Node binding of the Go SDK (`goacos/sdk/registry`) and its sibling in
 * Python. All three speak the same wire contract; see `CONTRACT.md` beside
 * this file for the invariants they share and the test that keeps them
 * honest.
 *
 * Why this binding exists
 * -----------------------
 * The platform decided every service registers itself. Without a Node
 * binding that decision is unimplementable for the Node estate
 * (smm-client, desktop-app, aider-desk, official-portal), and the
 * alternative — hand-written `fetch` calls in each service — produces four
 * services with four different heartbeat, retry and shutdown behaviours,
 * none of which match the server's eviction policy.
 *
 * Zero dependencies
 * -----------------
 * Built on the global `fetch` (Node 18+) and `node:timers`. A client that
 * pulled in an HTTP library would be one more version bump to argue about
 * in a container whose lockfile is already contentious.
 *
 * The invariants that matter (identical across all three SDKs)
 * -----------------------------------------------------------
 * 1. Registration never breaks the host application. A registry that is
 *    down must not stop the service from serving.
 * 2. The advertised address is never guessed. With no configured `ip`,
 *    register() throws. Sniffing an interface on a multi-homed host
 *    (netbird + LAN + docker bridge) usually picks an address no caller can
 *    reach, producing "registered but traffic never arrives".
 * 3. The server owns the heartbeat cadence — `clientBeatInterval` is
 *    adopted, because the server also owns eviction (unhealthy 15s,
 *    removed 30s).
 * 4. Retry backoff caps at 30s. A client whose backoff grows past the
 *    eviction window will never come back.
 * 5. Three failed beats mean re-register.
 * 6. A non-zero `code` in a 2xx envelope is an error. goacos answers some
 *    rejected registrations with HTTP 200 and `{"code":400,...}`; treating
 *    that as success is how an instance "registers" and never appears.
 */

'use strict';

// Wire paths. Relative to the "/nacos" context path.
const PATH_PREFIX = '/nacos';
const PATH_INSTANCE = '/v2/ns/instance';
const PATH_BEAT = '/v2/ns/instance/beat';
const PATH_INSTANCE_LIST = '/v2/ns/instance/list';
const PATH_SERVICE_LIST = '/v2/ns/service/list';
const PATH_HEALTH = '/actuator/health';
const PATH_LOGIN = '/v1/auth/users/login';

// Lifecycle tuning. These mirror the Go and Python SDKs exactly; changing
// one side without the others is caught by the conformance test.
const DEFAULT_GROUP_NAME = 'DEFAULT_GROUP';
const DEFAULT_CLUSTER_NAME = 'DEFAULT';
const DEFAULT_REGISTER_BACKOFF_MS = 1000;
const MAX_REGISTER_BACKOFF_MS = 30000;
const BEAT_FAILURE_THRESHOLD = 3;
const MIN_ADVERTISED_BEAT_MS = 50;
const DEFAULT_TOKEN_TTL_MS = 5 * 60 * 60 * 1000;
const DEFAULT_REQUEST_TIMEOUT_MS = 5000;
const DEREGISTER_TIMEOUT_MS = 2000;
const TOKEN_REFRESH_MARGIN_MS = 5 * 60 * 1000;

class RegistryError extends Error {}
class DisabledError extends RegistryError {}
class AuthError extends RegistryError {}

/** A non-2xx answer, carrying the status so callers branch on the code. */
class StatusError extends RegistryError {
  constructor(method, path, status, body = '') {
    const detail = String(body || '').trim().slice(0, 400);
    super(
      `registry ${method} ${path}: status ${status}${detail ? ': ' + detail : ''}`
    );
    this.name = 'StatusError';
    this.method = method;
    this.path = path;
    this.status = status;
    this.body = body;
  }
}

function noopLogger() {
  return {
    info() {},
    warn() {},
    error() {},
    debug() {},
  };
}

/**
 * Normalize a partial config into the full shape the client uses.
 * Mirrors registry.Config in Go and Config in Python.
 */
function normalizeConfig(config) {
  if (!config || typeof config !== 'object') {
    throw new Error('registry: config object is required');
  }
  if (!config.serviceName) {
    throw new Error('registry: config.serviceName is required');
  }
  const port = Number(config.port);
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    // A port of 0 or NaN is the shape of a mis-wired env var, and the
    // server would answer "serviceName/ip/port required" — an error that
    // points at the registry instead of at the service's own config.
    throw new Error(`registry: config.port must be a TCP port, got ${config.port}`);
  }
  const weight = Number(config.weight);
  return {
    enabled: config.enabled !== false,
    serverAddr: (config.serverAddr || 'http://127.0.0.1:8848').replace(/\/+$/, ''),
    serviceName: config.serviceName,
    // Trim before the fallback test: a group/cluster of "   " is not an
    // unset name to be filled in later, it is a DIFFERENT, unreachable name —
    // the instance lands in a bucket nobody queries while every layer above
    // reports success. The Python and Go bindings both treat whitespace as
    // unset; without this the three SDKs disagree on the same config.
    namespaceId: (typeof config.namespaceId === 'string' ? config.namespaceId.trim() : '') || '',
    groupName: (typeof config.groupName === 'string' ? config.groupName.trim() : '') || DEFAULT_GROUP_NAME,
    clusterName: (typeof config.clusterName === 'string' ? config.clusterName.trim() : '') || DEFAULT_CLUSTER_NAME,
    ip: config.ip || '',
    port,
    weight: weight > 0 ? weight : 1.0,
    metadata: config.metadata || {},
    username: config.username || '',
    password: config.password || '',
    token: config.token || '',
    heartbeatIntervalMs:
      Number(config.heartbeatIntervalMs) > 0 ? Number(config.heartbeatIntervalMs) : 5000,
    requestTimeoutMs:
      Number(config.requestTimeoutMs) > 0
        ? Number(config.requestTimeoutMs)
        : DEFAULT_REQUEST_TIMEOUT_MS,
  };
}

/** Unwrap `{code, message, data}`; a non-zero code is an error. */
function decodeEnvelope(text) {
  const trimmed = String(text || '').trim();
  if (!trimmed) return null;
  let parsed;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return trimmed;
  }
  if (parsed && typeof parsed === 'object' && 'code' in parsed) {
    const code = Number(parsed.code);
    if (Number.isFinite(code) && code !== 0) {
      throw new RegistryError(
        `registry error code ${parsed.code}: ${parsed.message || trimmed.slice(0, 200)}`
      );
    }
    return parsed.data === undefined ? null : parsed.data;
  }
  return parsed;
}

function asList(payload) {
  if (payload == null) return [];
  if (Array.isArray(payload)) return payload;
  if (typeof payload === 'object') {
    for (const key of ['hosts', 'doms', 'items', 'data']) {
      if (Array.isArray(payload[key])) return payload[key];
    }
  }
  return [];
}

function sleep(ms, signal) {
  return new Promise((resolve) => {
    if (signal && signal.aborted) {
      resolve(false);
      return;
    }
    const timer = setTimeout(() => {
      if (signal) signal.removeEventListener('abort', onAbort);
      resolve(true);
    }, ms);
    function onAbort() {
      clearTimeout(timer);
      resolve(false);
    }
    if (signal) signal.addEventListener('abort', onAbort, { once: true });
  });
}

class RegistryClient {
  /**
   * @param {object} config see normalizeConfig
   * @param {object} [deps] { logger, fetch, sleep } — injection points so the
   *   lifecycle can be driven deterministically without a server.
   */
  constructor(config, deps = {}) {
    this.cfg = normalizeConfig(config);
    this.log = deps.logger || noopLogger();
    this._fetch = deps.fetch || globalThis.fetch;
    this._sleep = deps.sleep || sleep;
    if (typeof this._fetch !== 'function') {
      throw new Error(
        'registry: global fetch is unavailable; Node 18+ is required or a fetch must be injected'
      );
    }

    this._token = this.cfg.token;
    this._tokenExpiresAt = 0;
    this._heartbeatIntervalMs = this.cfg.heartbeatIntervalMs;
    this._registered = false;
    this._running = false;
    this._abort = null;
    this._loop = null;
  }

  get registered() {
    return this._registered;
  }

  heartbeatIntervalMs() {
    return this._heartbeatIntervalMs;
  }

  baseUrl() {
    return this.cfg.serverAddr + PATH_PREFIX;
  }

  /**
   * Begin registering in the background. Returns immediately and never
   * blocks on the registry.
   *
   * Calling start twice is a no-op rather than an error: a service that
   * starts its HTTP server and its background jobs from two places should
   * not have to coordinate.
   */
  start() {
    if (!this.cfg.enabled) {
      this.log.info(`registry disabled; ${this.cfg.serviceName} will not register`);
      return;
    }
    if (this._running) return;
    this._running = true;
    this._abort = new AbortController();
    this._loop = this._run(this._abort.signal).catch((err) => {
      // The loop is designed never to reject. Reaching here means a bug in
      // the loop itself, and it must still not become an unhandled
      // rejection that kills the host process.
      this.log.error(`registry loop stopped unexpectedly: ${err && err.message}`);
    });
  }

  /** Deregister and stop the background loop. Safe to call twice. */
  async stop(timeoutMs = DEREGISTER_TIMEOUT_MS + 1000) {
    if (!this._running) {
      // Still deregister: stop() may be called on a client that never
      // started but did register synchronously.
      if (this.cfg.enabled) {
        try {
          await this.deregister();
        } catch (err) {
          this.log.warn(`registry deregister failed (best effort): ${err && err.message}`);
        }
      }
      this._registered = false;
      return;
    }
    this._running = false;
    if (this._abort) this._abort.abort();
    if (this._loop) {
      await Promise.race([
        this._loop,
        new Promise((resolve) => setTimeout(resolve, timeoutMs)),
      ]);
    }
    this._loop = null;
    this._abort = null;
    if (this.cfg.enabled) {
      try {
        await this.deregister();
      } catch (err) {
        // Deregistration is best-effort: the server evicts ephemeral
        // instances that stop beating anyway. A wedged registry must not
        // turn shutdown into a hang; it only turns a 15-second window of
        // phantom routing into an immediate one.
        this.log.warn(`registry deregister failed (best effort): ${err && err.message}`);
      }
    }
    this._registered = false;
  }

  /** Register this instance. Idempotent on the server. */
  async register() {
    this._requireEnabled();
    const address = this.advertiseIp();
    const body = {
      serviceName: this.cfg.serviceName,
      ip: address,
      port: this.cfg.port,
      namespaceId: this.cfg.namespaceId,
      groupName: this.cfg.groupName,
      clusterName: this.cfg.clusterName,
      weight: this.cfg.weight,
      enabled: true,
      ephemeral: true,
      metadata: this.cfg.metadata,
    };
    await this._do('POST', PATH_INSTANCE, null, body);
    this._registered = true;
    this.log.info(
      `registry instance registered ${this.cfg.serviceName} ${address}:${this.cfg.port}`
    );
  }

  /** Send one heartbeat and adopt the server's advertised cadence. */
  async beat() {
    this._requireEnabled();
    const address = this.advertiseIp();
    const query = {
      serviceName: this.cfg.serviceName,
      ip: address,
      port: String(this.cfg.port),
      clusterName: this.cfg.clusterName,
    };
    const payload = await this._do('POST', PATH_BEAT, query, null);
    const raw = payload && typeof payload === 'object' ? payload.clientBeatInterval : 0;
    const advertised = Number(raw);
    if (Number.isFinite(advertised) && advertised > 0) {
      // Clamp absurdly small values so a misconfigured server cannot turn
      // every registered instance into a busy loop. No upper clamp on
      // purpose: the server owns its own eviction policy.
      const interval = Math.max(advertised, MIN_ADVERTISED_BEAT_MS);
      const previous = this._heartbeatIntervalMs;
      this._heartbeatIntervalMs = interval;
      if (previous !== interval) {
        this.log.info(
          `registry heartbeat interval adopted from server: ${previous}ms -> ${interval}ms`
        );
      }
    }
  }

  /** Remove this instance from the registry. */
  async deregister() {
    this._requireEnabled();
    const address = this.advertiseIp();
    const query = {
      serviceName: this.cfg.serviceName,
      ip: address,
      port: String(this.cfg.port),
      clusterName: this.cfg.clusterName,
      namespaceId: this.cfg.namespaceId,
      groupName: this.cfg.groupName,
    };
    await this._do('DELETE', PATH_INSTANCE, query, null);
    this._registered = false;
  }

  /** List a service's instances. */
  async listInstances(serviceName, healthyOnly = true) {
    this._requireEnabled();
    const query = {
      serviceName,
      namespaceId: this.cfg.namespaceId,
      groupName: this.cfg.groupName,
      clusterName: this.cfg.clusterName,
    };
    if (healthyOnly) query.healthyOnly = 'true';
    return asList(await this._do('GET', PATH_INSTANCE_LIST, query, null));
  }

  async listServices(pageNo = 1, pageSize = 100) {
    this._requireEnabled();
    const query = {
      pageNo: String(pageNo),
      pageSize: String(pageSize),
      namespaceId: this.cfg.namespaceId,
    };
    return asList(await this._do('GET', PATH_SERVICE_LIST, query, null)).map(String);
  }

  /** Throw if the registry is not answering. For health endpoints. */
  async ping() {
    this._requireEnabled();
    await this._do('GET', PATH_HEALTH, null, null);
  }

  /**
   * The address to register, or throw.
   *
   * Deliberately does not sniff a network interface: on a host with
   * netbird + LAN + docker bridge, the first non-loopback IPv4 is almost
   * always one no caller can reach, and the resulting "registered but
   * traffic never arrives" is far harder to diagnose than a loud failure.
   */
  advertiseIp() {
    if (this.cfg.ip) return this.cfg.ip;
    throw new RegistryError(
      `registry: no advertise IP configured for service ${this.cfg.serviceName}. ` +
        'Set config.ip explicitly (e.g. the netbird 100.x address) — guessing an ' +
        'interface would register an address callers cannot reach.'
    );
  }

  // --------------------------------------------------------------------
  // transport
  // --------------------------------------------------------------------

  _requireEnabled() {
    if (!this.cfg.enabled) throw new DisabledError('registry client is disabled');
  }

  /** One call, with one transparent token refresh on 401/403. */
  async _do(method, path, query, body) {
    try {
      return await this._send(method, path, query, body);
    } catch (err) {
      if (!(err instanceof StatusError) || (err.status !== 401 && err.status !== 403)) {
        throw err;
      }
      this.log.warn(
        `registry rejected credentials; refreshing token and retrying once (${method} ${path})`
      );
      this._invalidateToken();
      try {
        return await this._send(method, path, query, body);
      } catch (retryErr) {
        if (retryErr instanceof StatusError && (retryErr.status === 401 || retryErr.status === 403)) {
          // Give up rather than loop: an unbounded refresh loop against a
          // registry with the wrong password is a self-inflicted DoS.
          throw new AuthError(String(retryErr.message));
        }
        throw retryErr;
      }
    }
  }

  async _send(method, path, query, body) {
    let url = this.baseUrl() + path;
    if (query) {
      const search = new URLSearchParams(query).toString();
      if (search) url += '?' + search;
    }
    const headers = { Accept: 'application/json' };
    const token = await this._ensureToken();
    if (token) headers.Authorization = 'Bearer ' + token;
    let payload;
    if (body !== undefined && body !== null) {
      payload = JSON.stringify(body);
      headers['Content-Type'] = 'application/json';
    }

    const response = await this._fetch(url, { method, headers, body: payload });
    const text = await response.text();
    if (!response.ok) {
      // The body is quoted back because goacos answers a bad register with
      // a 400 and the literal string "serviceName/ip/port required", which
      // is far more useful than the status alone.
      throw new StatusError(method, path, response.status, text);
    }
    // Unwrapped here, at the single choke point every call goes through,
    // so no caller can forget to — a 200 with a non-zero code is an error.
    return decodeEnvelope(text);
  }

  async _ensureToken() {
    if (this._token && !this._tokenExpired()) return this._token;
    if (this.cfg.token) {
      // A static token is never refreshed or expired locally: the server
      // decides. Re-logging-in would fail, since there are no credentials.
      this._token = this.cfg.token;
      return this._token;
    }
    return this._login();
  }

  _tokenExpired() {
    return Date.now() >= this._tokenExpiresAt - TOKEN_REFRESH_MARGIN_MS;
  }

  _invalidateToken() {
    this._token = '';
    this._tokenExpiresAt = 0;
  }

  async _login() {
    if (!this.cfg.username || !this.cfg.password) {
      // No credentials configured is the default deployment (auth
      // disabled), not an error.
      return '';
    }
    const url = this.baseUrl() + PATH_LOGIN;
    const form = new URLSearchParams({
      username: this.cfg.username,
      password: this.cfg.password,
    }).toString();
    const response = await this._fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Accept: 'application/json',
      },
      body: form,
    });
    const text = await response.text();
    if (!response.ok) {
      throw new AuthError(`registry login failed: status ${response.status}: ${text.slice(0, 400)}`);
    }
    let payload;
    try {
      payload = JSON.parse(text || '{}');
    } catch (err) {
      throw new AuthError(`registry login returned non-JSON: ${text.slice(0, 200)}`);
    }
    const data = payload.data || {};
    const token = payload.accessToken || data.accessToken || '';
    let ttlMs = Number(payload.tokenTtl || data.tokenTtl || 0);
    if (!token) {
      throw new AuthError(`registry login returned no accessToken: ${text.slice(0, 200)}`);
    }
    if (!Number.isFinite(ttlMs) || ttlMs <= 0) {
      // A registry that authenticates but advertises no TTL leaves no
      // basis for a refresh schedule. Assume a conservative lifetime rather
      // than cache forever — caching forever is the failure this prevents.
      ttlMs = DEFAULT_TOKEN_TTL_MS;
      this.log.warn('registry login advertised no token TTL; assuming the default');
    }
    this._token = token;
    this._tokenExpiresAt = Date.now() + ttlMs;
    return token;
  }

  // --------------------------------------------------------------------
  // background loop
  // --------------------------------------------------------------------

  async _run(signal) {
    let backoffMs = DEFAULT_REGISTER_BACKOFF_MS;
    let registered = false;
    let consecutiveBeatFailures = 0;

    while (!signal.aborted) {
      if (!registered) {
        try {
          // Register before the first sleep. If we slept first, the
          // registry would have no view of this instance for a full
          // interval — which is exactly the window an operator checks when
          // asking "is this service there at all".
          await this.register();
          registered = true;
          consecutiveBeatFailures = 0;
          backoffMs = DEFAULT_REGISTER_BACKOFF_MS;
        } catch (err) {
          if (signal.aborted) return;
          this.log.warn(
            `registry registration failed; will retry in ${backoffMs}ms: ${err && err.message}`
          );
          if (!(await this._sleep(backoffMs, signal))) return;
          // Capped at the eviction window on purpose: retrying slower than
          // 30s means the instance is already gone when we try again.
          backoffMs = Math.min(backoffMs * 2, MAX_REGISTER_BACKOFF_MS);
          continue;
        }
      }

      // Sleep first: a beat immediately after registration is redundant.
      if (!(await this._sleep(this._heartbeatIntervalMs, signal))) return;

      try {
        await this.beat();
        consecutiveBeatFailures = 0;
        continue;
      } catch (err) {
        if (signal.aborted) return;
        consecutiveBeatFailures += 1;
        this.log.warn(
          `registry heartbeat failed (${consecutiveBeatFailures} consecutive): ${err && err.message}`
        );
        if (consecutiveBeatFailures >= BEAT_FAILURE_THRESHOLD) {
          // The server may have evicted us (a missed beat past its 15s/30s
          // thresholds removes the instance outright), in which case
          // beating is pointless until we register again.
          this.log.warn('registry beat failing repeatedly; re-registering');
          registered = false;
          consecutiveBeatFailures = 0;
        }
      }
    }
  }
}

module.exports = {
  RegistryClient,
  RegistryError,
  StatusError,
  AuthError,
  DisabledError,
  DEFAULT_GROUP_NAME,
  DEFAULT_CLUSTER_NAME,
  // Exported for the conformance test, which asserts the three SDKs agree.
  CONSTANTS: {
    PATH_PREFIX,
    PATH_INSTANCE,
    PATH_BEAT,
    PATH_INSTANCE_LIST,
    PATH_SERVICE_LIST,
    PATH_HEALTH,
    PATH_LOGIN,
    DEFAULT_REGISTER_BACKOFF_MS,
    MAX_REGISTER_BACKOFF_MS,
    BEAT_FAILURE_THRESHOLD,
    MIN_ADVERTISED_BEAT_MS,
    DEFAULT_TOKEN_TTL_MS,
  },
};
