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

- **Config center** — publish/get/delete, namespaces, groups, tags, change history + revert, **long-polling listeners** (v1 `\x02/\x01` protocol and v2 JSON), MD5 change detection from owned database transactions with local notifications and periodic listener checks.
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
./deploy/deploy.sh --auth --admin-pass '<12+ characters>' --token-secret '<32+ byte private key>' --db prod_goacos --force
```

`--auth` makes the server require an explicit private JWT key and a private seeded
admin password; the script checks the same contract the server does and refuses to
start the container with a readable message instead of letting it crash-loop.

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
| `GOACOS_HOST` | `0.0.0.0` | HTTP listen host (the port comes from `GOACOS_PORT`) |
| `GOACOS_PORT` | `8848` | HTTP listen port |
| `GOACOS_DB_TYPE` | `mysql` | `mysql` or `postgres` — forced by deployment files |
| `GOACOS_DB_DSN` | — | full DSN override (`postgres://...` implies PostgreSQL) |
| `GOACOS_MYSQL_HOST` / `MYSQL_PORT` | `127.0.0.1` / `3306` | database endpoint (either engine) |
| `GOACOS_MYSQL_DB` | `goacos` | database (auto-created) |
| `GOACOS_MYSQL_USER` / `MYSQL_PASSWORD` | `root` / `` | credentials |
| `GOACOS_AUTH_ENABLED` | `false` | unsecured compatibility mode; enable for private deployments |
| `GOACOS_AUTH_TOKEN_SECRET` | empty | explicit private JWT key, at least 32 bytes; old public default rejected when auth enabled |
| `GOACOS_AUTH_TOKEN_TTL_SECONDS` | `18000` | access-token lifetime; must be positive and at most `86400` |
| `GOACOS_ADMIN_USERNAME` / `ADMIN_PASSWORD` | `nacos` / `nacos` | seed values; auth-enabled startup requires nonblank username and private password with 12+ characters/72 bytes max |
| `GOACOS_HEARTBEAT_INTERVAL_MS` | `5000` | heartbeat interval advertised to clients |
| `GOACOS_HEARTBEAT_TIMEOUT_MS` | `15000` | ephemeral instance → unhealthy |
| `GOACOS_EPHEMERAL_DELETE_AFTER_MS` | `30000` | ephemeral instance → removed |
| `GOACOS_PERSISTENT_HEARTBEAT_TIMEOUT_MS` | `90000` | persistent instance → unhealthy (`0` disables the check) |
| `GOACOS_SWEEP_INTERVAL_MS` | `5000` | instance cleanup sweep interval |
| `GOACOS_MYSQL_MAX_OPEN_CONNS` | `100` | database pool max open connections |
| `GOACOS_MYSQL_MAX_IDLE_CONNS` | `20` | database pool max idle connections |
| `GOACOS_MYSQL_CONN_MAX_LIFETIME_SECONDS` | `1800` | database connection max lifetime |
| `GOACOS_LOG_LEVEL` | `info` | log level (read into the config; not consumed by the logger yet) |

`NACOS_*` is accepted as an alias prefix.

Auth-enabled startup validates configuration before opening storage, then refuses any enabled administrator still using the old public seed password or a malformed hash. Existing hashes are never replaced from environment values; unsafe persisted credentials require an explicitly authorized rotation. The 12-character/72-byte/unpadded password contract also gates login and account create/update. Token TTL must be positive and at most 86400 seconds. Login tokens require HS256, issuer `goacos` and expiry; protected requests reload enabled users and current roles. Normal users cannot register, heartbeat, deregister, change namespaces/configuration or list the user directory. GET/HEAD heartbeat is a write; the two POST config-listener routes remain reads. Existing read routes have no namespace/service ACL enforcement, and revocation is checked at request admission rather than atomically with each business commit. Private deployment admission still requires those remaining gates. See the scoped [verification evidence](docs/evidence/w038-auth-20261011/README.md).

## API compatibility

Implemented against the Nacos **3.2.x / 2.5.x HTTP Open API** (`/nacos/v1/*` and `/nacos/v2/*`). Details and endpoint matrix: [docs/API.md](docs/API.md).

