# Go 工程规范与代码约定

本文件面向人类开发者与学习复盘，沉淀本仓库的 Go 工程约定（含 Java 背景对照视角），也可直接作为从 0 起新 Go 项目的起手参考。
AI 助手每次对话必读的协作边界、验证命令与反模式红线见根目录 [AGENTS.md](../AGENTS.md)，存储层高危约束见 [internal/postgres/AGENTS.md](../internal/postgres/AGENTS.md)。

## 包划分、注释与命名

- **按业务领域或底座边界分包**：包名短小全小写（如 `link`、`tenant`、`click`、`httpx`），避开标准库重名。对照 Java 常见按 `handler/`、`service/`、`dto/` 技术层横向切分的习惯，Go 更强调按领域聚拢依赖，避免跨包循环依赖与无意义的数据搬运。
- **文件名全小写直接连写**：如 `clientip.go`、`ratelimit.go`，不用 `doc.go`、`utils.go`、`common.go` 等泛化名。
- **注释格式与职责**：单行注释用 `//`；多行注释用 `/* ... */`，块内行首不加 `*`。注释写“为什么”和“注意什么”，不复述代码。
- **包注释**：写在包主文件顶部，以 `Package 包名` 开头（`main` 包以 `Command 命令名` 开头）。
- **路由命名空间**：管理 API 走 `/api/v1`，公开跳转固定 `/r/{code}`，运维端点（`/healthz`、`/readyz`、`/metrics`）不版本化。

## 错误处理与并发

- **错误定义与判断**：哨兵错误用 `errors.New`，包装用 `fmt.Errorf("...: %w", err)`；错误比较与拆包统一使用 `errors.Is` / `errors.As` / `errors.AsType`（`.golangci.yml` 已启用 `errorlint` 自动检查）。
- **错误只处理一次**：要么原样或包装返回给调用方，要么在当前层消化处理，不要既打日志又向上返回同一错误。
- **异常边界**：不用 `panic` / `recover` 模拟 Java 的 `try-catch`；`recover` 只放在中间件和 goroutine 入口兜底。
- **goroutine 与取消/超时**：每个 goroutine 必须有明确的退出条件；数据库、Redis、HTTP 调用均传 `ctx` 并设超时。
- **并发状态保护**：共享状态加锁或用 `atomic`；带锁（`sync.Mutex` / `sync.RWMutex`）的结构体一律使用指针接收者，避免值拷贝导致锁失效。

## 接口、泛型与模块化扩展点

- **小接口与调用方定义**：接口由使用方（consumer）定义，尽量小（1–3 个方法），与 Java 习惯在实现侧先建大 `Service` 接口相反；透明装饰器（`cache`、`storemetrics`）按能力拆子接口再组合，不为凑方法数套转发适配器。
- **先具体类型后抽象**：先用具体类型写，出现第二个实现或测试替换外部依赖时再抽接口；泛型同理，重复明显后再抽象。
- **模块化扩展点**：新增外围特性或登录方式时，优先复用既有扩展点并通过 `GET /api/v1/capabilities` 自动对外宣告，不把新特性硬耦合进核心短链写事务或核心 `Server` 主体：
  - 存储层事务内事件：`postgres.TxHook`（见 `internal/postgres/txhook.go`）
  - 控制面登录方式：`console.AuthMethod`、`console.SessionIssuer`、`console.OAuthClient`
  - 控制面业务模块：`console.Module`
  - 后台任务模块：`WorkerModule`（见 `cmd/console/workermodule.go`）

## 结构化日志与宽访问日志（`internal/logx`）

- **统一根日志装配**：各进程统一通过 `logx.New`（配置加载失败或纯 CLI 兜底路径用 `logx.NewFallback`）构建 `*slog.Logger`，不裸用 `slog.Default()`，确保全局脱敏、`LevelVar` 动态调级与 `context` 顶层字段自动注入（`request_id`、`tenant_id`、`job_id`、`job_kind`、`trace_id`、`span_id`）一致生效。
- **宽访问日志（Canonical Log Line）**：HTTP 请求期上下文维度通过 `logx.SetActor`、`logx.SetCacheResult`、`logx.SetError`、`logx.SetRateLimited` 汇总进挂载在 `context` 上的 `RequestEvent`（类比 Java MDC 的请求级摘要），请求结束时由外层 `observe` 中间件统一写出唯一一条访问日志；业务 handler 不重复打印请求日志。

