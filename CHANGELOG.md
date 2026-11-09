# Changelog

本文件记录 goacos 的**对外可见**变更（格式参考 Keep a Changelog）。

> 详细的变更归档在 `docs/changelogs/`；本文件只放摘要与指针。

---

## [Unreleased] - 2026-11-11 · 接入 rule 71 统一日志标准

### ✨ Added
- **rule 71 统一日志标准接入**：新增 `internal/logging/` 规范层，输出符合
  `vibe-coding/rules/71-log-standard.md` 的结构化 JSONL——
  9 个必填字段（`timestamp`/`severity_text`/`severity_number`/`level`/`msg`/
  `service`/`version`/`env`/`instance_id`）、扁平单层、handler 层统一脱敏。
  - **两条管线合并成一条**：此前主服务全程用标准库 `log.Printf`（**纯文本**、
    40 处、且**完全没有级别概念**），`cmd/goacos-registrar` 另用
    `slog.NewTextHandler`（**纯文本**、且 `level` 大写）。
    现在两条都走规范层 ⇒ 同一个服务两种格式的问题消失。
  - **规范层必须早装**：`main.go` 的 `serve` 分支在 `config.FromEnv()` 之前先
    `InstallEarly` —— 连库、迁移、seed 都会打日志，晚装 ⇒ 那些行是旧格式。
  - `AddSource` 打开：调用位置平铺成 `"source":"app.go:38"`。
  - 新增环境变量 `GOACOS_ENV`（部署环境，默认 `local`）与
    `GOACOS_LOG_LEVEL`（默认 `info`）。
  - 归档：`docs/changelogs/2026-11-11-rule-71-log-standard.md`。

### 🔧 Changed
- **40 处 `log.Printf` 全部改走 `slog` 并按语义定级**（标准库 `log` 没有级别
  ⇒ 此前 40 行全是同一优先级，下游无法按级别过滤）：
  - **INFO**（正常流程，19 处）：迁移进度、admin seed、监听就绪、访问日志、
    证书签发/镜像完成、命名清扫结果等。
  - **WARN**（可恢复/降级，13 处）：证书不可用、证书目录不可读、ACME 账户不可用、
    证书已过期、路由表刷新失败（保留旧快照）、上游实例失败改试下一个、
    证书目录不可写回落临时路径、`ADMIN_USERNAME` 为空跳过 seed 等。
  - **ERROR**（真失败，8 处）：命名/持久化清扫失败、监听器停止、ACME 签发失败、
    证书存储失败、代理无法提供服务等。
  - 访问日志从 `log.Printf("[goacos] %s %s %s", method, path, duration)` 改为
    `slog.Info("request served", "method", …, "path", …, "duration_ms", …)`
    ⇒ 字段可被下游按 method/path/耗时聚合。
  - **被 recover 的 panic 归 ERROR 而非 FATAL**（进程未退出，按 rule 71 §4.1）。
- **保留名唯一**（rule 71 §4.4）：规范层**剥离**与 9 个必填字段 +
  `time`/`msg`/`message` 冲突的属性并记入 `reserved_name_conflicts` 诊断字段。
  业务侧同步改名：
  - **`service` → `registered_service`**（16 处：`cmd/goacos-registrar/main.go` 4 处、
    `sdk/registry/service.go` 7 处、`sdk/registry/client.go` 6 处）——
    这些 `service` 指的是**被注册的那个服务**的名字（另一个实体），
    与规范层的 `service`（goacos 自身服务标识）语义不同。
  - **`env` → `env_var`**（2 处，`sdk/registry/service.go`）——
    `EnvIP` / `EnvEnabled` 是**环境变量名**，不是部署环境。
  - **`version` → `schema_version`**（2 处，`internal/storage/migrate.go`）——
    数据库 schema 版本，不是服务版本。
  - 刻意不改：`cmd/goacos-registrar/flags.go` 的 `-service` **CLI flag 名**；
    `sdk/registry/client.go` 里 `json:"message"` 的**API 序列化字段**。
- **serve 路径的错误输出走规范层**：`main.go` 此前用
  `fmt.Fprintln(os.Stderr, "goacos:", err)` ⇒ 纯文本行绕过规范层；
  改为 `slog.Error("server exited", "error", err)`。
  （`version` / `healthcheck` / `db` 三个子命令的 **stdout 机器契约**保持不变。）

### 🧪 Tests
- `internal/logging` 10 个用例全绿。
- 全量 `go test ./...` **10 个包全绿**（基线 9 个包全绿 ⇒ 零回归）；
  `go build ./...` 与 `go vet ./...` 均无输出。
- **端到端**：起独立的 PostgreSQL 容器（真实建库、迁移、seed、监听），
  采样 7 行（含访问日志），`verify-log-standard.py --service goacos`
  ⇒ **errors=0**，9 个必填字段齐备、`source` 平铺、`version=dev+b0`、
  `env=local`。临时容器已清理。

### 📌 Known Issues
- `sdk/registry` 是**对外 SDK**：日志键名从 `service` 改为 `registered_service`、
  `env` 改为 `env_var` 属于可观测性契约变更（不是 API 契约变更），
  若有下游按旧键名采集需同步。
- `goacos` 已登记进工作区根仓 `deploy/platform/services.json`
  （键名 `goacos`、端口 8848、`repo_path` 指向本仓）⇒ 门禁 `--services-from`
  现在能校验它。