Works out of the box with: curl/scripts, [nacos-sdk-go](https://github.com/nacos-group/nacos-sdk-go) HTTP clients, Spring Cloud properties pulled via tooling, and any Nacos client that uses the REST API. **Not yet implemented:** the gRPC bi-stream channel (port 9848) used by Java/Go SDK 2.x for push — clients fall back to polling via the HTTP API; gRPC is on the roadmap (see below).

## Gateway

One binary, two surfaces. With `GOACOS_GATEWAY_ENABLED=true` the same process also serves inbound HTTP/HTTPS traffic: every declared `domain` + `pathPrefix` route is served by the healthy instances registered for that component's naming service. No nginx, no hand-maintained `domain → host:port` table — the policy is data and the addresses are live instance state.

**Off by default.** `GOACOS_GATEWAY_ENABLED` defaults to `false` so that dropping goacos into an existing environment does not silently start competing for `:80` / `:443` with whatever already serves those ports. Turn it on deliberately.

### Minimal working example

Declare an app → a component → a route, then register an instance:

```bash
export GOACOS_GATEWAY_ENABLED=true

# 1. the application
curl -sX POST localhost:8848/goacos/v1/apps -d name=shop -d displayName=Shop

# 2. the component — a naming service the route will point at
curl -sX POST localhost:8848/goacos/v1/apps/shop/components \
  -d name=api -d kind=api -d protocol=http -d serviceName=shop-api

# 3. the route — domain + path prefix -> app + component
curl -sX POST localhost:8848/goacos/v1/routes \
  -d domain=app.example.com -d pathPrefix=/api -d appName=shop -d componentName=api

# 4. an instance (the ordinary Nacos v1 register API — unchanged)
curl -sX POST localhost:8848/nacos/v1/ns/instance \
  -d ip=10.0.0.7 -d port=8080 -d serviceName=shop-api -d ephemeral=true

# 5. apply it now instead of waiting out the refresh interval
curl -sX POST localhost:8848/goacos/v1/gateway/refresh
```

`https://app.example.com/api` is now proxied to `http://10.0.0.7:8080`.

### Gateway environment variables

| Variable | Default | Meaning |
|---|---|---|
| `GOACOS_GATEWAY_ENABLED` | `false` | start the gateway data plane |
| `GOACOS_GATEWAY_NAMESPACE` | `public` | which namespace's routes this instance serves |
| `GOACOS_GATEWAY_HTTP_ADDR` | `:80` | plain-HTTP listener (ACME challenges + redirect) |
| `GOACOS_GATEWAY_HTTPS_ADDR` | `:443` | TLS listener for real traffic |
| `GOACOS_GATEWAY_FORCE_HTTPS` | `true` | redirect on `:80` instead of proxying plaintext |
| `GOACOS_GATEWAY_HSTS_MAX_AGE` | `0` | default `Strict-Transport-Security` max-age (`0` = off) |
| `GOACOS_GATEWAY_MAX_BODY_BYTES` | `0` | request body cap (`0` = unlimited) |
| `GOACOS_GATEWAY_CONNECT_TIMEOUT_MS` | `3000` | upstream connect timeout |
| `GOACOS_GATEWAY_READ_TIMEOUT_MS` | `60000` | upstream read timeout |
| `GOACOS_GATEWAY_SEND_TIMEOUT_MS` | `60000` | upstream send timeout |
| `GOACOS_GATEWAY_WEBSOCKET` | `true` | allow connection upgrades |
| `GOACOS_GATEWAY_TRUST_FORWARDED_FOR` | `false` | honour `X-Forwarded-For` for client-IP rules |
| `GOACOS_GATEWAY_STATIC_ROOT` | — | root directory for `protocol=static` components (empty = off) |
| `GOACOS_GATEWAY_STATIC_FALLBACK` | `true` | serve `index.html` for unmatched extension-less paths |
| `GOACOS_GATEWAY_SECURITY_HEADERS` | — | security response headers to emit, one `Name: Value` per line; a line without a colon is dropped |
| `GOACOS_GATEWAY_BLOCK_USER_AGENTS` | — | user-agent regular expressions, one per line; a match is rejected with 403 before routing |
| `GOACOS_GATEWAY_BLOCK_PATHS` | — | path regular expressions, one per line; a match is rejected with 404 before routing |
| `GOACOS_GATEWAY_REFRESH_SECONDS` | `10` | how often routes + certificates are re-read from the database |
| `GOACOS_GATEWAY_ACME_ENABLED` | `true` | automatic certificate issuance |
| `GOACOS_GATEWAY_ACME_EMAIL` | — | ACME account email |
| `GOACOS_GATEWAY_ACME_DIRECTORY_URL` | `https://acme-v02.api.letsencrypt.org/directory` | ACME directory; point at staging to exercise issuance without spending production rate limits |
| `GOACOS_GATEWAY_ACME_DATA_DIR` | `./data/acme` | ACME account key — back it up; losing it loses the account and every rate limit attached to it |
| `GOACOS_GATEWAY_ACME_TIMEOUT_SECONDS` | `120` | ACME request timeout |
| `GOACOS_GATEWAY_SELFSIGNED_DIR` | `./data/selftest` | self-signed fallback certificate cache |
| `GOACOS_GATEWAY_CERT_DIR` | *(empty)* | shared directory of certificates to load |
| `GOACOS_GATEWAY_CERT_WRITE_BACK` | `true` | mirror issued certificates into that directory |
| `GOACOS_GATEWAY_CERT_SCAN_SECONDS` | `30` | how often the directory is rescanned |

### Serving certificates from a shared directory

Set `GOACOS_GATEWAY_CERT_DIR` to a mounted directory and goacos serves whatever
certificates it finds there — no need to copy them into the database. Three
layouts are recognised, because all three exist in the wild:

```
certs/example.com/fullchain.pem + privkey.pem   # certbot "live" layout
certs/example.com.crt          + example.com.key # flat pair
certs/example.com.pem                           # combined
```

Wildcards work: `certs/*.example.com.crt` is served for `api.example.com` but
not for `a.b.example.com`, matching what CAs actually issue.

The directory is **not** read during a TLS handshake. It is scanned on a timer,
parsed once, and held in the same in-memory cache the database feeds — so a
busy host or a slow volume cannot turn into a handshake failure storm.

Two rules decide which certificate wins when more than one exists:

- A certificate from an external CA beats a self-signed placeholder. A directory
  pair that is itself self-signed is recognised as a placeholder and does not
  get to displace a genuine certificate.
- A manually uploaded certificate is never overridden by either.

Certificates goacos issues are written back into the same directory
(`GOACOS_GATEWAY_CERT_WRITE_BACK`), so nginx, other gateways and scripts can
reuse them. The write is atomic — a temporary file plus a rename — so a reader
scanning mid-write never sees a truncated certificate. Set
`GOACOS_GATEWAY_CERT_WRITE_BACK=false` for a read-only mount.

In Docker:

```bash
docker run \
  -v /etc/goacos/certs:/etc/goacos/certs \
  -e GOACOS_GATEWAY_CERT_DIR=/etc/goacos/certs \
  ...
```

The container runs as **uid 10001**. The host directory must be readable (and
writable, with write-back on) by that uid — a root-owned `0700` directory loads
nothing and gives no obvious hint why.

### Certificates

- **ACME HTTP-01 is the default path.** A declared domain with no certificate is issued for automatically. HTTP-01 needs no DNS provider credentials and only requires that the gateway answers on port 80 — which is why `:80` is a listener at all.
- **A self-signed fallback always keeps a declared domain answering.** The moment a domain is declared it serves a locally generated certificate, recorded as `self_signed` and reported as such by `/goacos/v1/gateway/status`, and it is replaced by the real certificate as soon as the CA answers. A refused handshake presents to an operator as a dead site, and the real fault — no certificate yet — is invisible at that layer.
- **Manual upload** — `POST /goacos/v1/certs` with `domain`, `certPem`, `keyPem`, for air-gapped deployments and internal CAs. **Manual certificates are never auto-renewed**: nothing in the process knows what to renew them with, so upload a replacement before expiry.

### Static frontend delivery

Declare a component with `protocol=static` and set `GOACOS_GATEWAY_STATIC_ROOT`. Each component's files live in `<static-root>/<app>-<component>/`.

- extension-less paths that are not on disk get `index.html` (`GOACOS_GATEWAY_STATIC_FALLBACK=true`), so a single-page app can deep-link its own routes
- fingerprinted build assets (`app.4f3a2b1c.js`, `main-C1x9zPq2.css`) are served `immutable` for a year, while `index.html` is never cached
- if the directory is missing, the route falls back to proxying — so a component can switch between static delivery and a registered instance without redeclaring the route

### Two listeners

`:80` answers ACME HTTP-01 challenges and, with `GOACOS_GATEWAY_FORCE_HTTPS=true`, redirects everything else to HTTPS. `:443` carries the real traffic. Both addresses are configurable.

The control plane for all of this — status, certificates, per-domain policy, apps, components, routes — is documented in [docs/API.md](docs/API.md).

Why the gateway lives here, and what deliberately does **not** — rate limiting, service-mesh mTLS, WAF, CDN — is argued in [docs/GATEWAY.md](docs/GATEWAY.md).

## Running multiple replicas

Configuration serving requires one database-active instance. Startup acquires a PostgreSQL session advisory lock or a MySQL database-scoped named lock on a dedicated connection; a second instance refuses startup. The shared pool must allow at least two connections (or be unlimited), since ownership reserves one while fresh auth/naming checks use the pool. Configuration reads, writes, tags, history, namespace config counts and deletion checks use transactions on that actual connection, with ownership checked before commit. Successful HTTP responses and local notifications follow commit. MySQL requires the existing config/history/tag/namespace tables to use InnoDB; legacy nontransactional tables fail closed without conversion.

Long listeners release the connection between short database checks, use local notifications to wake quickly, and recheck ownership before returning. Connection or ownership loss permanently disables configuration serving in that process (HTTP 503); it never retries a write through another pooled connection. Restart a fresh process to compete for ownership and preload committed state. Auth and naming retain their existing pool and authority. This is a single-active constraint on one authoritative database, with no request forwarding, automatic failover or cross-replica HA claim. See [the bounded PG/MySQL evidence](docs/evidence/w038-single-active-20261011/README.md).

## Development

```bash
make test              # unit tests
E2E_DB=mysql bash scripts/e2e.sh      # end-to-end on MySQL 8 (75 checks)
E2E_DB=postgres bash scripts/e2e.sh   # end-to-end on PostgreSQL 16 (75 checks)
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
