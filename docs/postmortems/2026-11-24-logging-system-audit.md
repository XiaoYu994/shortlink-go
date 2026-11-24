# 日志体系与可观测性架构审查

- 日期：2026-11-24
- 来源：架构审查（对照 Stripe Canonical Log Lines、Caddy / Envoy 数据面采样、Prometheus / Kubernetes、Go `log/slog` 与 OpenTelemetry 最佳实践）
- 状态：Phase A / B 已落地（Phase C 待在路线图 [阶段 16](../roadmap.md#阶段-16租户隔离兜底与可观测性待立-adr) 与 OpenTelemetry 合流）
- 关联：[ADR 0005](../adr/0005-prometheus-before-tracing.md)、[ADR 0023](../adr/0023-audit-logs.md)、[internal/httpx/logging.go](../../internal/httpx/logging.go)、[internal/httpx/requestid.go](../../internal/httpx/requestid.go)、[internal/config/config.go](../../internal/config/config.go)

## 现状评估

项目当前基于 Go 标准库 `log/slog` 构建日志链路，基础底座已有明确设计纪律：

- **格式与级别可控**：`internal/config/config.go`（`423–432` 行）通过 `BaseConfig.NewLogger` 统一支持 `LOG_LEVEL` 与 `LOG_FORMAT`（`text`/`json`）。
- **请求级安全基线清晰**：`internal/httpx/logging.go`（`27–42` 行）跳过 `/metrics` 避免 Prometheus 抓取淹没业务日志，并故意只记录 `r.URL.Path` 而不记查询串；`internal/httpx/requestid.go`（`55–107` 行）对调用方传入的 `X-Request-ID` 做字符集与长度校验，防止日志注入。
- **错误级联较克制**：`internal/httpx/response.go`（`66–85` 行）将 `context.DeadlineExceeded` 与 `temporaryError`（回源过载）降为 `WarnContext`，客户端取消不误报 `Error`，与真正的服务端故障（500）分开级别。

但在对照高并发 Go 生产项目与云原生可观测性体系后，当前日志架构存在 **3 个 P0 级上下文/性能短板、3 个 P1 级运维隐患与 1 个 P2 级追踪预留缺失**。

## 问题清单与源码证据

### 1. [P0] `requestIDHandler` 仅挂在 `httpx.Server`，`cache.Store` 与异步风控链路上下文断链

- **现象**：在一次带 `X-Request-ID` 的请求中，外层访问日志和 HTTP handler 错误日志带有 `request_id`，但同一请求在缓存层写回失败或复查失败时的 `WarnContext` 日志却丢失了 `request_id`；`console worker` 执行任务时，下游 `risk.VerdictCache` 与 `cache.Store` 的告警同样找不到归属的 `job_id` 与 `tenant_id`。
- **源码证据**：
  - `internal/httpx/server.go:56-58` 只给 `httpx.Server.logger` 包了 `requestIDHandler`；
  - `cmd/redirector/main.go:46,98-103` 与 `cmd/console/serve.go:32,66-70` 传给 `cache.New` 的是未包装的原始 `logger`（`internal/cache/store.go:246,338,521,626` 均打在这个裸 `logger` 上）；
  - `internal/job/runner.go:381-437` 仅在 `runner` 自身日志里手动传入 `"job_id"` 和 `"kind"`，未将 `job_id` / `tenant_id` 注入传给 `handler(jobCtx, j)` 的 `jobCtx`，也未派生子 logger（导致 `internal/risk/cache.go:147,174` 的 Redis 降级警告无法关联具体任务和租户）。
- **根因**：`slog.Handler` 的 context 提取链只在 HTTP 底座构造时局部包裹，而非在进程根 logger 创建阶段统一装配。

### 2. [P0] 访问日志停留在 `authenticate` 外层，缺少业务维度且未按状态码分级

- **现象**：`internal/httpx/logging.go:36-41` 仅输出 `method`、`path`、`status`、`duration` 四个字段。
- **源码证据**：
  - `logging` 中间件位于 `internal/httpx/server.go:99-107` 的通用链上，而 `authenticate`、`rateLimit` 与路由分发在其内层。访问日志既不知道命中的路由模板（如 `/api/v1/links/{code}` 对比实际高基数短码 `/r/AbC123xy`），也不知道 `tenant_id`、`actor_kind`/`actor_id`、限流结果或缓存命中情况；
  - `internal/httpx/metrics.go:103-109` 已有 `routePattern(mux, r)` 计算路由模板，但访问日志未复用；
  - 未记录 `bytes_written` 与 `status` 分级（5xx / 4xx / 2xx-3xx 一律打 `Info`）。
- **影响**：排障“某个租户慢请求”或“某时段 429/5xx 突发”时，只能先拿外部传来的 `request_id` 去捞零散错误日志，无法直接对访问日志按 `tenant_id`、`route`、`cache_result` 聚合下钻。

### 3. [P0] `redirector` 数据面全量同步打 `Info` 访问日志与探针噪音

- **现象**：每次 `GET /r/{code}`（302 跳转、高频 404 扫描）以及编排器/健康检查每数秒一次的 `/healthz`、`/readyz` 请求（`logging.go:27` 只跳过了 `/metrics`）都会以 `Info` 级别同步写入加锁的 `os.Stdout`。
- **源码证据**：`internal/httpx/logging.go:27-42`。
- **影响**：高并发跳转场景下，热路径每个 302 都要经历 `slog` 字段组装、JSON/Text 格式化与 stdout mutex 写锁竞争；同时探活 200 和正常 302 会冲淡真正需要人工关注的 5xx、慢请求与异常扫描流量。

### 4. [P1] 客户端主动断开后的写响应失败被记为 `Error`

- **现象**：客户端超时或提前断开连接后，`w.Write(data)` 返回 `EPIPE` / `ECONNRESET` 时，服务端无条件打出 `Error` 日志，容易引发假阳性告警。
- **源码证据**：`internal/httpx/response.go:39-41`（`WriteJSON` 写出失败直接 `logger.ErrorContext(ctx, "write JSON response", "err", err)`，未结合 `ctx.Err()` / `syscall.EPIPE` / `syscall.ECONNRESET` 降为 `Debug`）；相比之下，读入口 `IsClientGone`（`16-22` 行）已按 `Debug` 处理。

### 5. [P1] 启动 fatal 与 CLI 辅日志格式割裂、多进程缺标识、`duration` 序列化不直观

- **源码证据**：
  - `cmd/redirector/main.go:36` 启动失败调用全局默认 `slog.Error("fatal", "err", err)`（未应用配置的 JSON handler，直接以 text 输出），`cmd/console/main.go:45,62,70` 直接 `fmt.Fprintf(os.Stderr, ...)`，而 `cmd/console/main.go:159` 给 `cache.New` 硬编码了 `slog.NewTextHandler(os.Stderr, nil)`；
  - `BaseConfig.NewLogger` 未自动附加 `service`（`redirector` / `console` / `console-worker`）默认属性，多容器汇入同一日志平台（Loki/ELK）后需要额外靠采集标签区分来源；
  - `internal/httpx/logging.go:40` 直接传 `time.Since(start)`（`time.Duration`），`slog.NewJSONHandler` 默认将其输出为纳秒整数（如 `1423589`），人工阅读及日志查询不如 `duration_ms`（浮点毫秒）直观。

### 6. [P1] 缺少运行时动态调级（`slog.LevelVar`）与敏感数据脱敏防线

- **源码证据**：
  - `internal/config/config.go:70,373-377,424` 在启动时一次性解析静态 `slog.Level`，线上遇到疑难故障时无法在不重启进程（不破坏连接/缓存现场）的前提下临时把日志级别调成 `debug`；
  - 仓库尚无基于 `slog.LogValuer` 或 `HandlerOptions.ReplaceAttr` 的全局脱敏护栏，当前主要靠人工规避“不要打印 `Authorization` / `sl_session` / `DATABASE_URL`”。一旦未来新增 `Debug` 排障日志误打结构体或 Header，极易发生凭证泄漏。

### 7. [P2] 尚未建立统一的 `ContextHandler` 收口，预留阶段 16 OpenTelemetry 联动位

- **源码证据**：`internal/httpx/requestid.go:90-107` 仅提取了 `request_id`。未来阶段 16 引入 OpenTelemetry 后，需要同步把 `trace_id`、`span_id` 注入所有业务与基础组件日志；如果当前先收口统一的 `ContextHandler`，后续只需在一处追加提取逻辑，业务代码零侵入。

## 优秀项目实践对照

| 维度 | 业界优秀实践（代表项目） | 本项目当前状态 | 差距与启发 |
| --- | --- | --- | --- |
| **结构化宽事件** | **Stripe（Canonical Log Lines）**：每次请求只在结束时写一条携带路由、租户、用户、缓存、限流与错误码的宽日志 | `httpx/logging` 与外层脱节，关键维度散落在或完全缺失于 access log | 引入请求级 Mutable Attrs 收集槽，由内层鉴权/限流/存储补充属性，结束时统一写出 |
| **数据面性能与采样** | **Envoy / Caddy**：区分控制面与数据面；健康检查 200 降为 Debug/静默；支持头部/尾部采样（保留全部 `5xx`、慢请求，正常 `2xx/3xx` 按比例采样） | `redirector` 对每条 `302` 及 `/healthz`/`/readyz` 全量 `Info` 同步加锁写 stdout | `/healthz`/`/readyz` 仅在非 2xx 时写 Warn；`redirector` 预留采样 + 慢请求/异常全保策略 |
| **跨层上下文注入** | **Uber Zap / Go `slog` `ContextHandler` 模式**：在进程根 logger 层统一挂载从 `ctx` 自动提取多键的 Handler | `requestIDHandler` 只挂在 `httpx.Server` 上，裸 `logger` 传给了 `cache` 和 `job` | 将多字段 `ContextHandler`（`request_id`、`job_id`、`tenant_id`、未来 `trace_id`）上移到根 logger 构造器 |
| **动态调级** | **Prometheus / Kubernetes（`/-/reload` 或运维端点 + `slog.LevelVar`）**：运行时热切换局部或全局日志级别 | 仅启动读取静态环境变量 `LOG_LEVEL` | 使用 `slog.LevelVar`，可在 `PPROF_ADDR` 回环管理监听上安全暴露调级入口 |
| **脱敏安全兜底** | **Go 1.21+ `slog.LogValuer` + `ReplaceAttr`**：对令牌、Config 密码、特定 Header 键（`authorization`、`cookie`、`password`）自动掩码 | 靠人工约束不在调用点传入明文 | 对配置结构体实现 `LogValuer`（掩码 DSN/ClientSecret/Salt），在 `ReplaceAttr` 中统一兜底 |
| **可观测性三支柱协同** | **OpenTelemetry + Loki/Grafana**：日志带 `trace_id`/`span_id`；审计事件与运行日志严格分库存储 | 已实现业务审计表 `audit_logs`（通过 `request_id` 关联）与 Prometheus 指标 | 统一三层边界，避免把运维日志当审计、或把高 QPS 访问日志写进 PostgreSQL |

## 三层可观测性边界与演进路线

保持清晰的职责切分，避免日志体系膨胀失控：

1. **安全/业务审计日志（`audit_logs` 表，阶段 14 已落地）**：只记录会改变持久化状态的组织/短链/成员/密钥行为，追求事务强一致与长期可追溯。
2. **指标与直方图（Prometheus `/metrics`，阶段 5 已落地）**：承载高频聚合统计（QPS、P99、缓存命中率、队列堆积、连接池水位）。
3. **运行与访问日志（`slog` 结构化流）**：负责故障排障、慢请求宽事件与异步任务轨迹，按下面三步渐进演进：
   - **Phase A（快速止血，无需改架构，已落地）**：
     - 抽出轻量的根日志上下文包装并放到 `BaseConfig.NewLogger` 统一装配，绑定固定 `service`（`redirector` / `console` / `console-worker`）和 `slog.LevelVar`，修复 `cache.Store` 与 `worker`/`risk` 丢失关联 ID 的问题；
     - 在 JSON 输出中规范 `duration_ms`；启动 fatal 错误沿用环境变量配置的 handler（`LOG_FORMAT`）；
     - `/healthz` 和 `/readyz` 成功（200）访问日志降为 `Debug`；`WriteJSON` 遇到客户端断连（`EPIPE`/`ECONNRESET`/`context.Canceled`）降为 `Debug`；
     - 配置及密钥类型实现 `slog.LogValuer` 脱敏。
   - **Phase B（高并发与可分析性增强，已落地）**：
     - 本地 `text` 格式接入 `github.com/lmittmann/tint` 彩色可读终端输出（自动识别 TTY / `NO_COLOR`），`json` 保持严格结构化；
     - 合并 `httpx` 日志与指标中间件，升级为 Canonical Log Line（`logx.RequestEvent`）：复用 `routePattern`、补充 `bytes` / `ip`，收集 `tenant_id`、`actor`、`cache`、`retry_after`、`err_msg`，并按 `>=500 -> Error`、`>=400` / 慢请求 `-> Warn`、`2xx/3xx` 与 `redirector` 404 `-> Info` 分级；
     - 为 `redirector` 增加 `ACCESS_LOG_SAMPLE_RATE` 与 `ACCESS_LOG_SLOW_THRESHOLD` 慢请求/异常全保采样策略。
   - **Phase C（与阶段 16 OpenTelemetry 合流）**：
     - 在统一 `ContextHandler` 内接入 `trace.SpanFromContext(ctx)`，自动输出 `trace_id`、`span_id`，配合已有 `request_id`，打通 **审计日志 <-> 进程结构化日志 <-> 分布式链路追踪**。

## 防再犯护栏

- 后续新增任何异步后台组件或存储装饰器时，禁止重新用未包 `ContextHandler` 的裸 `logger` 或硬编码 `NewTextHandler(os.Stderr, nil)`；
- 所有配置、外部凭证与会话对象在定义阶段默认补齐 `slog.LogValuer` 掩码实现与单测断言（验证 `%+v` / JSON log 不输出密钥或 DSN 密码）；
- 若在回环管理端口开放动态 `LevelVar` 调级，沿用 [ADR 0013](../adr/0013-pprof-on-loopback-listener.md) 护栏，严禁暴露在业务公网监听端口上。
