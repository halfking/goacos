# 需求第 7 条复核：goacos 的应用模型**能**表达前端后端完整应用 —— R8 卡的不是模型

**日期**：2026-10-15 14:4x
**类别**：需求第 7 条（强化 acc 与 goacos，支持前端后端完整应用）· 只读代码审计
**未接触** 252 / 211，未改任何代码。

---

## 一句话

我原以为「`round_robin` 对前端是错的」是**模型缺陷**。
读完实现发现不是 —— **模型能表达完整前端应用**，
缺的只是「把它开起来」和「按前端语义配 `lb_algorithm`」。
R8 不成立的原因仍然是 R2（网关没开），**不是能力缺失**。

## 一、三张表（`002_application.mysql.sql`，v1.2.0 迁移新增）

| 表 | 关键列 |
|---|---|
| `application` | `name` / `env` / `metadata` |
| `application_component` | `kind`(默认 `api`) / `protocol` / `service_name`+`group_name`+`cluster_name`（绑 nacos 服务）/ `health_path` / `weight` / **`lb_algorithm`** / `sticky` / `metadata` |
| `route_binding` | `domain` + `path_prefix` → `app_name` + `component_name` + `upstream_name` + `priority` |

⇒ 一个应用可以挂多个组件（前端 + 后端），各自绑不同的 nacos 服务。

## 二、四个前端关切，逐个查了实现，不是看表猜的

| 前端关切 | 模型能否表达 | 实现依据 |
|---|---|---|
| **多组件（前端 + API 同应用）** | ✅ | `application_component` 按 `app_name` 聚合 |
| **按路径分流** | ✅ | `route_binding.path_prefix`，`sortRouteEntries` **最长前缀优先**，其次 `priority` |
| **rewrite（剥前缀再转发）** | ✅ | `routetable.go:323 stripPrefixRequested(comp)` 读 `metadata["stripPrefix"]`，`Target.StripPrefix` 带到转发层 |
| **会话亲和** | ✅ | `lb.go` 支持 `ip_hash` / `least_conn` / **`cookie_sticky`** |
| **SPA 回落（未匹配路径回 index.html）** | ⚠ **无内建回落，但可声明** | `Lookup` 是**首个匹配即返回**；但用 `path_prefix='/'` 声明一条 catch-all 绑定即可 |

**⇒ 「`round_robin` 对前端是错的」这句话要改写成：
「前端组件必须把 `lb_algorithm` 从默认的 `round_robin` 改成 `cookie_sticky`（或 `ip_hash`）」。
这是**配置义务**，不是模型缺陷。**

## 三、所以 R8 为什么还是不成立

| 原因 | 性质 |
|---|---|
| 网关没开（252 无网关代码；211 有但开关未启） | **R2，未解** |
| 前端组件需要逐个把 `lb_algorithm` 从 `round_robin` 改成 `cookie_sticky` | **配置义务，可做** |
| 现有 60 个 vhost 的分流今天在 nginx 上 | 迁移期的现实约束 |

**能力齐了，缺的是启用与迁移。** 这比「模型表达不了」要具体得多，
也说明第 7 条不是一笔重写，是**开网关 + 按前端语义配 LB** 两件事。

## 四、必须说清的边界

* 以上**全部是读代码得出的**，**没有在真机上跑过一次**。
  网关在 252 上根本没有代码，在 211 上开关未开 ⇒ **无法端到端验证**。
* `stripPrefix` 只是一个**布尔开关**（`true/1/yes/on`），
  **不能做任意路径重写**（例如 `/old/a/b` → `/new/a/b`）。要那种语义得另说。
* SPA 回落靠 catch-all 绑定是**约定**，不是内建行为 ——
  漏配就会 404，**没有任何东西会提醒你**。

## 五、对判据的影响（未改）

`test_application_declaration_wiring.py` 的两条红是关于**切路由会丢 nginx 加固**
与端口漂移，**与本文件无关**。本文件不构成对它们的放宽理由，
也没有改任何判据或加任何 skip。
