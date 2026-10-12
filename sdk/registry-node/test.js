'use strict';

/**
 * Tests for the goacos Node binding.
 *
 * Run with: node --test sdk/registry-node/   (Node 18+)
 * or:        node sdk/registry-node/test.js
 *
 * Written against node:test so no test framework dependency is needed — the
 * same rule the Python binding follows. A client whose tests need a runner
 * installed is a client some container will not be able to verify.
 */

const test = require('node:test');
const assert = require('node:assert/strict');

const {
  RegistryClient,
  RegistryError,
  StatusError,
  AuthError,
  DisabledError,
  DEFAULT_GROUP_NAME,
  DEFAULT_CLUSTER_NAME,
  CONSTANTS,
} = require('./index.js');

/**
 * A scripted goacos. Records every call and replays canned answers.
 *
 * Scripted rather than a real HTTP server because the point of these tests
 * is the client's decisions (retry, refresh, re-register), not fetch.
 */
function fakeRegistry(options = {}) {
  const calls = [];
  const state = {
    registerStatus: 200,
    registerBody: {},
    beatStatus: 200,
    beatBody: { clientBeatInterval: 5000 },
    loginStatus: 200,
    loginBody: { accessToken: 'tok-1', tokenTtl: 3600 },
  };
  const impl = async (url, init) => {
    const parsed = new URL(url);
    calls.push({
      method: init.method,
      url,
      path: parsed.pathname,
      query: Object.fromEntries(parsed.searchParams.entries()),
      headers: init.headers || {},
      body: init.body,
    });
    const json = (payload) => ({
      ok: true,
      status: 200,
      async text() {
        return JSON.stringify(payload);
      },
    });
    if (parsed.pathname.endsWith('/auth/users/login')) {
      return {
        ok: state.loginStatus >= 200 && state.loginStatus < 300,
        status: state.loginStatus,
        async text() {
          return JSON.stringify(state.loginBody);
        },
      };
    }
    if (parsed.pathname.endsWith(CONSTANTS.PATH_BEAT)) {
      return {
        ok: state.beatStatus >= 200 && state.beatStatus < 300,
        status: state.beatStatus,
        async text() {
          return JSON.stringify({ code: 0, data: state.beatBody });
        },
      };
    }
    if (parsed.pathname.endsWith(CONSTANTS.PATH_INSTANCE)) {
      return {
        ok: state.registerStatus >= 200 && state.registerStatus < 300,
        status: state.registerStatus,
        async text() {
          return JSON.stringify({ code: 0, data: state.registerBody });
        },
      };
    }
    return { ok: false, status: 404, async text() { return 'not found'; } };
  };
  return { fetch: impl, calls, state };
}

function makeClient(fake, config = {}, deps = {}) {
  return new RegistryClient(
    {
      serviceName: 'llm-router',
      port: 8080,
      ip: '100.106.177.3',
      serverAddr: 'http://goacos:8848',
      ...config,
    },
    { fetch: fake.fetch, logger: deps.logger, sleep: deps.sleep }
  );
}

// ---------------------------------------------------------------- config

test('config requires a service name', () => {
  assert.throws(() => new RegistryClient({ port: 80 }, { fetch: async () => {} }), /serviceName/);
});

test('config rejects an impossible port', () => {
  // A port of 0 or NaN is a mis-wired env var; the server would answer
  // "serviceName/ip/port required", pointing at the registry instead of at
  // this service's own config.
  for (const bad of [0, -1, 70000, 'eighty']) {
    assert.throws(
      () => new RegistryClient({ serviceName: 'svc', port: bad }, { fetch: async () => {} }),
      /port/,
      `port ${bad} should be refused`
    );
  }
});

test('config defaults group and weight', () => {
  const client = new RegistryClient({ serviceName: 'svc', port: 80 }, { fetch: async () => {} });
  assert.equal(client.cfg.groupName, DEFAULT_GROUP_NAME);
  assert.equal(client.cfg.weight, 1.0);
  const neg = new RegistryClient({ serviceName: 'svc', port: 80, weight: -3 }, { fetch: async () => {} });
  assert.equal(neg.cfg.weight, 1.0);
});

