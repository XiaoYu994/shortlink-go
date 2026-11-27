# 0032. OpenTelemetry 分布式追踪与日志关联（Phase C）

- 状态：Accepted
- 日期：2026-11-27
- 相关：`0005`, `0006`, `0008`, `0009`, `0020`, `0023`, `0024`, `0027`, `0029`, `0031`

## 背景

在阶段 5（[ADR 0005](0005-prometheus-before-tracing.md)）中，项目按“先指标、后追踪”的顺序先落下了 Prometheus 指标与 `ClickRecorder` 观测；随后在 2026-11-24 的[日志体系架构审查](../postmortems/2026-11-24-logging-system-audit.md)中，Phase A 与 Phase B 已落地（`internal/logx` 统一根日志与脱敏、动态 `LevelVar`、`RequestEvent` 宽访问日志与 `redirector` 采样），并在 `logx.contextHandler` 中预留了 Phase C 的合流位。

随着项目演进为 `redirector`、`console serve`、`console worker` 三进程协作，核心链路已涵盖 Redis 读路径保护（`singleflight` + `missSlots`/`fallbackSlots` bulkhead）、PostgreSQL Outbox 异步任务（`link.risk_scan`、`webhook.deliver`、`invitation.email`、`link.purge_stats`）与 `ClickRecorder` 异步批写。当前需要完成阶段 16 的最后半步：在不破坏包边界与极简模式零开销的前提下，把 HTTP、缓存、数据库、异步任务与结构化日志串联成完整的可观测链路。

## 决策

### 1. OTel API 与 SDK 严格分层（`make arch-check` 强约束）

- **唯一 SDK 持有者 `internal/tracex`**：新建纯基础设施包 `internal/tracex`（零 `internal/*` 依赖），唯一允许引入 `go.opentelemetry.io/otel/sdk/...` 与 `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`；仅由组合根 `cmd/redirector`、`cmd/console`（`serve` / `worker`）调用初始化。
- **轻量 API 插桩层**：`internal/logx`、`internal/httpx`、`internal/pgdb`、`internal/storemetrics`、`internal/cache`、`internal/job`、`internal/postgres`、`internal/risk`、`internal/webhook`、`internal/click` **仅允许引入 OTel API 包**（`go.opentelemetry.io/otel`、`.../trace`、`.../codes`、`.../attribute`、`.../propagation`），严禁依赖 `otel/sdk` 或 `otel/exporters`。
- **纯叶子包零 OTel 依赖与无直接引入约束**：`link`、`tenant`、`idgen`、`audit`、`ratelimit`、`safehttp`、`mailer` 保持传递零 OTel 依赖；`config`、`identity`、`sms` 仅因复用 `logx`（或 `pgdb`）而传递触达 `otel/trace` 值类型，但严禁直接 import 任何 OpenTelemetry 包（由 `make arch-check` 分别校验）。
- **配置与零开销开关（Noop）**：`config.BaseConfig` 新增 `TracingConfig`（`OTEL_EXPORTER_OTLP_ENDPOINT` 与 `OTEL_TRACES_SAMPLE_RATE`，默认采样率 `1.0`）。未配置 `OTEL_EXPORTER_OTLP_ENDPOINT` 时，`tracex.Init` 返回 `enabled=false` 与空 `shutdown`，不创建 `BatchSpanProcessor`、不起后台协程、不改变全局默认 Noop TracerProvider。
- **关停顺序（LIFO 最终落盘）**：三进程在 `logger` 初始化后即调用 `tracex.Init` 并注册首个 `defer shutdownTracing`（LIFO 最后执行），确保 HTTP `Shutdown`、`ClickRecorder.Close`、`Runner.Run` 排空与关闭 DB/Redis 之后再将内存缓冲的末尾 Span 完整 flush 到导出器。

### 2. HTTP 入口追踪与日志 Phase C（`internal/httpx` + `internal/logx`）