## 数据库、SQL 与 PostgreSQL 实践

- **SQL 组织与参数**：多行 SQL 抽到文件顶部包级 `const`，单行短 SQL 可就近内联；单条 SQL 控制在 6–8 个位置参数以内且编号严格按出现顺序递增；JSON 载荷优先在 Go 侧用结构体序列化，避免在 SQL 里多层手拼 `jsonb_build_object`。
- **事务编排**：控制面多步写一律在事务闭包（如 `withTenantTx`）内编排单职责 `xxxTx` 小函数，必选审计复用 `insertAuditLogTx`，外围副作用通过 `TxHook` 挂载；禁止用多业务域巨型写 CTE 堆叠副作用或通过“跳号插参”往旧 SQL 叠加需求。
- **租户隔离与 RLS**：所有租户读写经 `withTenantTx` / `withTenantReadTx`（`internal/postgres/tenanttx.go`）在事务内降权为受限角色 `shortlink_tenant` 并通过 `SET LOCAL` 绑定 `app.tenant_id`；新增含 `tenant_id` 的表时，迁移需同步授权 `shortlink_tenant`、`ENABLE ROW LEVEL SECURITY`（不加 `FORCE`，以免阻塞托管库非超管属主在跨租户白名单路径的查询）并建立零 `OR` 分支的 fail-closed policy。
- **索引与 HOT Update**：新增索引或改动查询/清理 SQL 时，在合成数据量上用 `EXPLAIN (ANALYZE, BUFFERS)` 确认命中预期索引；禁止用 `OR` 混合有索引列与无索引列导致全表扫描；避免给高频更新计数列建索引以保留 PostgreSQL HOT Update（仅在不更新索引列时调整 `fillfactor` 配合 HOT）。
- **分批清理、租约与分区 DDL**：后台清理与租约回收 SQL 一律带 `LIMIT` 分批执行；范围分区表分批删改必须依据完整主键元组，禁止跨分区不唯一的 `ctid`；租约回写校验单调递增 Fencing Token（`jobs.lock_version`）；分区 DDL 在事务内设 `SET LOCAL lock_timeout = '3s'` 且不碰 `*_default` 分区。

## 测试与工具链护栏

- **基础测试约定**：表驱动测试；HTTP handler 用 `httptest`；环境变量用 `t.Setenv`；依赖外部服务且缺环境变量时 `t.Skip`；全部测试默认开启 `-race`（`Makefile` 已配置）。
- **时间相关并发测试**：涉及定时器、ticker、限流窗口或超时唤醒的测试优先使用标准库 `testing/synctest`，替代依赖真实等待且易抖动的 `time.Sleep`。
- **自动化护栏分工**：能用工具拦住的约束尽量下沉到检查脚本，而不是堆在文档里：
  - `make fmt-check`：`gofmt` 格式检查。
  - `make vet` & `make lint`：`go vet` 与 `.golangci.yml`（启用 `standard` 及 `errorlint`、`nilerr`、`bodyclose`、`sqlclosecheck`、`rowserrcheck`、`copyloopvar`、`misspell`、`durationcheck`、`nolintlint`）。
  - `make arch-check`：校验 `cmd/redirector` 与核心底座包（`logx`、`httpx`、`tracex`、`config`、`safehttp`、`pgdb`、`pgredirect`、`identity`、`ratelimit`、`mailer`）的单向静态依赖边界，以及全仓 `otel/sdk` 与导出器的隔离边界。

## Git 提交规范

- 格式：`<type>: <摘要>`，摘要中文、动词开头、不加句号、50 字以内；必要时空一行写正文说明“为什么改与影响”。
- `type` 取值：`init` / `feat` / `fix` / `test` / `refactor` / `docs` / `chore`。
- 一次提交只做一件事，保持历史可回溯。
