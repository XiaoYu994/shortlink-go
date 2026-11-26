# 0031. 缓存写路径与 Worker 模块装配做减法

- 状态：Accepted
- 日期：2026-11-26
- 相关：`0006`, `0008`, `0011`, `0016`, `0025`, `0029`, `0030`

## 背景

复盘全部压测、故障演练记录（[runbook](../runbook.md)）与代码演化后，发现仓库中存在两处把**纸面假设性 P1 的重型防御**直接揉进核心主干、显著拉高阅读和理解成本的设计：

1. **缓存层写路径多层叠甲（[ADR 0016](0016-link-states-and-takedown.md)、[ADR 0025](0025-reconcile-cache-async-purge-and-audit-hardening.md)）**：
   - 跳转读路径的保护（`singleflight`、`missSlots`/`fallbackSlots` bulkhead、`CACHE_FETCH_TIMEOUT`、负缓存、TTL 抖动、`DialerRetries=1`）全部经过多轮压测与 3 副本故障演练验证，是高价值核心；
   - 但围绕“1 小时正缓存 TTL 下，写库瞬间 Redis 闪断或微秒级 `PATCH` 与 `Ban` 交错导致旧值留存 1 小时”的纸面假设，先后叠加了两层重型机制：`Create`/`Update`/`Unban` 每次写正缓存后再同步回查数据库（`recheckAfterPositiveWrite`，报错时甚至反向写负缓存），以及同事务无去重入队 `cache.reconcile` Outbox 任务由 worker 异步再查库、再写缓存、再复查。一次普通更新最多产生多轮额外 DB/Redis 往返，核心缓存文件膨胀到近 800 行，阅读主干时心智负担极重。
2. **后台 Worker 装配框架化过重（[ADR 0029](0029-modular-feature-plugins.md) 收尾加固）**：
   - `cmd/console/workermodule.go` 仅服务于同仓 `package main` 内固定装配的 6 个私有模块，却叠加了泛型去重校验器 `validateUniqueEntry[V any]`、延迟错误字段 `r.err`、双 map 运行时防重名、专用的 `shortlink_jobs_feature_disabled_skipped_total` 指标（含失败不污染 registry 的零值预热）以及配套 Prometheus 漂移告警规则 `ShortlinkJobsFeatureDisabledSkipped`——而 `job.Runner` 本身已提供 `shortlink_jobs_processed_total{kind, result="skipped"}` 与结构化日志。
   - 同期 `cmd/console/features.go` 为极罕见的“配了 `WEBHOOK_SECRET_KEY` 但未配任何登录方式”启动日志单独拆出 `webhooks_fanout`/`webhooks_api` 状态分支，引入额外维护噪音。

按本仓库新沉淀的「用事实说话、优先减法解」原则（[AGENTS.md](../../AGENTS.md)），对这两处核心主干做精准减法。

## 决策

### 1. 缓存层：用“缩短正缓存 TTL 到 5 分钟”替代写后复查与 Outbox 对账

- 将 `internal/cache` 的 `positiveTTL` 从 `1 * time.Hour` 缩短为 **`5 * time.Minute`**（保留 10% 随机抖动与短链剩余寿命取小值）。
  - 对热链的影响：每条热链每 5 分钟最多额外回源 1 次（且经 `singleflight` 合并），在单机 PG 10k QPS 基线下可忽略不计，命中率几乎无感；
  - 对一致性的收益：无论遇到管理写瞬间 Redis 闪断，还是极低概率的并发修改与下架交错，最坏陈旧窗口都自然收窄到 **≤ 5 分钟**（且 Redis 宕机期间 `redirector` 本就会降级回源读数据库最新状态）。
- **移除 `recheckAfterPositiveWrite`**（[ADR 0016](0016-link-states-and-takedown.md) 的写后回查决策由本 ADR 取代）：
  - `Create`、`Update`、`Delete` 提交数据库后直接按 `Servable` 同步覆盖写正/负缓存（保留 `context.WithoutCancel` + `1s` 超时），失败只记日志与指标；
  - `Ban`、`BanIfURL`、`Unban` 同样只做一次同步覆盖写，写失败返回 `cache.ErrNotUpdated`：平台 CLI 提示运维“旧缓存最长还能跳转 5 分钟，重跑命令幂等自愈”，异步风控记 Warn 日志返回 `nil`（不重抓外部 URL）。
- **移除 `cache.reconcile` Outbox 任务全链路**（[ADR 0025](0025-reconcile-cache-async-purge-and-audit-hardening.md) 第 1 节由本 ADR 取代，第 2–4 节异步统计清理、审计 `fail-closed` 与服务端 `X-Request-ID` 保持不变）：
  - 删除 `job.KindCacheReconcile`、`postgres.CacheReconcileHook`、`cache.Reader.Reconcile` 与 `cmd/console/feature_cache.go` 中的 `cacheWorkerModule`（`feature_cache.go` 仅保留 `openRedisCache`）。