test('null/undefined cluster and group fall back to defaults', async () => {
  // 这条是跨语言对齐的回归用例。Python 绑定曾在这里出事：
  // sidecar 写 `cluster_name=env(...) or None`，而 Python 的 None
  // 是**显式赋值**，会覆盖 dataclass 自己的默认值，于是字面量
  // cluster="None" 上线——同一实例写出两行、注销只删得掉一行，
  // 而且两行都不在 DEFAULT_GROUP 下，网关按默认组取实例时看不到它。
  //
  // Node 的 `|| DEFAULT` 对 null/undefined/'' 都生效，Go 的 orString
  // 对空串生效。三种绑定必须在同一条不变式上等价，所以这里显式验一遍。
  // 纯空白必须与未设置同等对待：它是**另一个没人查询的名字**，
  // 不是「稍后再填」。Python / Go 两端都 trim，三端要一致。
  for (const bad of [null, undefined, '', '   ', '\t']) {
    const client = new RegistryClient(
      { serviceName: 'svc', port: 80, clusterName: bad, groupName: bad },
      { fetch: async () => {} },
    );
    assert.equal(client.cfg.clusterName, DEFAULT_CLUSTER_NAME,
      `clusterName=${JSON.stringify(bad)} 没落到默认值`);
    assert.equal(client.cfg.groupName, DEFAULT_GROUP_NAME,
      `groupName=${JSON.stringify(bad)} 没落到默认值`);
  }

  // 不只看字段，还要看**真的发出去的那份**。
  const fake = fakeRegistry();
  const client = makeClient(fake, {}, { clusterName: null, groupName: null });
  await client.register();
  const body = JSON.parse(fake.calls[0].body);
  assert.equal(body.clusterName, DEFAULT_CLUSTER_NAME);
  assert.equal(body.groupName, DEFAULT_GROUP_NAME);
  assert.notEqual(body.clusterName, 'None');
  assert.notEqual(body.clusterName, 'null');
});

// ------------------------------------------------------------- wire paths

test('wire paths match the go and python SDKs', async () => {
  const fake = fakeRegistry();
  const client = makeClient(fake);
  await client.register();
  await client.beat();
  assert.ok(
    fake.calls[0].path.startsWith('/nacos/v2/ns/instance'),
    `register must hit /nacos/v2/ns/instance, got ${fake.calls[0].path}`
  );
  assert.equal(fake.calls[1].path, '/nacos/v2/ns/instance/beat');
});

test('register body carries the instance metadata contract', async () => {
  // The platform's routing depends on these metadata keys; a binding that
  // spells them differently registers an instance the gateway cannot place.
  const fake = fakeRegistry();
  const client = makeClient(fake, {
    metadata: { app: 'llm', component: 'router', role: 'primary', tier: 'edge' },
  });
  await client.register();
  const body = JSON.parse(fake.calls[0].body);
  for (const key of [
    'serviceName', 'ip', 'port', 'namespaceId', 'groupName',
    'clusterName', 'weight', 'enabled', 'ephemeral', 'metadata',
  ]) {
    assert.ok(key in body, `register body missing ${key}`);
  }
  assert.equal(body.metadata.app, 'llm');
  assert.equal(body.metadata.component, 'router');
  assert.equal(body.ephemeral, true);
  assert.equal(body.port, 8080);
});

test('beat carries the full instance key', async () => {
  // The server keys an instance by (serviceName, ip, port, clusterName). A
  // beat missing `ip` addresses a DIFFERENT instance, so it returns 200
  // while the real instance ages out and is evicted.
  const fake = fakeRegistry();
  const client = makeClient(fake);
  await client.beat();
  assert.deepEqual(fake.calls[0].query, {
    serviceName: 'llm-router',
    ip: '100.106.177.3',
    port: '8080',
    clusterName: 'DEFAULT',
  });
});

