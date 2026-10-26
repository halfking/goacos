# goacos

**用 Go 重写的 Nacos 兼容配置中心 + 服务发现服务器，MySQL 或 PostgreSQL 存储，单二进制，极低资源占用。**

[English](README.md)

`goacos` 将 [Apache Nacos](https://github.com/alibaba/nacos)(最新稳定线 3.2.x / LTS 2.5.x)的核心能力用 Go 重新实现为一个静态编译的单二进制。它兼容 Nacos HTTP Open API(v1 + v2),所有数据存 **MySQL 或 PostgreSQL**,一条命令即可部署——没有 JVM、没有 Derby、没有 Raft 磁盘要维护。

## 为什么

| | Nacos (Java) | goacos (Go) |
|---|---|---|
| 镜像体积 | ~1 GB(JDK + server) | **~17 MB 镜像,~15 MB 静态二进制** |
| 应用内存(RSS) | ~700 MB – 1.2 GB(JVM) | 完整 API 压测后实测 **~11 MB** |
| 启动时间 | 20–40 秒 | **< 1 秒** |
| 依赖 | JDK 17+,内嵌 Derby 或 MySQL | MySQL 或 PostgreSQL(`GOACOS_DB_TYPE` 切换) |
| 扩展模型 | Distro/JRaft gossip + 快照 | **共享数据库 = 无状态多副本**(LB 后直接加节点) |
| 运维 | JVM 调优、raft 数据目录、console fat jar | 一个环境变量驱动的进程 |

本机实测:goacos 跑完整个 e2e 场景(配置发布/监听、实例注册/心跳)后 RSS 10.9 MB;Nacos 数据取自项目默认 JVM 参数(-Xms512m 起步,另有 metaspace/堆外开销)。

## 功能

- **配置中心** — 发布/查询/删除、命名空间、分组、标签、变更历史+回滚、**长轮询监听**(v1 `\x02/\x01` 协议与 v2 JSON)、MD5 变更检测(进程内 hub,不冲击数据库)。
- **服务发现** — 临时/持久实例注册注销、集群、权重、元数据、心跳(Nacos 同款 15 秒不健康 / 30 秒摘除生命周期)、实例与服务查询(v1 `ServiceInfo` 形状 + v2 信封)。
- **认证** — JWT access token(HS256,要求 issuer 为 `goacos` 且带过期时间)、用户/角色、管理接口仅限管理员、可选开启(`GOACOS_AUTH_ENABLED=true`);按资源的细粒度权限尚未实现。
- **控制台** — 内嵌单页 UI(`/nacos/index.html`):服务/实例、配置编辑器、命名空间、用户。
- **双引擎存储** — **MySQL** 与 **PostgreSQL** 均为一等公民。每个引擎各内嵌一份 schema(配置表与 Nacos 官方布局一致);统一方言层自动切换。
- **数据库自动发现** — `goacos db discover` 自动查找可达的数据库服务器:优先 Docker 容器(直接读取容器 env 中的 `MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD`),其次 localhost(MySQL :3306、PostgreSQL :5432),可选子网扫描(`--cidr`);`--db-type` 可限定引擎。
- **自初始化** — 启动时自动建库、按引擎应用 schema、播种管理员账号。零手工 SQL。

## 快速开始

### 一条命令(动态数据库发现)

```bash
./deploy/deploy.sh
```

部署脚本会自动发现可达的 MySQL 或 PostgreSQL——优先 Docker 容器(直接读取容器 env 里的 `MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD`),其次 localhost,可选子网扫描(`--cidr 192.168.1.0/24`)——校验凭据、建库,然后启动 goacos 容器(linux/amd64 + linux/arm64 双架构镜像)。也可以显式指定:

```bash
./deploy/deploy.sh --host 10.0.0.5 --port-db 3306 --user root --password secret
./deploy/deploy.sh --db-type postgres --host 10.0.0.6 --port-db 5432 --user postgres --password secret
./deploy/deploy.sh --auth --admin-pass '<12+ characters>' --token-secret '<32+ byte private key>' --db prod_goacos --force
```

### Docker Compose(自带 MySQL 或 PostgreSQL)

```bash
docker compose up -d                                  # MySQL 栈
docker compose -f docker-compose.postgres.yml up -d   # PostgreSQL 栈
# 控制台: http://localhost:8848/nacos/index.html  (nacos / nacos)
```

### 源码运行

```bash
make build && GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_USER=root GOACOS_MYSQL_PASSWORD=... ./goacos serve
```

## 配置(环境变量)

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GOACOS_HOST` | `0.0.0.0` | HTTP 监听地址(端口取自 `GOACOS_PORT`) |
| `GOACOS_PORT` | `8848` | HTTP 监听端口 |
| `GOACOS_DB_TYPE` | `mysql` | `mysql` 或 `postgres`——部署文件强制指定入口 |
| `GOACOS_DB_DSN` | — | 完整 DSN 覆盖(`postgres://...` 前缀自动识别为 PostgreSQL) |
| `GOACOS_MYSQL_HOST` / `MYSQL_PORT` | `127.0.0.1` / `3306` | 数据库地址(双引擎通用) |
| `GOACOS_MYSQL_DB` | `goacos` | 数据库(不存在则自动创建) |
| `GOACOS_MYSQL_USER` / `MYSQL_PASSWORD` | `root` / `` | 凭据 |
| `GOACOS_AUTH_ENABLED` | `false` | 是否强制 access token |
| `GOACOS_AUTH_TOKEN_SECRET` | (空) | JWT 签名密钥;开启认证时必须显式提供至少 32 字节的私有密钥,内置的旧公开默认值会被拒绝 |
| `GOACOS_AUTH_TOKEN_TTL_SECONDS` | `18000` | 访问令牌有效期(秒);必须为正数且不超过 `86400` |
| `GOACOS_ADMIN_USERNAME` / `ADMIN_PASSWORD` | `nacos` / `nacos` | 播种管理员(仅 users 表为空时) |
| `GOACOS_HEARTBEAT_INTERVAL_MS` | `5000` | 对客户端下发的心跳间隔 |
| `GOACOS_HEARTBEAT_TIMEOUT_MS` | `15000` | 临时实例转不健康阈值 |
| `GOACOS_EPHEMERAL_DELETE_AFTER_MS` | `30000` | 临时实例删除阈值 |
| `GOACOS_PERSISTENT_HEARTBEAT_TIMEOUT_MS` | `90000` | 持久实例转不健康阈值(`0` 表示关闭该检查) |
| `GOACOS_SWEEP_INTERVAL_MS` | `5000` | 实例清理扫描间隔 |
| `GOACOS_MYSQL_MAX_OPEN_CONNS` | `100` | 数据库连接池最大连接数 |
| `GOACOS_MYSQL_MAX_IDLE_CONNS` | `20` | 数据库连接池最大空闲连接数 |
| `GOACOS_MYSQL_CONN_MAX_LIFETIME_SECONDS` | `1800` | 数据库连接最长存活时间(秒) |
| `GOACOS_LOG_LEVEL` | `info` | 日志级别(已读入配置,尚未被日志初始化消费) |

`NACOS_*` 前缀同样有效。

## API 兼容性

对标 Nacos **3.2.x / 2.5.x HTTP Open API**(`/nacos/v1/*` 与 `/nacos/v2/*`),端点矩阵见 [docs/API.md](docs/API.md)。

开箱即用:curl/脚本、[nacos-sdk-go](https://github.com/nacos-group/nacos-sdk-go) HTTP 客户端、各类通过 REST API 访问 Nacos 的工具链。**尚未实现:** Java/Go SDK 2.x 使用的 gRPC 双向流通道(9848 端口)——这类客户端可通过 HTTP API 轮询工作;gRPC 在路线图中。

## 网关

一个二进制,两个面。开启 `GOACOS_GATEWAY_ENABLED=true` 后,同一进程还会对外提供 HTTP/HTTPS 入站流量:每条声明过的 `domain` + `pathPrefix` 路由,由该组件对应命名服务里已注册的健康实例承接。不需要 nginx,也不需要手工维护 `域名 → host:port` 表——策略是数据,地址是实时实例状态。

**默认关闭。** `GOACOS_GATEWAY_ENABLED` 默认 `false`,这样把 goacos 放进已有环境时,不会悄悄开始和现有服务抢占 `:80` / `:443`。请显式开启。

### 最小可运行示例

按 应用 → 组件 → 路由 的顺序声明,再注册实例:

```bash
export GOACOS_GATEWAY_ENABLED=true

# 1. 应用
curl -sX POST localhost:8848/goacos/v1/apps -d name=shop -d displayName=Shop

# 2. 组件 —— 路由将要指向的命名服务
curl -sX POST localhost:8848/goacos/v1/apps/shop/components \
  -d name=api -d kind=api -d protocol=http -d serviceName=shop-api

# 3. 路由 —— 域名 + 路径前缀 -> 应用 + 组件
curl -sX POST localhost:8848/goacos/v1/routes \
  -d domain=app.example.com -d pathPrefix=/api -d appName=shop -d componentName=api

# 4. 实例(就是标准的 Nacos v1 注册接口,无需改动)
curl -sX POST localhost:8848/nacos/v1/ns/instance \
  -d ip=10.0.0.7 -d port=8080 -d serviceName=shop-api -d ephemeral=true

# 5. 立即生效,而不必等完刷新间隔
curl -sX POST localhost:8848/goacos/v1/gateway/refresh
```

此后 `https://app.example.com/api` 会被反向代理到 `http://10.0.0.7:8080`。

### 网关环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GOACOS_GATEWAY_ENABLED` | `false` | 是否启动网关数据面 |
| `GOACOS_GATEWAY_NAMESPACE` | `public` | 本实例服务哪个命名空间的路由 |
| `GOACOS_GATEWAY_HTTP_ADDR` | `:80` | 明文 HTTP 监听地址(ACME 质询 + 跳转) |
| `GOACOS_GATEWAY_HTTPS_ADDR` | `:443` | 承载真实流量的 TLS 监听地址 |
| `GOACOS_GATEWAY_FORCE_HTTPS` | `true` | `:80` 上做跳转而不是代理明文 |
| `GOACOS_GATEWAY_HSTS_MAX_AGE` | `0` | 默认 `Strict-Transport-Security` max-age(`0` 表示不下发) |
| `GOACOS_GATEWAY_MAX_BODY_BYTES` | `0` | 请求体上限(`0` 表示不限制) |
| `GOACOS_GATEWAY_CONNECT_TIMEOUT_MS` | `3000` | 上游连接超时 |
| `GOACOS_GATEWAY_READ_TIMEOUT_MS` | `60000` | 上游读超时 |
| `GOACOS_GATEWAY_SEND_TIMEOUT_MS` | `60000` | 上游写超时 |
| `GOACOS_GATEWAY_WEBSOCKET` | `true` | 是否允许连接升级 |
| `GOACOS_GATEWAY_TRUST_FORWARDED_FOR` | `false` | 是否采信 `X-Forwarded-For` 判定客户端 IP |
| `GOACOS_GATEWAY_STATIC_ROOT` | — | `protocol=static` 组件的根目录(为空则关闭静态托管) |
| `GOACOS_GATEWAY_STATIC_FALLBACK` | `true` | 无扩展名且未命中的路径回落到 `index.html` |
| `GOACOS_GATEWAY_SECURITY_HEADERS` | — | 逐行下发的安全响应头，每行 `名称: 值`；无冒号的行被丢弃 |
| `GOACOS_GATEWAY_BLOCK_USER_AGENTS` | — | 逐行 UA 正则；命中即在进路由前 403 |
| `GOACOS_GATEWAY_BLOCK_PATHS` | — | 逐行路径正则；命中即在进路由前 404 |
| `GOACOS_GATEWAY_REFRESH_SECONDS` | `10` | 路由与证书从数据库重新读取的间隔 |
| `GOACOS_GATEWAY_ACME_ENABLED` | `true` | 自动签发证书 |
| `GOACOS_GATEWAY_ACME_EMAIL` | — | ACME 账户邮箱 |
| `GOACOS_GATEWAY_ACME_DIRECTORY_URL` | `https://acme-v02.api.letsencrypt.org/directory` | ACME directory;指向 staging 可在不消耗生产额度的情况下跑通签发 |
| `GOACOS_GATEWAY_ACME_DATA_DIR` | `./data/acme` | ACME 账户私钥——请备份;丢失即丢失账户及其全部速率额度 |
| `GOACOS_GATEWAY_ACME_TIMEOUT_SECONDS` | `120` | ACME 请求超时 |
| `GOACOS_GATEWAY_SELFSIGNED_DIR` | `./data/selftest` | 自签证书兜底缓存目录 |
| `GOACOS_GATEWAY_CERT_DIR` | *(空)* | 共享证书目录，挂载后自动加载 |
| `GOACOS_GATEWAY_CERT_WRITE_BACK` | `true` | 把签发结果回写到该目录 |
| `GOACOS_GATEWAY_CERT_SCAN_SECONDS` | `30` | 目录重扫间隔 |

### 从共享目录加载证书

设置 `GOACOS_GATEWAY_CERT_DIR` 指向一个挂载目录，goacos 就会直接对外提供其中的证书，
无需把它们复制进数据库。三种目录结构都能识别，因为现实中三种都存在：

```
certs/example.com/fullchain.pem + privkey.pem   # certbot live 结构
certs/example.com.crt          + example.com.key # 平铺成对
certs/example.com.pem                           # 单文件合并
```

通配符同样支持：`certs/*.example.com.crt` 会服务 `api.example.com`，但不会服务
`a.b.example.com` —— 这与 CA 实际签发的范围一致。

该目录**不会在 TLS 握手时被读取**。它按定时器扫描、解析一次，然后与数据库来源共用
同一份内存缓存 —— 所以宿主机繁忙或挂载卷变慢，不会演变成一场握手失败风暴。

当同一域名存在多份证书时，由两条规则决定用哪一份：

- 外部 CA 签发的证书优先于自签占位证书。目录里如果本身是自签的，会被识别为占位证书，
  不会顶掉一张真实证书。
- 手动上传的证书不被任何来源覆盖。

goacos 签发的证书会**回写**到同一目录（`GOACOS_GATEWAY_CERT_WRITE_BACK`），
便于 nginx、其他网关或脚本复用。写入是原子的（临时文件 + rename），
并发扫描的读取方不会读到被截断的证书。只读挂载时把该开关设为 `false`。

Docker 中：

```bash
docker run \
  -v /etc/goacos/certs:/etc/goacos/certs \
  -e GOACOS_GATEWAY_CERT_DIR=/etc/goacos/certs \
  ...
```

容器以 **uid 10001** 运行。宿主机目录必须对该 uid 可读（开启回写时还需可写）——
root 属主的 `0700` 目录会导致一张证书都加载不出来，且没有任何明显提示。

### 证书

- **默认走 ACME HTTP-01。** 已声明但尚无证书的域名会被自动签发。HTTP-01 不需要 DNS 服务商凭据,只要求网关能在 80 端口应答——这正是保留 `:80` 监听的意义所在。
- **自签兜底保证已声明的域名始终能应答。** 域名一旦被声明,就会立刻用本地生成的证书对外服务,该记录标记为 `self_signed`,并由 `/goacos/v1/gateway/status` 如实上报;CA 签发成功后即被真实证书替换。握手被拒在运维看来就是一个"站点挂了",而真正的故障(证书还没签下来)在那一层是完全不可见的。
- **手工上传** —— `POST /goacos/v1/certs`,提交 `domain`、`certPem`、`keyPem`,适用于隔离网络部署与内部 CA。**手工证书永不自动续期**:进程内没有任何东西知道该拿什么去续,所以必须在到期前上传替换件。

### 前端静态托管

把组件声明为 `protocol=static`,并设置 `GOACOS_GATEWAY_STATIC_ROOT`。每个组件的文件放在 `<static-root>/<app>-<component>/` 目录下。

- 无扩展名且磁盘上不存在的路径返回 `index.html`(`GOACOS_GATEWAY_STATIC_FALLBACK=true`),因此单页应用可以直接深链自己的前端路由
- 带内容指纹的构建产物(`app.4f3a2b1c.js`、`main-C1x9zPq2.css`)按 `immutable` 缓存一年;`index.html` 则永不缓存
- 目录不存在时该路由自动回落到反向代理——因此组件可以在"静态托管"与"注册实例"之间切换,无需重新声明路由

### 两个监听端口

`:80` 负责应答 ACME HTTP-01 质询,并在 `GOACOS_GATEWAY_FORCE_HTTPS=true` 时把其余请求跳转到 HTTPS;`:443` 承载真实流量。两个监听地址都可配置。

上述全部配置项的控制面(状态、证书、按域策略、应用、组件、路由)详见 [docs/API.md](docs/API.md)。

为什么网关放在本项目、以及**刻意不做**什么(限流、服务间 mTLS、WAF、CDN),见 [docs/GATEWAY.md](docs/GATEWAY.md)。

## 多副本

所配数据库(MySQL 或 PostgreSQL,`GOACOS_DB_TYPE`)即事实源,但配置服务是单活动(single-active)的:启动时会在一条独立连接上取 PostgreSQL 会话级 advisory 锁或 MySQL 库级命名锁,第二个实例因此拒绝启动。共享连接池至少需要两条连接(一条被所有权占用,认证与命名检查还要用池)。配置读写、标签、历史与命名空间配置计数都走这条被占用连接上的事务,提交前再次校验所有权;成功响应与本地通知在提交之后才发出。长轮询在两次短事务之间释放连接,靠本地通知与周期检查唤醒,返回前再做一次带围栏的校验。连接或所有权一旦丢失,该进程的配置服务永久关闭(HTTP 503),不会改走池里的另一条连接重试;需要重启一个全新进程去竞争所有权。认证与命名沿用各自的连接池与权限。这是单一数据库上的单活动约束,不包含请求转发、自动故障切换或跨副本 HA 声明。详见[有界的 PG/MySQL 证据](docs/evidence/w038-single-active-20261011/README.md)。

## 开发

```bash
make test                            # 单元测试
E2E_DB=mysql bash scripts/e2e.sh     # MySQL 8 全链路验收(75 项)
E2E_DB=postgres bash scripts/e2e.sh  # PostgreSQL 16 全链路验收(75 项)
make image-multi                     # 构建 linux/amd64 + linux/arm64 镜像
```

发布由 [.github/workflows/release.yml](.github/workflows/release.yml) 自动完成:双架构镜像推 GHCR,同时产出 linux(amd64/arm64)与 macOS(amd64/arm64)二进制。

## 路线图

- gRPC 2.x 通道(9848 端口),支持 SDK 推送订阅
- 配置灰度/beta 发布与标签 API
- Prometheus 指标端点
- 集群写转发(每命名空间单写者)

## 许可证

[Apache-2.0](LICENSE)。内嵌 MySQL schema 派生自 Apache Nacos 发行版 schema(Apache-2.0),仅为保持表结构互操作——见 [NOTICE](NOTICE)。goacos 不包含任何 Nacos Java 代码。

## 相关项目

- [alibaba/nacos](https://github.com/alibaba/nacos) — 原始 Java 实现
- [heqingpan/rnacos](https://github.com/heqingpan/rnacos) — 独立的 Rust 重实现