- **100% 保留全部压测验证过的核心保护**：`singleflight`、`missSlots`/`fallbackSlots` bulkhead、`CACHE_FETCH_TIMEOUT`、负缓存（1 分钟）、TTL 抖动、回源 `SET NX` vs 管理写无条件 `SET`、`DialerRetries=1` 与入口/出口 `ctx.Err()` 拦截。

### 2. Worker 装配：保留垂直特性边界与 `job.Skip`，砍掉自研校验框架与冗余告警

- 保留 [ADR 0030](0030-phone-otp-and-vertical-console-assembly.md) 的 `feature_*.go` 垂直拆分结构、`WorkerModule` / `WorkerRegistrar` 接口及关闭特性时通过 `HandleDisabled` 返回 `job.Skip` 的收尾机制（避免残留任务打入 `dead` 队列）。
- 精简 `cmd/console/workermodule.go`：
  - 移除 `validateUniqueEntry[V any]`、延迟错误字段 `r.err`、运行时双 map 查重、零值指标预热与 `shortlink_jobs_feature_disabled_skipped_total` 指标；关闭特性跳过直接复用 `job.Runner` 已有的 `shortlink_jobs_processed_total{kind, result="skipped"}` 指标与结构化日志；
  - 同步删除 `deploy/alerts.yml` 中的 `ShortlinkJobsFeatureDisabledSkipped` 规则；
  - `assembleWorkerModules` 退回清晰直白的 40 行装配函数，同仓 6 个固定模块的正确性由启动装配单测守门。
- 同步精简 `cmd/console/features.go`：移除 `StoreFeatures.Cache`（不再有缓存事务钩子）以及仅为极端启动日志拆分的 `webhooks_fanout`/`webhooks_api` 状态分支。

## 不做什么（明确接受的风险边界）

1. **明确接受“Redis 写失败或并发下架交错时最多 5 分钟陈旧读”**：不再引入写后回查 DB、Outbox 缓存对账或 Redis Lua 版本号校验；若未来产品升级为“封禁必须 1 秒内全球强失效”，再通过实测评估独立方案（如缩短 TTL 至 30s 或事件总线广播失效）。
2. **明确接受私有装配代码不做运行时重复名校验框架**：`cmd/console` 内的 `WorkerModule` 为同仓私有枚举装配，非第三方插件宿主；靠单测校验完整装配即可，不写泛型注册校验器。
3. **不顺带重构已隔离良好的独立叶子包**：`internal/ratelimit`、`internal/safehttp`、`internal/console/request.go` 与 PostgreSQL RLS（[ADR 0028](0028-postgres-rls-tenant-isolation.md)）均隔离在清晰边界内、不影响主干可读性，不作无收益返工。

## 怎么验证

1. `internal/cache` 单元与集成测试覆盖：`positiveTTL = 5m`、剩余寿命取小值、回源 `SET NX` 不覆盖管理写、删除/停用/封禁写负缓存、解封按 `Servable` 写正/负缓存、Redis 故障时 `Ban`/`Unban` 返回 `cache.ErrNotUpdated` 且 `Create`/`Update`/`Unban` 不再触发额外 `GetByCode` 回查。
2. `internal/postgres` 与 `internal/risk` 回归测试确认：删除 `CacheReconcileHook` 后核心写事务零多余 `cache.reconcile` 入队，`BanIfURL` 遇 `cache.ErrNotUpdated` 仍返回 `nil` 不重抓外网 URL。
3. `cmd/console` 单测确认：全部开启与全部关闭特性时各 `WorkerModule` 正确装配，关闭特性任务经真实 `job.Runner` 执行后记为 `done` 且 `shortlink_jobs_processed_total{result="skipped"}` 递增。
4. `make selfcheck`（含 `verify`、`test-integration`、真实双进程 + 极简模式 `smoke`）全绿。

## 后果

- **收益**：`internal/cache/store.go` 与 `cmd/console/workermodule.go` 生产代码显著瘦身，去除约 1000+ 行冗余防御与配套测试；缓存读/写路径心智模型回归清晰（压测保护读路径 + 5 分钟短 TTL 写穿），`Update`/`Create`/`Unban` 省去多余数据库回查。
- **代价**：若线上历史库在滚动升级前仍存留旧版 `cache.reconcile` 待执行任务，升级后旧任务会被 `job.Runner` 标为未注册错误并转入 `dead`（学习/单库部署重启或清表前可忽略；若需平滑迁移可在发布窗口保留一个版本跳过该 kind）。