test('deregister carries the namespace and group', async () => {
  const fake = fakeRegistry();
  const client = makeClient(fake, { namespaceId: 'prod', groupName: 'EDGE' });
  await client.deregister();
  assert.equal(fake.calls[0].query.namespaceId, 'prod');
  assert.equal(fake.calls[0].query.groupName, 'EDGE');
  assert.equal(fake.calls[0].method, 'DELETE');
});

test('refuses to guess the advertise IP', async () => {
  // Sniffing an interface on a host with netbird + LAN + docker bridge
  // picks an address no caller can reach: "registered but traffic never
  // arrives". A loud failure at registration is far cheaper.
  const client = makeClient(fakeRegistry(), { ip: '' });
  await assert.rejects(() => client.register(), /advertise IP/);
});

test('disabled client refuses every network call', async () => {
  const client = makeClient(fakeRegistry(), { enabled: false });
  client.start();
  await assert.rejects(() => client.register(), DisabledError);
  await assert.rejects(() => client.beat(), DisabledError);
  await assert.rejects(() => client.ping(), DisabledError);
});

// -------------------------------------------------------- heartbeat cadence

test('adopts the servers advertised interval', async () => {
  // The server owns the eviction policy (unhealthy 15s, removed 30s). A
  // client with its own hardcoded timer will eventually be evicted while
  // believing it is healthy.
  const fake = fakeRegistry();
  fake.state.beatBody = { clientBeatInterval: 2000 };
  const client = makeClient(fake, { heartbeatIntervalMs: 5000 });
  assert.equal(client.heartbeatIntervalMs(), 5000);
  await client.beat();
  assert.equal(client.heartbeatIntervalMs(), 2000);
});

test('clamps an absurd advertised interval', async () => {
  // 0ms advertised would otherwise become a busy loop hammering the
  // registry from every registered instance at once.
  const fake = fakeRegistry();
  fake.state.beatBody = { clientBeatInterval: 0 };
  const client = makeClient(fake);
  await client.beat();
  assert.ok(client.heartbeatIntervalMs() >= 50);
});

test('ignores a missing interval', async () => {
  const fake = fakeRegistry();
  fake.state.beatBody = {};
  const client = makeClient(fake, { heartbeatIntervalMs: 7000 });
  await client.beat();
  assert.equal(client.heartbeatIntervalMs(), 7000);
});

// ------------------------------------------------------------------- tokens

test('login then authorizes', async () => {
  const fake = fakeRegistry();
  const client = makeClient(fake, { username: 'u', password: 'p' });
  await client.register();
  assert.ok(
    fake.calls.some((c) => c.headers.Authorization === 'Bearer tok-1'),
    'expected a bearer token from login'
  );
});

test('static token skips login', async () => {
  const fake = fakeRegistry();
  const client = makeClient(fake, { token: 'preset-token' });
  await client.register();
  assert.ok(
    !fake.calls.some((c) => c.path.endsWith('/auth/users/login')),
    'a static token must not trigger a login round trip'
  );
  assert.equal(fake.calls[0].headers.Authorization, 'Bearer preset-token');
});

test('gives up rather than looping on bad credentials', async () => {
  // One transparent retry is right (tokens expire mid-flight). An unbounded
  // refresh loop against a registry with the wrong password is a
  // self-inflicted denial of service.
  const fake = fakeRegistry();
  fake.state.loginStatus = 401;
  const client = makeClient(fake, { username: 'u', password: 'wrong' });
  await assert.rejects(() => client.register(), AuthError);
});

test('missing token in login response is an error', async () => {
  // Caching "no token" would make every later call silently
  // unauthenticated and produce a 401 the caller cannot explain.
  const fake = fakeRegistry();
  fake.state.loginBody = { tokenTtl: 3600 };
  const client = makeClient(fake, { username: 'u', password: 'p' });
  await assert.rejects(() => client.register(), AuthError);
});

test('login without ttl assumes a conservative lifetime', async () => {
  const fake = fakeRegistry();
  fake.state.loginBody = { accessToken: 'tok-2' };
  const client = makeClient(fake, { username: 'u', password: 'p' });
  await client.register();
  assert.equal(client._token, 'tok-2');
  assert.ok(client._tokenExpiresAt > Date.now());
});

