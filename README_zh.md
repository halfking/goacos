# goacos

**用 Go 重写的 Nacos 兼容配置中心 + 服务发现服务器，MySQL 存储，单二进制，极低资源占用。**

[English](README.md)

`goacos` 将 [Apache Nacos](https://github.com/alibaba/nacos)(最新稳定线 3.2.x / LTS 2.5.x)的核心能力用 Go 重新实现为一个静态编译的单二进制。它兼容 Nacos HTTP Open API(v1 + v2),所有数据存 MySQL,一条命令即可部署——没有 JVM、没有 Derby、没有 Raft 磁盘要维护。

## 为什么

| | Nacos (Java) | goacos (Go) |
|---|---|---|
| 镜像体积 | ~1 GB(JDK + server) | **~17 MB 镜像,~15 MB 静态二进制** |
| 应用内存(RSS) | ~700 MB – 1.2 GB(JVM) | 完整 API 压测后实测 **~11 MB** |
| 启动时间 | 20–40 秒 | **< 1 秒** |
| 依赖 | JDK 17+,内嵌 Derby 或 MySQL | 仅 MySQL |
| 扩展模型 | Distro/JRaft gossip + 快照 | **共享 MySQL = 无状态多副本**(LB 后直接加节点) |
| 运维 | JVM 调优、raft 数据目录、console fat jar | 一个环境变量驱动的进程 |

本机实测:goacos 跑完整个 e2e 场景(配置发布/监听、实例注册/心跳)后 RSS 10.9 MB;Nacos 数据取自项目默认 JVM 参数(-Xms512m 起步,另有 metaspace/堆外开销)。

## 功能

- **配置中心** — 发布/查询/删除、命名空间、分组、标签、变更历史+回滚、**长轮询监听**(v1 `\x02/\x01` 协议与 v2 JSON)、MD5 变更检测(进程内 hub,不冲击数据库)。
- **服务发现** — 临时/持久实例注册注销、集群、权重、元数据、心跳(Nacos 同款 15 秒不健康 / 30 秒摘除生命周期)、实例与服务查询(v1 `ServiceInfo` 形状 + v2 信封)。
- **认证** — JWT access token(HS256,兼容 Nacos 登录端点)、用户/角色/权限、管理接口仅限管理员、可选开启(`GOACOS_AUTH_ENABLED=true`)。
- **控制台** — 内嵌单页 UI(`/nacos/index.html`):服务/实例、配置编辑器、命名空间、用户。
- **MySQL 原生** — 数据库是唯一事实源。配置表与 Nacos 官方 schema 一致(可互操作);naming 表为 goacos 自有设计(Nacos 的注册表在内存里,goacos 持久化——**重启零丢失**)。
- **自初始化** — 启动时自动建库、应用 schema、播种管理员账号。零手工 SQL。

## 快速开始

### 一条命令(动态数据库发现)

```bash
./deploy/deploy.sh
```

部署脚本会自动发现可达的 MySQL——优先 Docker 容器(直接读取容器 env 里的 `MYSQL_ROOT_PASSWORD`),其次 localhost,可选子网扫描(`--cidr 192.168.1.0/24`)——校验凭据、建库,然后启动 goacos 容器(linux/amd64 + linux/arm64 双架构镜像)。也可以显式指定:

```bash
./deploy/deploy.sh --host 10.0.0.5 --port-db 3306 --user root --password secret
./deploy/deploy.sh --auth --admin-pass S3cret --db prod_goacos --force
```

### Docker Compose(自带 MySQL)

```bash
docker compose up -d
# 控制台: http://localhost:8848/nacos/index.html  (nacos / nacos)
```

### 源码运行

```bash
make build && GOACOS_MYSQL_HOST=127.0.0.1 GOACOS_MYSQL_USER=root GOACOS_MYSQL_PASSWORD=... ./goacos serve
```

## 配置(环境变量)

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GOACOS_PORT` | `8848` | HTTP 监听端口 |
| `GOACOS_MYSQL_HOST` / `MYSQL_PORT` | `127.0.0.1` / `3306` | MySQL 地址 |
| `GOACOS_MYSQL_DB` | `goacos` | 数据库(不存在则自动创建) |
| `GOACOS_MYSQL_USER` / `MYSQL_PASSWORD` | `root` / `` | 凭据 |
| `GOACOS_AUTH_ENABLED` | `false` | 是否强制 access token |
| `GOACOS_AUTH_TOKEN_SECRET` | 内置开发密钥 | JWT 签名密钥(多副本必须一致) |
| `GOACOS_ADMIN_USERNAME` / `ADMIN_PASSWORD` | `nacos` / `nacos` | 播种管理员(仅 users 表为空时) |
| `GOACOS_HEARTBEAT_TIMEOUT_MS` | `15000` | 临时实例转不健康阈值 |
| `GOACOS_EPHEMERAL_DELETE_AFTER_MS` | `30000` | 临时实例删除阈值 |

`NACOS_*` 前缀同样有效。

## API 兼容性

对标 Nacos **3.2.x / 2.5.x HTTP Open API**(`/nacos/v1/*` 与 `/nacos/v2/*`),端点矩阵见 [docs/API.md](docs/API.md)。

开箱即用:curl/脚本、[nacos-sdk-go](https://github.com/nacos-group/nacos-sdk-go) HTTP 客户端、各类通过 REST API 访问 Nacos 的工具链。**尚未实现:** Java/Go SDK 2.x 使用的 gRPC 双向流通道(9848 端口)——这类客户端可通过 HTTP API 轮询工作;gRPC 在路线图中。

## 多副本

MySQL 即事实源,副本无状态:多个节点指向同一数据库、挂在同一 LB 后、设置相同的 `GOACOS_AUTH_TOKEN_SECRET` 即可。配置长轮询 ~1 秒内感知跨节点变更;实例生命周期清扫幂等,多节点并发执行安全。

## 开发

```bash
make test         # 单元测试
make e2e          # 全链路验收(自动起一个一次性 MySQL 8 容器)
make image-multi  # 构建 linux/amd64 + linux/arm64 镜像
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
