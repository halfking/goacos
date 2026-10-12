# Nacos HTTP Open API compatibility

goacos targets the Nacos **3.2.x / 2.5.x HTTP Open API**. Below is the endpoint
matrix. "v1" = `/nacos/v1/...` (legacy REST), "v2" = `/nacos/v2/...`
(`{"code":0,"message":"success","data":...}` envelope).

## Config center

| Area | Endpoint | Status |
|---|---|---|
| Get config (raw / `show=all`) | `GET /v1/cs/configs` | ✅ |
| Publish | `POST /v1/cs/configs` | ✅ |
| Delete | `DELETE /v1/cs/configs` | ✅ |
| Long-polling listener (`\x02/\x01` protocol) | `POST /v1/cs/configs/listener` | ✅ |
| Page list (`search=blur/accurate`, tag filter) | `GET /v1/cs/configs` | ✅ |
| Get / publish / delete | `GET/POST/DELETE /v2/cs/config` | ✅ |
| Page list | `GET /v2/cs/config/list` | ✅ |
| Long-polling listener (JSON) | `POST /v2/cs/config/listener` | ✅ |
| History list / detail / previous (revert) | `GET /v1/cs/history*`, `GET /v2/cs/history*` | ✅ |
| Beta / tag publish APIs | `/v1/cs/configs?betaIps=...`, tag config APIs | ❌ roadmap |
| Aggregation (`config_info_aggr`) APIs | `/v1/cs/aggr` | ❌ (table exists) |

## Naming / service discovery

| Area | Endpoint | Status |
|---|---|---|
| Register / update | `POST/PUT /v1/ns/instance` | ✅ |
| Deregister | `DELETE /v1/ns/instance` | ✅ |
| Get single instance | `GET /v1/ns/instance` | ✅ |
| Instance list (ServiceInfo, `healthyOnly`, clusters) | `GET /v1/ns/instance/list` | ✅ |
| Heartbeat (`clientBeatInterval`, code 10200) | `GET/PUT /v1/ns/instance/beat` | ✅ |
| Service list | `GET /v1/ns/service/list` | ✅ |
| Service detail / update / delete | `GET/PUT/DELETE /v1|v2/ns/service` | ✅ |
| Register / get / list / deregister | `POST/GET /v2/ns/instance`, `GET /v2/ns/instance/list` | ✅ |
| Beat | `POST /v2/ns/instance/beat` | ✅ |
| Operator metrics | `GET /v1/ns/operator/metrics` | ✅ (subset) |
| UDP push (legacy 1.x clients) | port 7878 | ❌ (poll instead) |
| gRPC bi-stream (2.x/3.x SDKs) | port 9848 | ❌ roadmap |

## Auth

| Area | Endpoint | Status |
|---|---|---|
| Login (`accessToken`, `tokenTtl`, `globalAdmin`) | `POST /v1/auth/users/login`, `POST /v2/auth/user/login` | ✅ |
| User CRUD (admin) / self password change | `GET/POST/PUT/DELETE /v1/auth/users` | ✅ |
| Roles | `GET/POST/DELETE /v1/auth/roles` | ✅ |

Token sources: `?accessToken=`, `accessToken` cookie, or `Authorization: Bearer`.
JWT HS256 with `GOACOS_AUTH_TOKEN_SECRET`.

## Console / ops

| Area | Endpoint | Status |
|---|---|---|
| Namespace CRUD | `/v1/console/namespaces`, `/v2/console/namespace` | ✅ |
| Server state | `GET /v1/console/server/state` | ✅ |
| Readiness / liveness | `GET /v1/console/health/*` | ✅ |
| Health | `GET /nacos/actuator/health` | ✅ |
| Console UI | `/nacos/index.html` | ✅ (embedded SPA) |
| Export/import config zip | `/v1/cs/configs?export=...` | ❌ roadmap |

## Gateway control plane (goacos-specific)

Everything the embedded gateway data plane is configured by lives under
`/goacos/v1/...` on the **API port** (`GOACOS_PORT`, `8848` by default) — not on
the gateway's own `:80` / `:443` listeners, which carry real inbound traffic.
Like the application model, these are goacos's own surfaces: there is no Nacos
equivalent to stay compatible with, so they are deliberately not under `/nacos/`.

**Response envelope.** Every endpoint below answers with
`{"code":200,"message":"","data":...}` on success and
`{"code":<http-status>,"message":"<error>","data":null}` on failure. `namespaceId`
is an optional query parameter on every read and delete; it defaults to `public`.
Write endpoints accept **either** a JSON body or form-encoded fields with the
same names; `metadata` on applications/components/policies and `lbAlgorithm` on
components are honoured only in the JSON body.

