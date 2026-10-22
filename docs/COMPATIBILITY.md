# goacos gRPC 9848 — Protocol Compatibility Matrix (v2 + v3)

> **Status:** draft, awaiting user sign-off before Phase 2 implementation begins.
> **Scope:** minimal `nacos-sdk-go 2.x` & `nacos-sdk-go 3.x` direct-connect surface,
> with v2/v3 auto-routing inside the same `:9848` gRPC service. No proto regeneration
> (nacos service.proto is identical across v2 and v3 lines — the only divergence is
> the `ConnectionSetupRequest` body and the auth scheme, both of which are decoded at
> runtime).

This document is the **contract** for Phase 2–6. If anything below is wrong, fix it
*here* before code lands.

---

## A. Connection layer (gRPC service definition)

Source of truth: `nacos_grpc_service.proto` (to be vendored at `internal/grpcapi/proto/api.proto` in Phase 2 — not yet in tree).
The proto file is byte-identical between Nacos 2.x and 3.x — same package, same field
tags, same RPC signatures. There is **no separate proto per major version**.

| Item                          | nacos v2 SDK                  | nacos v3 SDK                  | goacos 行为              |
| ----------------------------- | ----------------------------- | ----------------------------- | ----------------------- |
| gRPC package                  | `nacos.api.grpc`              | `nacos.api.grpc`              | identical                |
| Service name                  | `nacos.api.grpc.nacos_grpc_service` | same                    | register same service    |
| RPC: `request`                | bidi-stream                   | bidi-stream                   | Phase 2 target (single stream reused) |
| RPC: `ConfigPublishRequest`   | server-streaming unary         | server-streaming unary         | Phase 2 target (reuse single-stream pub/sub path) |
| RPC: `ConfigBatchListenRequest` | client-streaming unary      | client-streaming unary         | Phase 2 target (reuse single-stream pub/sub path) |
| Listen port                   | HTTP+1000 (default 9848)      | HTTP+1000 (default 9848)      | planned `GOACOS_GRPC_PORT=9848`, configurable |
| TLS                            | optional (`nacos.remote.client.grpc.tls.*`) | same            | Phase 2 ships plaintext; TLS deferred |
| Auth on connect               | none / `username+password`    | `Metadata.token` (JWT) + `tenant` | **Phase 4 branches on `clientVersion`** |

**Metadata envelope** (`Payload.metadata`):

| field   | tag | type              | set by           | goacos use                                  |
| ------- | --- | ----------------- | ---------------- | ------------------------------------------- |
| type    | 3   | string            | client           | dispatch key (e.g. `ConnectionSetupRequest`) |
| clientIp| 8   | string            | client           | log only (Phase 2); for allowlist (deferred) |
| headers | 7   | map<string,string>| client           | cross-call headers (`Authorization`, `Tenant`) |

**Body** (`Payload.body`): `google.protobuf.Any`. We do **not** decode Any — instead
we use `Metadata.type` (the simple class name) as the discriminator, matching Nacos
Java's `PayloadRegistry`.

---

## B. Request/Response matrix (`Metadata.type` dispatch)

Each row is one discriminated message type that flows on the stream. "v2 wire shape"
and "v3 wire shape" describe only the runtime-detected divergence; the proto
**service contract** is identical.