- **融入 `httpx.observe`**：在 `Server.observe` 中复用单次 `routePattern(mux, r)` 计算路由模板，跳过 `/metrics` 与探针 `/healthz`/`/readyz`。对业务请求提取入站 W3C `traceparent` 后创建 `SpanKindServer` 根/子 Span；未匹配路由仅使用受控方法名（`methodLabel(method)`）作为 Span 名且不记录高基数 URL 路径；请求结束时写入状态码、响应字节数以及 `RequestEvent` 中的低基数业务维度（`shortlink.tenant_id`、`shortlink.actor.kind`、`shortlink.cache.result`、`shortlink.rate_limited`），仅 `status >= 500` 标为 `codes.Error`。
- **日志 Phase C 自动关联**：`logx.contextHandler` 从 `ctx` 提取 `trace.SpanContextFromContext(ctx)`，在有效时自动向顶层追加 `trace_id` 与 `span_id`（兼容 `With` 与 `WithGroup`）。配合 `httpx.observe` 在 Span 激活期内写出 Canonical Log Line，以及 `audit_logs` 已有的 `request_id`，打通 **审计表（`request_id`）↔ 结构化日志（`trace_id`/`span_id`）↔ 分布式链路**。

### 3. 缓存、Singleflight 与 PostgreSQL 查询追踪

- **缓存与 Singleflight Span Link（`internal/cache`）**：
  - `GetByCode` 在入口 `ctx.Err()` 拦截之后启动 `cache.get`；`readCache` 记录 `redis.get`（`SpanKindClient`）；`writeCache` 记录 `redis.set` / `redis.setnx`。
  - `singleflight` 合并回源时，领头请求在脱离取消信号的 `fetchCtx`（`context.WithoutCancel` + 独立 `FetchTimeout`）上启动 `cache.origin_fetch`，保留领头请求 Trace 上下文；所有共享结果的等待者在各自 `cache.get` Span 上标记 `cache.singleflight.shared=true`、`cache.singleflight.leader=false`，并通过 `span.AddLink` 链接到领头请求的 `cache.origin_fetch` Span，既真实还原合并关系又避免父子 Span 交叉阻塞。