test('expired token is refreshed before the next call', async () => {
  // Refresh five minutes early: a token that expires between two
  // 30-second heartbeats would otherwise 401 mid-flight.
  const fake = fakeRegistry();
  const client = makeClient(fake, { username: 'u', password: 'p' });
  await client.register();
  const first = fake.calls.filter((c) => c.path.endsWith('/auth/users/login')).length;

  client._tokenExpiresAt = Date.now() - 1;
  await client.register();

  const logins = fake.calls.filter((c) => c.path.endsWith('/auth/users/login')).length;
  assert.ok(logins > first, 'an expired token must trigger a fresh login, not reuse');
});

// ----------------------------------------------------------------- envelope

test('a non-zero code is an error even on HTTP 200', async () => {
  // goacos answers some rejected registrations with 200 and
  // {"code":400,...}. Treating that as success is how an instance
  // "registers" and never appears in the registry.
  const client = makeClient({
    fetch: async () => ({
      ok: true,
      status: 200,
      async text() {
        return JSON.stringify({ code: 400, message: 'serviceName/ip/port required' });
      },
    }),
  });
  await assert.rejects(() => client.register(), (err) => {
    assert.ok(err instanceof RegistryError);
    assert.match(err.message, /400/);
    return true;
  });
});

test('status error keeps the status for branching', async () => {
  const attempts = [];
  const client = makeClient({
    fetch: async (url) => {
      attempts.push(url);
      return { ok: false, status: 500, async text() { return 'internal error'; } };
    },
  });
  await assert.rejects(() => client.register(), (err) => {
    assert.ok(err instanceof StatusError);
    assert.equal(err.status, 500);
    return true;
  });
  // 500 is not an auth rejection: exactly one attempt, no refresh, no
  // retry, no backoff storm against a broken registry.
  assert.equal(attempts.length, 1);
});

// ---------------------------------------------------------------- lifecycle

test('registers before the first sleep', async () => {
  // If the loop slept first, the registry would have no view of this
  // instance for a full interval — exactly the window an operator checks
  // when asking "is this service there at all".
  const fake = fakeRegistry();
  const slept = [];
  const client = makeClient(fake, {}, {
    sleep: async (ms, signal) => {
      slept.push(ms);
      if (slept.length >= 2) return false;
      return true;
    },
  });
  await client._run(new AbortController().signal);
  const registers = fake.calls.filter((c) => c.path === '/nacos/v2/ns/instance');
  assert.ok(registers.length >= 1, 'expected a registration');
  assert.equal(fake.calls[0].path, '/nacos/v2/ns/instance');
  assert.ok(slept.length >= 1);
});

test('three failed beats trigger re-registration', async () => {
  // The server may have evicted the instance outright; beating on is
  // pointless until we announce ourselves again.
  const fake = fakeRegistry();
  let slept = 0;
  const client = makeClient(fake, {}, {
    sleep: async () => {
      slept += 1;
      return slept <= CONSTANTS.BEAT_FAILURE_THRESHOLD + 2;
    },
  });
  client.beat = async () => {
    throw new StatusError('POST', CONSTANTS.PATH_BEAT, 500, 'registry down');
  };
  await client._run(new AbortController().signal);
  const registers = fake.calls.filter((c) => c.path === '/nacos/v2/ns/instance');
  assert.ok(registers.length >= 2, 'expected a re-registration after repeated beat failures');
});

test('backoff never exceeds the eviction window', () => {
  // Backoff capped past 30s means the instance is already evicted before
  // the next attempt, so it would never come back.
  assert.equal(CONSTANTS.MAX_REGISTER_BACKOFF_MS, 30000);
});

test('a failing registry never rejects out of start/stop', async () => {
  // The load-bearing guarantee: registration failures must not become an
  // unhandled rejection that kills the host process.
  const fake = fakeRegistry();
  fake.state.registerStatus = 500;
  const client = makeClient(fake, {}, { sleep: async () => false });
  client.start();
  await client.stop();
  assert.equal(client.registered, false);
});