| `Metadata.type`               | Direction | Direction | v2 wire shape                                                                 | v3 wire shape                                                                                                  | goacos Phase 3/4                                    |
| ----------------------------- | --------- | --------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| `ConnectionSetupRequest`      | C → S     | Setup     | `{clientVersion:"2.x", labels:{}, abilityTable:{}}`                            | `{clientVersion:"3.x", tenant:"ns", labels:{}, abilityTable:{...}}`                                              | **Phase 4**: parse → branch by major version         |
| `ConnectionSetupResponse`     | S → C     | Setup     | `{success:true, connectionId, abilityTable:{...}}`                             | same + abilityTable entries `supportRemoteConnection=true`, `supportLongConnection=true`                          | return server capability                             |
| `InstanceRequest`             | C → S     | Naming    | `{type:"registerInstance|deregisterInstance|updateInstance", instance:{...}, namespace, serviceName, groupName}` | same                                                                                                            | route by `type` → `s.ST.RegisterInstance/Beat/DeleteInstance` |
| `BatchInstanceRequest`        | C → S     | Naming    | `{type, instances:[...], namespace, serviceName, groupName}`                  | same                                                                                                            | loop single-register                                 |
| `ServiceQueryRequest`         | C → S     | Naming    | `{namespace, serviceName, groupName, cluster, healthyOnly, udpPort}`          | same                                                                                                            | `s.ST.ListInstances`                                 |
| `ServiceListRequest`          | C → S     | Naming    | `{namespace, groupName, pageNo, pageSize}`                                    | same                                                                                                            | `s.ST.ListServices`                                  |
| `SubscribeServiceRequest`     | C → S     | Naming    | `{namespace, serviceName, groupName, clusters, subscribe}`                    | same                                                                                                            | register in-memory subscriber → push `ServiceResponse` |
| `ServiceResponse`             | S → C     | Naming    | `{serviceName, groupName, clusters, hosts:[{ip,port,healthy,...}], cacheMillis}` | same                                                                                                            | built from `s.ST.ListInstances` on subscriber push    |
| `ConfigQueryRequest`          | C → S     | Config    | `{dataId, group, tenant, tag, requestId}`                                       | `{dataId, group, tenant, tag, requestId}` + (3.0+) `notify` header                                              | `s.ST.GetConfig` → `ConfigResponse`                  |
| `ConfigPublishRequest`        | C → S     | Config    | `{dataId, group, tenant, content, casMd5, additionMap}`                        | same                                                                                                            | `s.ST.UpsertConfig` + `s.Hub.Publish`               |
| `ConfigRemoveRequest`         | C → S     | Config    | `{dataId, group, tenant}`                                                     | same                                                                                                            | `s.ST.DeleteConfig` + `s.Hub.Invalidate`            |
| `ConfigBatchListenRequest`    | C → S     | Config    | `{configListenInfos:[{dataId,group,tenant,md5}], requestId, longPullingTimeoutMs}` | same                                                                                                            | `s.Hub.WaitChanged` → push `ConfigResponse`          |
| `ConfigResponse`              | S → C     | Config    | `{dataId, group, tenant, content, md5, type}`                                  | same                                                                                                            | built from `*storage.ConfigInfo`                      |
| `ConfigChangeNotifyResponse`  | S → C     | Config    | not used by v2                                                                | (3.0+) push variant — goacos treats it as `ConfigResponse` (v3 SDK accepts both)                                | alias → `ConfigResponse`                             |
| `HealthCheckRequest`          | C → S     | Internal  | `{}`                                                                          | `{}`                                                                                                            | echo `{}` + ack timestamp                            |
| `HealthCheckResponse`         | S → C     | Internal  | `{requestId}`                                                                 | `{requestId}`                                                                                                  | server-injected `requestId`                           |
| `ClientDetectionRequest`      | S → C     | Internal  | server pushes every 5s                                                        | server pushes every 5s                                                                                        | keepalive ping (optional Phase 5)                     |
| `ClientDetectionResponse`     | C → S     | Internal  | client responds                                                               | client responds                                                                                               | server records liveness                              |
| `ErrorResponse`               | S → C     | All       | `{resultCode, errorCode, message, requestId}`                                   | same                                                                                                            | any handler can emit                                 |

**v3-only fields** (rejected by v2 SDKs but accepted by `Metadata.type` lookup):

| field                        | location                                | goacos 行为                                                          |
| ---------------------------- | --------------------------------------- | ------------------------------------------------------------------- |
| `authSystemType`             | `ConnectionSetupRequest` body            | if non-zero → mark connection as v3 + require `Metadata.token`         |
| `tenant`                     | `ConnectionSetupRequest` body            | persist on connection record, applied as default namespace for all subsequent requests |
| `Metadata.headers["token"]` | every non-setup request                  | validate via `core.AuthService.Parse`; on fail → `ErrorResponse(401)` |
| `Metadata.headers["tenant"]`| every non-setup request                  | override the default namespace                                       |

---

## C. HTTP ↔ gRPC relationship