- **存储逻辑 Span（`internal/storemetrics`）**：为 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`、`ban`、`ban_if_url`、`unban` 包裹 `store.<op>`，错误分类沿用 Prometheus 的 `classifyResult`（仅真实错误与超时标 `codes.Error`）。
- **PostgreSQL 查询追踪（`internal/pgdb`）**：`pgdb.Open` 改用 `pgx.ParseConfig` + `stdlib.OpenDB` 挂载原生 `pgx.QueryTracer`。`TraceQueryStart` 首先检查 `!trace.SpanFromContext(ctx).IsRecording()`，未采样或关闭追踪时零分配直接跳过；采样时记录 `pg.<op>`（`SpanKindClient`）、参数化 SQL 摘要（`db.query.text`，折叠空白并安全截断）与 `server.address`/`server.port`，**绝不记录查询参数值**；通过私有 `pgSpanKey{}` 严格隔离所建 Span，防止 `TraceQueryEnd` 误关外层父 Span。

### 4. PostgreSQL Outbox 跨进程追踪（`jobs.traceparent` + Span Link）

- **迁移 `018`**：为 `jobs` 表新增 `traceparent TEXT NOT NULL DEFAULT ''`（长度检查 `<= 128` 字节）。在所有业务事务入队点（`EnqueueJob`、`insertJobTx`、`InvitationEmailHook`、`EnqueueWebhookTest`、`enqueueWebhookEventTx`）从当前 `ctx` 提取 W3C `traceparent` 一并写入数据库。
- **Consumer Span Link（`internal/job`）**：`Runner.executeJob` 抢占任务后，不强行把异步任务拼成同步 HTTP 请求的子 Span（防止数秒/重试延迟拉歪同步接口火焰图），而是按 OpenTelemetry Messaging 规范以 `trace.WithNewRoot()`、`SpanKindConsumer` 启动独立 Span `job.process <kind>`，若 `j.Traceparent` 合法则挂载 `trace.WithLinks(trace.Link{SpanContext: producerSC})` 与 `shortlink.job.source_trace_id`（畸形值静默忽略）。该消费 Span 上下文同时传入业务 `Handler`（包括 `risk.scan`、`webhook.http_deliver`）和状态写回 `commitCtx`。
- **出站与点击批写（`internal/risk` / `internal/webhook` / `internal/click`）**：`risk.NewHandler` 仅在缓存未命中发起 `Scanner.Scan` 时创建 `risk.scan`（`SpanKindClient`）；`webhook.NewHandler` 包裹 `webhook.http_deliver`（`SpanKindClient`）；`click.Recorder.flush` 为非空点击批次创建 `click.flush` 并驱动下层 `pgredirect.RecordClicks` SQL Span。

## 不做什么

1. **不引入重型第三方自动插桩库**（如 `otelhttp`、`otelpgx`、`redisotel`）：避免引入过多间接依赖与无法精细控制的行为（例如 `ServeMux` 未匹配路径基数爆炸、`singleflight` 链接语义与 `pgdb` 零开销短路）。
2. **不给后台空轮询与运维探针生成 Span**：`worker` 每 200ms 的 `Claim` 轮询、`reapLoop`/`metricsLoop` 以及 `/healthz`/`/readyz`/`/metrics` 不建立根 Span（`pgdb` 依靠 `IsRecording()` 自动静默），避免存储与导出成本被空轮询淹没。
3. **不向不可信外部站传播追踪头**：`risk` 扫描任意目标 URL 与 `webhook` 投递租户外部地址时，**不**在出站 HTTP 请求中注入 `traceparent`，避免暴露内部链路信息或引入非预期请求头。
4. **不替代 Prometheus 指标**：Metrics 继续沿用 [ADR 0005](0005-prometheus-before-tracing.md) 的自建 Registry 与 `/metrics`；Jaeger 放在 `docker-compose.yml` 的可选 `tracing` profile，不拖慢日常 `make up`。
5. **接受公网边界由网关清洗 `traceparent`**：若客户端直接携带 `traceparent: ...-01`（已采样标志）绕过边缘网关直连服务，`ParentBased` 会遵循父级采样决策。生产公网入口应由反向代理/LB 统一剥离或重写不可信外部 `traceparent`。

## 怎么验证

1. **静态依赖边界**：`make arch-check` 验证 `internal/tracex` 无业务 `internal/*` 依赖，除 `tracex` 外任何内部包均未引入 `otel/sdk` 或 `otel/exporters`，纯叶子包零 OTel 依赖。
2. **单测与集成测试（`make verify` & `make test-integration`）**：
   - `internal/tracex`：未配置端点 Noop、采样率 `1.0`/`0.0` + `ParentBased`、本地 `httptest` OTLP HTTP 真实导出与 `Shutdown` 刷盘。
   - `internal/logx` & `internal/httpx`：W3C `traceparent` 传播、跨组顶层 `trace_id`/`span_id`、探针与 `/metrics` 零 Span 断言、500/panic 标错与未匹配路由低基数命名。
   - `internal/pgdb`、`internal/storemetrics`、`internal/cache`、`internal/click`：`QueryTracer` 参数脱敏与未采样零开销、`singleflight` 领头与等待者 Span Link、点击批写 Span。
   - `internal/postgres` & `internal/job`：迁移 `018` 的 `up -> down -> up` 可逆性、各业务入队路径持久化 `traceparent`、`Runner` 创建独立 `SpanKindConsumer` + `Link` 关联源请求并把 `trace_id` 带入日志。
3. **真实三进程冒烟（`make selfcheck` / `scripts/smoke.sh`）**：启动本地 OTLP HTTP 接收桩，发送携带固定 `traceparent` 的创建短链请求，验证 `console` 日志、`jobs.traceparent`、`redirector` 日志、`worker` 日志中的 `trace_id` 串联，并在 `SIGTERM` 优雅停机后验证三进程均将 Trace 成功导出至 OTLP 桩；同时验证极简模式关闭追踪后仍正常工作。

## 后果

- **收益**：阶段 16（OpenTelemetry 追踪 + 日志 Phase C）及整份路线图全部闭环；在不破坏现有包隔离、极简零依赖模式和并发语义的前提下，打通了 HTTP → 缓存（含 `singleflight` 合并）→ PostgreSQL → Outbox 异步 Worker → 日志/审计表的全链路观测。
- **代价**：引入 `go.opentelemetry.io/otel` 系列依赖使 `go.mod` 与二进制体积略有增加；`jobs` 表新增 `traceparent` 列占用极少量存储。
