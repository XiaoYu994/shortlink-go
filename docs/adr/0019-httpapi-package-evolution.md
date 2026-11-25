# 0019. 暂不拆分 httpapi，先拆结构体再按依赖分叉拆包

- 状态：Accepted
- 日期：2026-11-19

## 背景

`internal/httpapi` 目前有 22 个生产文件（共约 1,853 行）和 16 个测试文件（共约 3,118 行），是整个仓库文件最多的包。

看着拥挤主要有两个原因，性质不同：

1. **文件粒度很细**：例如 `healthz.go`（13 行）、`chain.go`（14 行）、`timeout.go`（22 行）、`deletelink.go`（27 行）。实际生产代码量不到两千行，在 Go 里仍属于中小规模单包。
2. **`Server` 正在承接两类不相关的依赖**：`Server` 同时持有 redirector 的依赖（`clicks`、`visitorSalt`、`trustProxy`）和 console 的依赖（`admin`、`stats`、`keys`、`authz`、`limiter`）。阶段 11–15 还要陆续加入 OIDC 登录、会话、成员管理、审计日志、Webhook、自定义域名和套餐计费，如果全部继续挂在 `*Server` 上，会演变成难以测试的上帝结构体。

另外，Go 里的 package 不是 Java 里单纯的命名空间，而是编译单元、封装边界（首字母大小写）和依赖图节点。`httpapi` 内的 handler 和中间件共用大量包内私有方法（`writeJSON`、`writeError`、`clientGone`、`handleStoreError`、`principal`），按技术分层硬拆反而会破坏封装。

## 决策

### 1. 判断要不要拆包的三把尺子

以后任何包面临"要不要拆"的抉择，按这三条判断：

1. **文件碎 vs 代码量大**：文件数多但总行数不到 2,000 行，先控制文件碎片化，不急着拆包。
2. **导出成本**：拆开后如果被迫把大量内部辅助方法改成大写导出，说明边界还没切对，拆完封装反而变差。
3. **依赖分叉**：存在明确的依赖边界（例如零业务依赖的通用 HTTP 底座、或 redirector 根本不需要 console 的依赖）时，才是真正的拆包信号。

### 2. `httpapi` 的三步演进路线

- **第 1 步（当前）**：保持 `internal/httpapi` 单包不动，避免在阶段 11 刚起步时做大规模机械搬迁。新增强相关的端点放进同一文件（如 OIDC 登录、回调、登出、`/me`），不再为几十行的小函数单独建文件。
- **第 2 步（阶段 11 写登录和成员管理 API 时，已完成）**：**先拆结构体，暂不拆包**。
  在 `httpapi` 包内按业务模块拆出独立的小结构体（例如 `authHandler` 只持 OIDC 和会话依赖，`memberHandler` 只持成员存储依赖），继续复用包内私有的 `writeJSON`、`writeError`、`principal`，最后由 `routes.go` 挂到同一个 `ServeMux` 上。这样既避免 `Server` 膨胀成上帝对象，又零导出污染、零循环依赖（对照 Java：拆分多个 Controller，但 Go 里不必为此各建子包）。
- **凭证代码归位（已完成）**：API Key 令牌格式、生成、哈希及 `ErrUnauthenticated` 已并入 `identity`（与会话令牌统一使用 `TokenFormat` 和 `HashToken`），`tenant` 只保留 `Tenant` 领域模型；`postgres` 继续负责密钥签发、列表与吊销。
- **第 3 步（阶段 12 前，已完成）**：按"零业务依赖底座 + 进程/业务边界"完成拆包，并移除 `internal/httpapi`：
  - `internal/httpx`：零业务依赖的通用 HTTP 底座（`chain`、`requestid`、`logging`、`metrics`、`recoverer`、`timeout`、`healthz`/`readyz`/`pprof`、`clientip`、JSON 响应与错误写出）。不 import `link`、`tenant`、`identity`、`click`、`idgen` 等任何业务包，杜绝循环依赖。
  - `internal/redirector`：公开短链跳转 HTTP 层（`GET /r/{code}` + `LinkStore` / `ClickRecorder` 最小接口），复用 `httpx`；不引入 `identity`、`tenant`、限流、`oauth2` 或 `console`。
  - `internal/console`：管理进程 HTTP 层（`authHandler`、`memberHandler`、按租户限流及 `shortlink_rate_limited_total` 指标、短链 CRUD 与统计），复用 `httpx`。
  - 使 HTTP 层代码结构与 [ADR 0009](0009-process-split-and-credentials.md) 的进程边界一致（注：存储层目前仍同处单包 `internal/postgres`，因此 `cmd/redirector` 的传递依赖仍会经由 `postgres` 触及 `identity`；存储包按领域拆包属独立议题，不在本 ADR 范围）。

## 不做什么

- **不按技术分层拆包**：不建 `httpapi/handler`、`httpapi/middleware`、`httpapi/dto` 这类 Java 风格分层目录。
- **不在阶段 11 起步阶段提前拆出 `internal/httpx`**：当前只有两千行代码，先专注完成用户、组织和权限主线，避免提前重构引入无收益的改动面。

## 怎么验证

本次仅记录架构约定，不改代码。后续执行第 2、3 步时按以下标准验收：

- 第 2 步完成时：新增的登录、成员 handler 各自只持有自己需要的依赖，不继续往 `Server` 塞无关字段；`make verify` 与 `make selfcheck` 全部通过（已完成）。
- 第 3 步拆包完成时：用 `go list -f '{{.ImportPath}} -> {{.Imports}}' ./internal/httpx ./internal/redirector ./cmd/redirector` 验证：
  - `internal/httpx` 不依赖任何业务领域包（`link`、`tenant`、`identity`、`click`）；
  - `internal/redirector` 与 `cmd/redirector` 不直接依赖 `identity` 或 `console` 管理包。
  - `make verify` 与 `make selfcheck` 全部通过（已完成）。

## 后果

- 阶段 11 的开发摩擦保持最低，同时在登录和成员 API 落地时通过拆结构体防住 `Server` 膨胀。
- 未来模块变多时有明确的拆包时机和目标目录，不需要临时拍脑袋，也不会误拆成技术分层。
- 短期内 `internal/httpapi` 的文件列表仍然偏长，靠清晰的文件命名和 `routes.go` 总览保持可读性。
- 2026-11-24（演进完成与延续）：本 ADR 第 3 步已落地为 `internal/httpx`、`internal/redirector`、`internal/console` 三包；随后全量架构审查进一步把鉴权前按 IP 限流收口进 `internal/console/request.go`，并为 `httpx` 增加独立运维端口监听与可信代理链校验（见 [ADR 0024](0024-process-boundary-and-ops-isolation.md)），同时以 `make arch-check` 自动化守住底座与公开面依赖隔离。