Both transports will run in the same `goacos` process. No port collision (HTTP=8848,
gRPC=9848 by default). No code-path fork: gRPC handlers will reuse the same `*storage.Store`
and `*core.ConfigHub` that HTTP handlers do.

| Domain   | HTTP endpoint                       | gRPC request type                | shared backing                                            |
| -------- | ----------------------------------- | -------------------------------- | --------------------------------------------------------- |
| Config   | `GET /nacos/v1/cs/configs`          | `ConfigQueryRequest`             | `s.ST.GetConfig`                                           |
| Config   | `POST /nacos/v1/cs/configs`         | `ConfigPublishRequest`           | `s.ST.UpsertConfig` + `s.Hub.Publish`                       |
| Config   | `DELETE /nacos/v1/cs/configs`       | `ConfigRemoveRequest`            | `s.ST.DeleteConfig` + `s.Hub.Invalidate`                    |
| Config   | `POST /nacos/v1/cs/configs/listener`| `ConfigBatchListenRequest`       | `s.Hub.WaitChanged` → push `ConfigResponse`                |
| Naming   | `POST /nacos/v1/ns/instance`        | `InstanceRequest{type:registerInstance}` | `s.ST.RegisterInstance`                              |
| Naming   | `GET /nacos/v1/ns/instance/list`    | `ServiceQueryRequest`            | `s.ST.ListInstances`                                       |
| Naming   | `GET /nacos/v1/ns/instance/beat`    | `InstanceRequest{type:updateInstance}` (beat is register update) | `s.ST.BeatInstance`                     |
| Naming   | `DELETE /nacos/v1/ns/instance`      | `InstanceRequest{type:deregisterInstance}` | `s.ST.DeleteInstance`                              |
| Naming   | `GET /nacos/v1/ns/service/list`     | `ServiceListRequest`             | `s.ST.ListServices`                                        |
| Naming   | push notifications                  | server-pushed `ServiceResponse`  | in-memory subscriber map (new) + `s.ST.ListInstances`       |
| Config   | push notifications                  | server-pushed `ConfigResponse`   | `core.S.Hub` (already exists)                              |
| Auth     | `POST /nacos/v1/auth/users/login`   | (v2) `ConnectionSetupRequest{username,password}` / (v3) `Metadata.token` | `core.AuthService.Login/Parse` |

**What gRPC will NOT cover in this round** (out of scope, deferred):

- Config CAS via `casMd5` (HTTP path is also permissive — tracked in roadmap)
- Cluster write forwarding (not requested; HTTP also lacks it)
- Beta / tag-config APIs (`config_info_tag` table exists; both transports route to
  same code path eventually)
- UDP push on 7878 (legacy 1.x; Nacos 2/3 SDKs do not use it)

---

## D. Out-of-scope (explicitly NOT implemented in v1.2.0-rc)

| Surface                                  | Reason                                            |
| ---------------------------------------- | ------------------------------------------------- |
| `BatchConfigQueryRequest`                | v3-only, can add post-rc                          |
| `ConfigPublishResponse` change-notify  | alias to `ConfigResponse`; no separate handling    |
| Connection-level TLS                     | deferred; Phase 6 follow-up                       |
| `ClientDetectionRequest` push loop       | Phase 5 only; heartbeat piggybacked on stream RPCs |
| `SetupAckRequest` / `SetupAckResponse`  | v3+2 deprecated alias; we treat it as `ConnectionSetupRequest/Response` |

---

## E. Acceptance gate for Phase 1 sign-off

Before I touch any code, **you** confirm:

1. ✅ The connection layer table (A) is correct against your understanding of
   nacos-sdk-go 2.x & 3.x wire behavior.
2. ✅ The request/response matrix (B) lists every type your target SDK actually
   sends. (If you only need register+query+heartbeat on naming and
   publish+query+listen on config, I can drop the rest from Phase 3.)
3. ✅ The HTTP↔gRPC relationship (C) doesn't miss a hot endpoint your services
   depend on.
4. ✅ Out-of-scope items (D) match your expectation — nothing here surprises you.

Once you ack, I begin Phase 2 (`GRPCEnabled`/`GRPCPort` config + `app.RunServer`
wiring + Dockerfile `9848` exposure).