**Certificates never return private key material.** `GET /goacos/v1/certs` and
`GET /goacos/v1/gateway/status` report source, issuer, validity and expiry only.
The key is write-only — `POST /goacos/v1/certs` accepts it, nothing returns it.

### Endpoint matrix

| Area | Endpoint | Status |
|---|---|---|
| Gateway status (snapshot, certificates, per-domain routes) | `GET /goacos/v1/gateway/status` | ✅ |
| Force an immediate routing + certificate reload | `POST /goacos/v1/gateway/refresh` | ✅ |
| Certificate list | `GET /goacos/v1/certs` | ✅ |
| Certificate upload (manual, never auto-renewed) | `POST /goacos/v1/certs` | ✅ |
| Certificate delete | `DELETE /goacos/v1/certs/{domain}` | ✅ |
| Per-domain policy list | `GET /goacos/v1/vhosts` | ✅ |
| Per-domain policy write | `POST /goacos/v1/vhosts`, `PUT /goacos/v1/vhosts/{domain}` | ✅ |
| Per-domain policy delete | `DELETE /goacos/v1/vhosts/{domain}` | ✅ |
| Applications | `GET/POST /goacos/v1/apps` | ✅ |
| Application detail (with components) | `GET /goacos/v1/apps/{app}` | ✅ |
| Application delete (cascades its components and routes) | `DELETE /goacos/v1/apps/{app}` | ✅ |
| Components | `GET/POST /goacos/v1/apps/{app}/components` | ✅ |
| Component write / delete | `PUT/DELETE /goacos/v1/apps/{app}/components/{component}` | ✅ |
| Routes | `GET/POST /goacos/v1/routes` | ✅ |
| Route delete | `DELETE /goacos/v1/routes/{domain}` | ✅ |
| Route resolve (policy + live healthy instances) | `GET /goacos/v1/route/resolve` | ✅ |

### `GET /goacos/v1/gateway/status`

No parameters. Answers "is the gateway doing its job, and what is it doing" in one
call, so an operator does not have to curl the site being diagnosed.

| `data` field | Meaning |
|---|---|
| `enabled` | `false` when `GOACOS_GATEWAY_ENABLED` is off |
| `reason` | present only when `enabled` is `false` |
| `namespace` | the namespace this instance serves (`GOACOS_GATEWAY_NAMESPACE`) |
| `listeners` | `{"http":"...","https":"..."}` — the bound addresses |
| `policyDefaults` | the gateway-wide defaults in effect (`forceHttps`, `hstsMaxAge`, `maxBodyBytes`, the three timeouts, `websocket`, `trustForwardedFor`, `staticRoot`, `staticFallback`, `refreshSeconds`) |
| `routing` | `{"loadedAt","ageSeconds","domainCount","error"}` — snapshot freshness |
| `acme` | `{"enabled","directory","email","dataDir","challengeEndpoint","pending","backingOff"}` |
| `domains` | one entry per declared domain |
| `certificates` | one entry per stored certificate |
| `counters` | `requests`, `proxied`, `static`, `redirects`, `rejected`, `noRoute`, `pooledUpstreams` |
| `uptimeSeconds` | server uptime |

Each `domains[]` entry carries `"domain"`, one key **per declared path prefix**
(each holding `app`, `component`, `kind`, `protocol`, `service`, `group`, `lb`,
`instances[]`, `healthy`) and the TLS summary `tls` / `tlsIssuer` / `tlsExpiry` /
`tlsDaysLeft` / `tlsWarning`:

```json
{
  "code": 200,
  "message": "",
  "data": {
    "domain": "app.example.com",
    "/api": {
      "app": "shop", "component": "api", "kind": "api", "protocol": "http",
      "service": "shop-api", "group": "DEFAULT_GROUP", "lb": "round_robin",
      "instances": ["10.0.0.7:8080", "10.0.0.8:8080"], "healthy": 2
    },
    "tls": "acme", "tlsIssuer": "R3", "tlsExpiry": "2027-01-14T00:00:00Z", "tlsDaysLeft": 89
  }
}
```

`tls` is `"none"` when the domain has no certificate yet, in which case
`tlsWarning` explains that issuance is pending or not eligible. When the gateway
is disabled the whole response collapses to:

