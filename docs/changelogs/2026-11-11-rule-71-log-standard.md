# 2026-11-11 · 接入 rule 71 统一日志标准（goacos）

> 归档自 `vibe-coding/rules/71-log-standard.md` 与 `spec/logging/log-schema.yaml`。
> 摘要见仓库根 `CHANGELOG.md`。

## 背景

goacos 是 Nacos 兼容的配置中心 / 服务发现 server，8848 端口。
此前有**两条互不相通的日志管线**：

| 管线 | 位置 | 形态 | 级别 |
|---|---|---|---|
| 标准库 `log` | `internal/{app,api,core,gateway,storage}` 共 **40 处** | 纯文本 `2026/11/11 08:00:00 [goacos] ...` | **完全没有级别** |
| `slog` TextHandler | `cmd/goacos-registrar` | 纯文本 `time=... level=INFO msg=...` | 大写 `INFO` |

两条都是纯文本、都没有 rule 71 的 9 个必填字段，级别口径还不一致
（一条没级别、一条大写）⇒ 下游 `level=~"error|fatal"` 抓不到任何错误日志。

## 本仓的工作量在「定级」

标准库 `log` 没有级别概念 ⇒ 40 处 `log.Printf` **全部是同一优先级**。
接规范层之前必须**逐个按语义定级**，否则接上去之后
「错误日志」和「日常巡检」在下游仍然分不开。

| 级别 | 处数 | 判据 | 例子 |
|---|---|---|---|
| **INFO** | 19 | 正常流程的进展与结果 | 迁移进度、admin seed、监听就绪、访问日志、证书签发完成、命名清扫结果 |
| **WARN** | 13 | 可恢复 / 降级 / 需要人看一眼但服务仍在正常服务 | 证书不可用、证书目录不可读、ACME 账户不可用、证书已过期、路由表刷新失败（**保留旧快照继续服务**）、上游实例失败改试下一个、证书目录不可写回落临时路径、`ADMIN_USERNAME` 为空跳过 seed |
| **ERROR** | 8 | 真失败 | 命名/持久化清扫失败、监听器停止、ACME 签发失败、证书存储失败、代理无法提供服务 |

其中两处判据值得单独记：

- **路由表刷新失败**（`gateway.go`）是 WARN 不是 ERROR —— 失败时**保留旧快照继续服务**，
  不是不可用。
- **被 recover 的 panic**（`api/router.go`）归 **ERROR** 而不是 FATAL ——
  进程没退出，按 rule 71 §4.1「进程即将退出/不可恢复」的口径不属于 FATAL。

## 访问日志改成结构化

```go
// 此前
log.Printf("[goacos] %s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
// 现在
slog.Info("request served", "method", r.Method, "path", r.URL.Path,
    "duration_ms", time.Since(start).Milliseconds())
```

⇒ 下游可以按 method / path / 耗时聚合，而不是在文本里做正则。

## 保留名唯一（rule 71 §4.4）

本仓的保留名冲突**语义很明确**：所有 `service` 都不是 goacos 自己，
而是**被注册的那个服务**（nginx 前端、redclaw-console 等静态站）。

| 原键 | 新键 | 处数 | 位置 |
|---|---|---|---|
| `service` | `registered_service` | 16 | `cmd/goacos-registrar/main.go`（4）、`sdk/registry/service.go`（7）、`sdk/registry/client.go`（6）—— 表达**被注册的服务**名，与规范层的 `service`（goacos 自身标识）是两个实体 |
| `env` | `env_var` | 2 | `sdk/registry/service.go` —— `EnvIP` / `EnvEnabled` 是**环境变量名**，不是部署环境 |
| `version` | `schema_version` | 2 | `internal/storage/migrate.go` —— 数据库 schema 版本，不是服务版本 |

> ⚠ `sdk/registry` 是**对外 SDK**：日志键名变更属于**可观测性契约**变更
> （不是 API 契约变更），若有下游按旧键名采集需同步。

刻意不改的同名项：

- `cmd/goacos-registrar/flags.go` 的 `-service` —— **CLI flag 名**；
- `sdk/registry/client.go` 的 `json:"message"` —— **API 序列化字段**；
- `internal/metrics` 类的 Prometheus 标签。

## stdout 机器契约

`main.go` 有三个子命令各有 stdout 契约：

| 子命令 | stdout 契约 |
|---|---|
| `version` | `goacos <ver> (unique …) …` |
| `healthcheck` | `healthy` |
| `db` | 数据库扫描结果 |

⇒ 规范层**只装在 `serve` 分支**，且只装在子命令分派**之后**。
`serve` 路径自己那条 `fmt.Fprintln(os.Stderr, "goacos:", err)` 也改成
`slog.Error("server exited", "error", err)` ⇒ 纯文本行不再绕过规范层。

## 端到端验证

起了一个**独立的 PostgreSQL 容器**（真实建库、跑迁移、seed admin、监听），
再打三个 HTTP 请求产生访问日志：

```
✅ goacos: goacos.log 抽样 7 行全部合规
结论: errors=0 warnings=0
```

覆盖到的形态：迁移进度、admin seed、连库成功、config hub 预载、监听就绪、
访问日志（含 method/path/duration_ms）、`source` 平铺（`app.go:38`、
`router.go:195`）、`version=dev+b0`（`buildinfo.UniqueVersion()`）、
`env=local`。验证后临时容器已清理。

## 测试

- `internal/logging` 10 个用例全绿。
- 全量 `go test ./...` 10 个包全绿（基线 9 个包全绿 ⇒ 零回归）。
- `go build ./...` 与 `go vet ./...` 无输出。

## 顺带补充的能力

`internal/logging` 新增 `NewStandardHandler(w, opts)` ——
返回一个符合 rule 71 的 handler 而**不碰全局状态**。
registrar 需要它：它要把自己的 `*slog.Logger` 传给 SDK，
不能靠 `slog.SetDefault`（那是全局的，且 registrar 与主服务是两个进程）。

> 这个能力此前只有 crm-go 一家按需加在自己的副本里，
> 现在两个仓都有 ⇒ 应回灌到规范仓的参考实现
> （`templates/logging/go`），避免第三个仓再复制一遍。
