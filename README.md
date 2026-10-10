# goacos

**A lightweight, Nacos-compatible configuration center and service discovery server — rewritten in Go, backed by MySQL or PostgreSQL.**

[中文文档](README_zh.md)

`goacos` reimplements the core of [Apache Nacos](https://github.com/alibaba/nacos) (latest stable line 3.2.x / LTS 2.5.x) as a single static Go binary. It speaks the Nacos HTTP Open API (v1 + v2), stores everything in **MySQL or PostgreSQL**, and deploys with one command — no JVM, no Derby, no Raft disks to babysit.

## Why

| | Nacos (Java) | goacos (Go) |
|---|---|---|
| Binary/image | ~1 GB image (JDK + server) | **~17 MB image, single ~15 MB static binary** |
| App memory (RSS) | ~700 MB – 1.2 GB (JVM) | **~11 MB measured** after a full API exercise |
| Startup | 20–40 s | **< 1 s** |
| Dependencies | JDK 17+, embedded Derby or MySQL | MySQL or PostgreSQL (one env switch) |
| Scaling model | Distro/JRaft gossip + snapshots | **Shared database = stateless replicas** (add nodes behind an LB) |
| Operations | JVM tuning, raft data dirs, console fat jar | one env-var-driven process |

Numbers measured locally: goacos RSS 10.9 MB after running the full e2e suite (config publish/listen, instance register/heartbeat); Nacos figures are the project's documented defaults (`-Xms512m` and up, plus metaspace/off-heap overhead).

## Features

- **Config center** — publish/get/delete, namespaces, groups, tags, change history + revert, **long-polling listeners** (v1 `\x02/\x01` protocol and v2 JSON), MD5 change detection with in-process hub (no DB polling storm).
- **Service discovery** — register/deregister ephemeral & persistent instances, clusters, weights, metadata, heartbeats with the Nacos 15 s-unhealthy / 30 s-removed lifecycle, instance & service queries (v1 `ServiceInfo` shape + v2 envelope).
- **Auth** — JWT access tokens (HS256, required goacos issuer/expiry), live enabled-user/role admission and admin-gated writes when `GOACOS_AUTH_ENABLED=true`. Fine-grained resource permission enforcement remains unimplemented.
- **Console** — embedded single-page UI at `/nacos/index.html`: services/instances, config editor, namespaces, users.
- **Dual-engine storage** — **MySQL** and **PostgreSQL** are both first-class. One embedded schema per engine (config tables mirror the official Nacos layout); a single dialect layer switches between them.
- **Database auto-discovery** — `goacos db discover` finds reachable database servers automatically: Docker containers first (reads their `MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD` from container env), then localhost (MySQL :3306, PostgreSQL :5432), optionally a subnet scan (`--cidr`). Restrict to one engine with `--db-type`.
- **Self-initializing** — on boot goacos creates the database if missing, applies the engine-specific schema, and seeds the admin user. Zero manual SQL.

## Quick start

### One command (dynamic DB discovery)

```bash
./deploy/deploy.sh
```

The deploy script discovers a reachable MySQL or PostgreSQL server — Docker containers first (reads their `MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD` from container env), then localhost, optionally a subnet scan (`--cidr 192.168.1.0/24`) — verifies credentials, creates the database, and starts the goacos container (linux/amd64 + linux/arm64 images). Override discovery:

```bash
./deploy/deploy.sh --host 10.0.0.5 --port-db 3306 --user root --password secret
./deploy/deploy.sh --db-type postgres --host 10.0.0.6 --port-db 5432 --user postgres --password secret
```

### Docker Compose (MySQL or PostgreSQL)

```bash
docker compose up -d                                  # MySQL stack
docker compose -f docker-compose.postgres.yml up -d   # PostgreSQL stack
# console: http://localhost:8848/nacos/index.html  (nacos / nacos)
```

### From source

```bash
make build && GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_USER=root GOACOS_MYSQL_PASSWORD=... ./goacos serve
```

## Configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `GOACOS_PORT` | `8848` | HTTP listen port |
| `GOACOS_DB_TYPE` | `mysql` | `mysql` or `postgres` — forced by deployment files |
| `GOACOS_DB_DSN` | — | full DSN override (`postgres://...` implies PostgreSQL) |
| `GOACOS_MYSQL_HOST` / `MYSQL_PORT` | `127.0.0.1` / `3306` | database endpoint (either engine) |
| `GOACOS_MYSQL_DB` | `goacos` | database (auto-created) |
| `GOACOS_MYSQL_USER` / `MYSQL_PASSWORD` | `root` / `` | credentials |
| `GOACOS_AUTH_ENABLED` | `false` | unsecured compatibility mode; enable for private deployments |
| `GOACOS_AUTH_TOKEN_SECRET` | empty | explicit private JWT key, at least 32 bytes; old public default rejected when auth enabled |
| `GOACOS_ADMIN_USERNAME` / `ADMIN_PASSWORD` | `nacos` / `nacos` | seed values; auth-enabled startup requires nonblank username and private password with 12+ characters/72 bytes max |
| `GOACOS_HEARTBEAT_TIMEOUT_MS` | `15000` | ephemeral instance → unhealthy |
| `GOACOS_EPHEMERAL_DELETE_AFTER_MS` | `30000` | ephemeral instance → removed |
| `GOACOS_SWEEP_INTERVAL_MS` | `5000` | instance cleanup sweep interval |

`NACOS_*` is accepted as an alias prefix.

Auth-enabled startup validates configuration before opening storage. Token TTL must be positive and at most 86400 seconds. Login tokens require HS256, issuer `goacos` and expiry; protected requests reload enabled users and current roles. Normal users cannot register, heartbeat, deregister, change namespaces/configuration or list the user directory. GET/HEAD heartbeat is a write; the two POST config-listener routes remain reads. Existing read routes have no namespace/service ACL enforcement, and revocation is checked at request admission rather than atomically with each business commit. Private deployment admission still requires those remaining gates. See the scoped [verification evidence](docs/evidence/w038-auth-20261011/README.md).

## API compatibility

Implemented against the Nacos **3.2.x / 2.5.x HTTP Open API** (`/nacos/v1/*` and `/nacos/v2/*`). Details and endpoint matrix: [docs/API.md](docs/API.md).

Works out of the box with: curl/scripts, [nacos-sdk-go](https://github.com/nacos-group/nacos-sdk-go) HTTP clients, Spring Cloud properties pulled via tooling, and any Nacos client that uses the REST API. **Not yet implemented:** the gRPC bi-stream channel (port 9848) used by Java/Go SDK 2.x for push — clients fall back to polling via the HTTP API; gRPC is on the roadmap (see below).

## Running multiple replicas

The database persists configuration, but `ConfigHub` loads its notification index once and updates it in process. Its 1s tick only rereads that in-memory index; cross-node writes do not reach another replica's listeners. Route all configuration reads/writes/listeners to one active instance until a real cross-replica consistency mechanism and failure tests are implemented. This routing fence is not yet enforced by the server; multiple replicas and HA are not accepted.

## Development

```bash
make test              # unit tests
E2E_DB=mysql bash scripts/e2e.sh      # end-to-end on MySQL 8 (28 checks)
E2E_DB=postgres bash scripts/e2e.sh   # end-to-end on PostgreSQL 16 (28 checks)
make image-multi       # build linux/amd64 + linux/arm64 images
```

Releases are built by [.github/workflows/release.yml](.github/workflows/release.yml): multi-arch image to GHCR plus binaries for linux (amd64/arm64) and macOS (amd64/arm64).

## Roadmap

- gRPC 2.x channel (port 9848) for SDK push subscriptions
- Config gray/beta release & tag-based APIs
- Prometheus metrics endpoint
- Cluster write forwarding (single active writer per namespace)

## License

[Apache-2.0](LICENSE). The embedded MySQL schema is derived from the Apache Nacos distribution schema (Apache-2.0) for interoperability — see [NOTICE](NOTICE). goacos contains no Nacos Java code.

## Related

- [alibaba/nacos](https://github.com/alibaba/nacos) — the original Java implementation
- [heqingpan/rnacos](https://github.com/heqingpan/rnacos) — an independent Rust reimplementation