```json
{"code":200,"message":"","data":{"enabled":false,"reason":"gateway data plane is disabled; set GOACOS_GATEWAY_ENABLED=true"}}
```

### `POST /goacos/v1/gateway/refresh`

No body. Reloads routes and certificates from the database immediately instead of
waiting out `GOACOS_GATEWAY_REFRESH_SECONDS`. Returns `400` (`"gateway is not
enabled"`) when the data plane is off.

```json
{"code":200,"message":"","data":{"reloadedAt":"2026-10-12T13:20:11Z","error":null}}
```

### Certificates

`GET /goacos/v1/certs` — query: `namespaceId`. `data` is an array:

| Field | Meaning |
|---|---|
| `namespaceId`, `domain` | identity (the domain is normalized: lowercased, port and trailing dot stripped) |
| `source` | `manual`, `acme` or `self_signed` |
| `issuer`, `notBefore`, `notAfter`, `daysLeft` | certificate validity |
| `status` | `active`, `expiring`, `pending` or `failed` |
| `lastError` | last issuance error, empty when there is none |
| `autoRenew` | `true` only for `acme` — a manual or self-signed certificate is never renewed automatically |
| `sourceWarning` | the one thing an operator must not miss (issuance failure, self-signed, expired, inside the renewal window); empty when there is nothing to say |

Neither the private key nor `certPem` appears in this response.

`POST /goacos/v1/certs` — body (JSON or form): `namespaceId`, `domain`,
`certPem`, `keyPem`. `domain` is required, and both PEMs are required. A
certificate that does not cover its own domain, or a key that does not match it,
is rejected with `400` rather than served.

```json
{
  "code": 200,
  "message": "",
  "data": {
    "domain": "app.example.com",
    "source": "manual",
    "notAfter": "2027-01-14T00:00:00Z",
    "daysLeft": 365,
    "note": "manual certificates are never auto-renewed; upload a replacement before expiry"
  }
}
```

The upload works even when the gateway data plane is disabled, so certificate
configuration can be staged before the listeners are switched on.

`DELETE /goacos/v1/certs/{domain}` — query: `namespaceId`. Removes the row and
drops the in-memory copy, so the gateway stops serving it immediately. `data` is
`{"deleted":"<domain>","note":"any serving domain will fall back to a self-signed certificate until issuance succeeds"}`.

### Per-domain policy (vhosts)

Zero values mean "inherit the gateway default", which is why the row can be
sparse.

`GET /goacos/v1/vhosts` — query: `namespaceId`. `data` is an array of
`namespaceId`, `domain`, `forceHttps`, `hstsMaxAge`, `allowIps`, `denyIps`,
`clientMaxBodyBytes`, `connectTimeoutMs`, `readTimeoutMs`, `sendTimeoutMs`,
`websocket`, `metadata`.

`POST /goacos/v1/vhosts` and `PUT /goacos/v1/vhosts/{domain}` — body:
`namespaceId`, `domain`, `forceHttps`, `hstsMaxAge`, `allowIps`, `denyIps`,
`clientMaxBodyBytes`, `connectTimeoutMs`, `readTimeoutMs`, `sendTimeoutMs`,
`websocket`, `metadata`. `domain` is required in the **body** on both verbs; the
write starts from the currently effective policy, so an omitted field keeps its
value rather than resetting to the default. `allowIps` / `denyIps` are
newline-separated CIDRs or bare addresses, evaluated deny-first. A change here
triggers an immediate routing reload.

```json
{"code":200,"message":"","data":{"created":true,"policy":{"namespaceId":"public","domain":"app.example.com","forceHttps":true,"hstsMaxAge":0,"allowIps":"","denyIps":"","clientMaxBodyBytes":0,"connectTimeoutMs":3000,"readTimeoutMs":60000,"sendTimeoutMs":60000,"websocket":true,"metadata":null}}}
```

`DELETE /goacos/v1/vhosts/{domain}` — query: `namespaceId`. `data` is
`{"deleted":"<domain>","note":"the domain now inherits the gateway-wide defaults"}`.

### Applications, components and routes

These describe **what** to serve; the naming registry describes **who** is
healthy. A component names a service and deliberately holds no IP or port — the
instances are read live at resolve time.

`GET /goacos/v1/apps` — query: `namespaceId`. `data` is an array of
`namespaceId`, `name`, `displayName`, `description`, `env`, `metadata`.

`POST /goacos/v1/apps` — body: `namespaceId`, `name` (required), `displayName`,
`description`, `env`, `metadata`. `data` is `{"created":<bool>,"name":"<name>"}`.

