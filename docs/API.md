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
