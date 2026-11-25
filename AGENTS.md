# AGENTS.md

本文件写给在本仓库工作的 AI 助手（实现与审查共用），同时也是仓库的工程约定。
它是活文档：约定有变化或复盘出新红线时直接改这里，不另起一份。

## 项目定位与协作方式

- 学习与工程演进项目：用短链接服务学 Go。用户有 Java 背景，配套笔记在 Notion。
- `docs/roadmap.md` 记录阶段进度，关键设计先立 ADR（`docs/adr/`），完成后同步更新 `README.md`、`docs/`、本文件与包注释。
- 协作模式：用户定方向与 ADR，AI 负责实现、验证与 code review；**未收到明确修改指令时（纯提问、讨论、评审），只给结论与方案，不擅自改代码**。
- 解释概念与设计取舍时对照 Java（Java 怎么做、Go 为什么不同）；不确定的标准库或第三方行为先查证代码或文档再讲，不凭印象下结论。
- 推送、开 PR 是对外动作，除非用户明确要求，不要自己推。

## 反模式红线（实现与 Review 必查）

工具（`gofmt`、`go vet`、`golangci-lint`、`arch-check`）能拦的交给检查脚本；以下 6 条工具查不出来、但 AI 迭代时高频踩坑的结构性反模式，实现与 review 时必须逐条核对：

1. **单一事实来源（防双存配置与重复计算）**：同一份配置或请求期衍生值（如可信代理链、客户端 IP、请求 ID）只在归属底座持有并解析一次，下游经 `context` 或显式接口消费；不在多个 `Server`/结构体里复制配置字段或重复计算。
2. **禁止留“伪兼容”壳与“半死分支”**：区分**真兼容**（外部已发出的 `/r/{code}` 短链、线上存量库表数据、多副本滚动发布窗口）与**伪兼容**（同仓库可控的 `web/`、`smoke.sh` 与旧测试）。重构或升级接口、路由与字段时，同步迁移全部仓内调用方并删除旧入口与双存字段，不用假默认值（如功能关闭时吐 `0`）掩盖语义变化；禁止在非 `_test.go` 文件中保留仅供测试或旧内部调用方使用的兼容壳。
3. **改共享语义时回溯整条调用链，禁止用注释合理化异常分支**：改动共享辅助逻辑或取消/超时语义（如引入 `context.WithoutCancel`、新增并发限流槽位）时，从请求入口重新走查边界场景（尤其是调用方 `ctx` 已取消/已超时）。遇到“按理不该走到这里”的状态分支，追根在入口拦截，禁止补一句注释把反常路径说圆。
4. **局部改动必须对齐同文件与邻近实现**：修改或新增函数前先通读当前文件的错误判断、注释与上下文用法，避免同一文件混用等价风格（如错误比较一律用 `errors.Is` / `errors.AsType`，不混用 `==`）。
5. **测试替身与断言必须校验“不该发生的副作用”**：`fake`/stub 测取消、超时、限流、降级等边界时，不仅断言返回值或指标，还要断言不该发生的副作用确实未发生（如取消后下游 `calls == 0`、未占并发槽位、未写缓存），避免用忽略 `ctx` 的假实现把 bug 焊死成测试断言。
6. **禁止用多业务域巨型写 CTE 堆叠副作用（防位置参数乱序与双轨实现）**：禁止把主业务表与审计日志、异步任务、Webhook 扇出等多个无关业务域硬塞进同一条多表修改 CTE，也禁止通过“跳号插参”往旧 SQL 上叠加新需求。控制面多步写一律在事务闭包（如 `withTenantTx`）内编排单职责 `xxxTx` 小函数，已有事务 helper（如 `insertAuditLogTx`、`enqueueWebhookEventTx`）必须复用；仅在单表/单域并发原子锁或只读跳扫场景（如 `FOR UPDATE SKIP LOCKED` 任务抢占与分批回收）保留 CTE。

## Go 代码约定

### 注释与命名

- 单行注释用 `//`；多行注释用 `/* ... */`，块内行首不加 `*`。注释写“为什么”和“注意什么”，不复述代码。
- 包注释写在包主文件顶部，以 `Package 包名` 开头（`main` 包以 `Command 命令名` 开头）。
- 文件名全小写直接连写（如 `clientip.go`、`ratelimit.go`），不用 `doc.go`、`utils.go`、`common.go` 等泛化名。
- 包名短小全小写、按业务领域或底座边界分包（如 `link`、`tenant`、`click`、`httpx`），避开标准库重名；不按 Java 习惯拆 `handler/`、`dto/` 等技术层（见 `docs/adr/0019-httpapi-package-evolution.md`）。
- 路由命名空间约定见 `docs/adr/0001-api-versioning-and-route-namespace.md`（管理 API 走 `/api/v1`，公开跳转固定 `/r/{code}`，运维端点不版本化）。

### 错误处理与并发