`GET /goacos/v1/apps/{app}` — query: `namespaceId`. `data` is the application
object plus `components[]`; `404` when the application does not exist.

`DELETE /goacos/v1/apps/{app}` — query: `namespaceId`. Deletes the application
together with its components and routes. Registered instances are **not** touched
— a service may be shared by another application. `data` is `{"deleted":"<app>"}`.

`GET /goacos/v1/apps/{app}/components` — query: `namespaceId`. `data` is an array
of `namespaceId`, `appName`, `name`, `kind`, `protocol`, `serviceName`,
`groupName`, `clusterName`, `healthPath`, `weight`, `lbAlgorithm`, `sticky`,
`metadata`. `kind` is one of `web`, `api`, `worker`, `mcp`, `gateway`;
`protocol` is one of `http`, `https`, `static`, `grpc`, `ws`.

`POST /goacos/v1/apps/{app}/components` and
`PUT /goacos/v1/apps/{app}/components/{component}` — body: `namespaceId`,
`appName`, `name` (required), `serviceName` (required), `kind`, `protocol`,
`groupName`, `clusterName`, `healthPath`, `weight`, `lbAlgorithm`, `sticky`,
`metadata`. `appName` falls back to the `{app}` path segment. Defaults applied
when omitted: `kind=api`, `protocol=http`, `groupName=DEFAULT_GROUP`, `weight=1`,
`lbAlgorithm=round_robin`. `data` is `{"created":<bool>}`.

`DELETE /goacos/v1/apps/{app}/components/{component}` — query: `namespaceId`.
Also removes any route pointing at the component, so the model can never hold a
route to a component that no longer exists. `data` is `{"deleted":true}`.

`GET /goacos/v1/routes` — query: `namespaceId`. `data` is an array of
`namespaceId`, `domain`, `pathPrefix`, `appName`, `componentName`,
`upstreamName`, `priority`.

`POST /goacos/v1/routes` — body: `namespaceId`, `domain`, `appName`,
`componentName` (all three required), `pathPrefix` (defaults to `/`),
`upstreamName`, `priority`. The route is keyed by `(namespace, domain,
pathPrefix)`; `domain` is lowercased. `data` is `{"saved":true}`.

`DELETE /goacos/v1/routes/{domain}` — query: `namespaceId`, `pathPrefix`
(defaults to `/`, so this removes the catch-all route of that domain).
`data` is `{"deleted":true}`.

### `GET /goacos/v1/route/resolve`

The call the gateway's route renderer depends on: one round trip answers "for
this domain and path, which healthy instances serve it, and under what policy".

| Query | Meaning |
|---|---|
| `domain` | required; matched case-insensitively |
| `path` | defaults to `/`; the longest matching prefix wins, ties broken by lower `priority` |
| `namespaceId` | defaults to `public` |
| `metaFilter` | `key=value,key2=value2` — restricts the instance set by instance metadata (canary / lane selection). A malformed pair is dropped, not fatal |

An unresolved domain is a `200` with `"data": null`, not a `404`: "not declared
here" is a normal answer the renderer must be able to tell apart from a database
error. `data`:

```json
{
  "namespaceId": "public",
  "domain": "app.example.com",
  "pathPrefix": "/api",
  "appName": "shop",
  "componentName": "api",
  "componentKind": "api",
  "protocol": "http",
  "upstreamName": "",
  "lbAlgorithm": "round_robin",
  "sticky": "",
  "serviceName": "shop-api",
  "groupName": "DEFAULT_GROUP",
  "clusterName": "",
  "healthPath": "",
  "instances": [
    {"ip":"10.0.0.7","port":8080,"cluster":"DEFAULT","weight":1,
     "healthy":true,"enabled":true,"ephemeral":true,"metadata":{}}
  ]
}
```

## Divergences from Nacos (by design)

1. **Everything is persisted in the configured database (MySQL or
   PostgreSQL, `GOACOS_DB_TYPE`)**, including the service registry. Nacos
   keeps naming data in memory (Distro) — goacos instances survive restarts.
2. **No Raft/Distro**: replicas share MySQL; cross-node config change
   visibility ≤ ~1 s; the ephemeral-instance sweeper is idempotent.
3. Heartbeats **auto-create** missing ephemeral instances instead of returning
   `RESOURCE_NOT_FOUND` for the client to re-register (behaviorally
   equivalent, one round-trip cheaper).
4. Config CAS (`casMd5` optimistic lock) is not enforced yet.