- 哨兵错误用 `errors.New`，包装用 `fmt.Errorf("...: %w", err)`；判断统一用 `errors.Is` / `errors.As` / `errors.AsType`。
- 错误只处理一次：要么返回，要么在此处理，不既打日志又返回。
- 不用 `panic`/`recover` 模拟 `try-catch`；`recover` 只放在中间件和 goroutine 入口。
- 每个 goroutine 必须有明确的退出条件；数据库、Redis、HTTP 调用均传 `ctx` 并设超时。
- 共享状态加锁或用 `atomic`；带锁的结构体一律用指针接收者。

### 接口与泛型

- 接口由使用方定义，尽量小（1–3 个方法）；透明装饰器（`cache`、`storemetrics`）按能力拆子接口再组合，不为凑方法数套转发适配器。
- 先用具体类型写，出现第二个实现或测试替换外部依赖时再抽接口；泛型同理，重复明显后再抽象。
- 新增外围特性或登录方式时优先复用既有模块化扩展点（`postgres.TxHook`、`console.AuthMethod`、`console.Module`、`WorkerModule`），并通过 `/api/v1/capabilities` 自动对外宣告，不把新特性耦合进核心短链写事务或 `console.Server` 主体（见 `docs/adr/0029-modular-feature-plugins.md`）。

### 数据库与 SQL

- 多行 SQL 抽到文件顶部包级 `const`，单行短 SQL 可就近内联。
- 单条 SQL 控制在 6–8 个位置参数以内且编号严格按出现顺序递增；JSON 载荷优先在 Go 侧用结构体序列化，避免在 SQL 里多层手拼 `jsonb_build_object`。
- 新增索引或改动查询/清理 SQL 时，在合成数据量上用 `EXPLAIN (ANALYZE, BUFFERS)` 确认命中预期索引；禁止用 `OR` 混合有索引列与无索引列导致全表扫描。
- 后台清理与租约回收 SQL 一律带 `LIMIT` 分批执行；范围分区表分批删改必须依据完整主键元组，禁止跨分区不唯一的 `ctid`；租约回写校验单调递增 Fencing Token（如 `jobs.lock_version`）；分区 DDL 在事务内设 `SET LOCAL lock_timeout = '3s'` 且不碰 `*_default` 分区（见 `docs/adr/0022-postgres-query-and-lease-hardening.md`、`docs/adr/0026-timeseries-partitioning-and-retention.md`）。
- 避免给高频更新计数列建索引以保留 PostgreSQL HOT Update；仅在不更新索引列时调整 `fillfactor` 配合 HOT。

### 测试

- 表驱动测试；HTTP handler 用 `httptest`；环境变量用 `t.Setenv`；依赖外部服务且缺环境变量时 `t.Skip`。
- 所有测试默认带 `-race`（`Makefile` 已配置）。

## 验证、AI Review 与提交规范

### 验证命令

- `make verify`：`fmt-check`、`go vet`、`go test -race`（单测）、`make arch-check`、`golangci-lint`、`build`（不依赖 Docker）。
- `make web-check`：改动 `web/` 时必跑（Prettier、`tsc --noEmit`、ESLint、Vitest、`vite build`；前端规范见 `web/AGENTS.md`）。
- `make selfcheck`：重建 `TEST_DATABASE_URL` schema（迁移升最新、降空、再升最新）并跑完整 `verify` + `test-integration` + 真实进程 `smoke`（需 Postgres/Redis/Dex 可连；仅无本机依赖时再执行 `make deps-up`）。改动 Go 行为后报告完成前必跑；若改了 HTTP 契约，同步更新 `docs/api.md` 与 handler 测试。
- CI 配置在 `.github/workflows/ci.yml`，push 到 main 与 PR 会跑 `make verify`、`make test-integration` 和 `make smoke`。

### AI Review（提交前自查 & 独立审查共用）

AI 写代码容易顺着当前思路局部最优，AI 审查 AI 也容易流于表面夸赞。无论提交前自查还是独立审查，都必须通读完整 `git diff --staged`（或目标提交），并**把写法与行为分开报告**：

- **写法检查**：对照本文件代码约定与 6 条**反模式红线**逐条核对。
- **行为检查**：对照对应 ADR（决策、不做什么、验证方式）与 roadmap 条目核对；**至少挑一条异常/边界路径（如中途 `ctx` 取消、超时、并发槽位满、部分失败）顺着完整调用链人肉走一遍**，确认没有语义漏洞后再给结论。发现问题先指明问题和根因，再给修改建议。

### 提交规范

- 格式：`<type>: <摘要>`，摘要中文、动词开头、不加句号、50 字以内；必要时空一行写正文说明“为什么改与影响”。
- `type` 取值：`init` / `feat` / `fix` / `test` / `refactor` / `docs` / `chore`。
- 一次提交只做一件事，保持历史可回溯。

## 文档地图

- `README.md`：项目概览、快速开始与文档入口。
- `docs/`：架构、API、运行手册、路线图、ADR 与问题复盘（`docs/postmortems/`）。
- `AGENTS.md`（本文件）：全局协作方式、反模式红线、Go 约定与验证/提交规范。
- `web/AGENTS.md` & `web/README.md`：前端控制台分层边界、规范与导读。
