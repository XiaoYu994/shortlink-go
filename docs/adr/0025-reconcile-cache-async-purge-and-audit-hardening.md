# 0025. 缓存电平对账、异步统计清理与审计元数据 fail-closed

- 状态：已采纳
- 日期：2026-11-24
- 相关：`0006`, `0010`, `0011`, `0016`, `0020`, `0023`, `0026`

## 背景

既有实现存在四处状态一致性与追溯完整性漏洞：

1. **租户自助写路径没有缓存失效重试**：`Update` / `Delete` 提交数据库后只同步调用尽力而为的 `writeCommitted`；一旦 Redis 短暂不可达或进程退出，已修改或已删除的短链最多仍能按旧正缓存跳转 1 小时。`Ban` / `Unban` 虽通过 `ErrNotUpdated` 重跑自愈，但 [`internal/risk/handler.go`](../../internal/risk/handler.go) 把 `cache.ErrNotUpdated` 返回给队列重试，会导致整条风控任务重跑并再次对外网目标 URL 发起 HTTP 抓取。
2. **删除短链同步删三张统计表拉长行锁**：`Delete` 在锁住 `links` 行后同步执行三条 `DELETE FROM link_*_daily WHERE link_id = $1`。面对累积大量维度或按天访客行的热点短链，会长时间持有连接和行锁。
3. **审计元数据存在静默降级兜底**：`internal/postgres/audit.go` 的 `resolveAuditMeta(ctx, fallback)` 在调用方遗漏 `audit.WithMeta` 时静默写入 `system` 或 `operator`，掩盖上游装配错误，导致追责记录失真。
4. **`X-Request-ID` 轻信客户端传入值**：`httpx.requestID` 直接复用合法的客户端 `X-Request-ID` 作为服务端 `request_id` 并落入 `audit_logs.request_id`，调用方可伪造固定编号污染日志关联与审计追踪。

## 决策

### 1. 电平触发的 `cache.reconcile` Outbox 对账任务

- 在 `Update`、`Delete`、`Ban`、`BanIfURL` 与 `Unban` 的同一数据库事务/多 CTE 语句内，原子入队一条 [`job.KindCacheReconcile`](../../internal/job/job.go)（`cache.reconcile`，载荷 `{"code": "..."}`）。
- **刻意不设 `dedup_key`**：`uq_jobs_dedup_active` 覆盖 `status IN ('pending', 'running')`。若设置 `dedup_key`，当第 1 条 `cache.reconcile` 处于 `running` 且刚读取旧状态、尚未写回 Redis 时，并发提交的新 `Unban` / `Update` 会被唯一索引静默吞掉，导致缓存长期停留在旧态。保留无去重多条任务+电平收敛（worker 按执行时刻数据库真值覆盖，并在正缓存写入后执行 `recheckAfterPositiveWrite`），并发写错序也必收敛到终态真值。
- 保留快速路径的同步 `writeCommitted`（`Update` / `Delete`）与 `writeCommittedResult`（`Ban` / `Unban`），正常情况下实现毫秒级可见，Outbox 负责 Redis 异常后的有重试兜底。
- [`risk/handler.go`](../../internal/risk/handler.go) 遇到 `cache.ErrNotUpdated` 时只记录 Warn 日志并返回 `nil`：数据库封禁与 `cache.reconcile` 已同事务提交，不再重跑外部 HTTP 抓取。

### 2. 删除短链异步分批清理统计行（`link.purge_stats`）

- [`postgres.Store.Delete`](../../internal/postgres/admin.go) 在事务内按 `FOR UPDATE` 校验并删除 `links` 主行后，仅同步写入审计日志、入队 `cache.reconcile` 与 [`job.KindLinkPurgeStats`](../../internal/job/job.go)（`link.purge_stats`，载荷 `{"link_id": id}`），不再同步扫描删除三张日统计表。
- 新增 `PurgeLinkStatsBatch(ctx, linkID, limit)`，由 worker 分批删除三张日表残留行；因为第 4 批将三张日表转为分区表（[ADR 0026](0026-timeseries-partitioning-and-retention.md)），跨分区 `ctid` 不唯一，**所有分批删除必须基于各表完整主键元组定位**（禁止 `WHERE ctid IN (...)`）。
- 删除 `links` 主行后，`GetByCode` / `Stats` / `RecordClicks` 均已对已删除链接不可见或过滤；对于保留期内很快随分区整体 `DROP TABLE` 回收的历史分区，异步清理只做渐进回收，不阻塞在线请求。

### 3. 审计上下文 `fail-closed`（`audit.ErrMissingMeta`）

- 在 [`internal/audit`](../../internal/audit/audit.go) 定义 `ErrMissingMeta` 与 `ValidateMeta(m)`。
- 移除 `resolveAuditMeta` 静默降级：[`internal/postgres/audit.go`](../../internal/postgres/audit.go) 改为 `requireAuditMeta(ctx)`（缺失或主体非法时直接返回包装 `audit.ErrMissingMeta` 的错误）；仅少数由方法参数显式传入执行者 `userID` 的成员/建组织/带发起人邀请路径与邀请兑现使用 `requireAuditMetaOrUser(ctx, userID)`，但绝不再伪造 `system` / `operator`。

### 4. 服务端权威签发 `X-Request-ID`

- [`httpx.requestID`](../../internal/httpx/requestid.go) 始终为每个 HTTP 请求服务端生成高熵 128 位十六进制 `request_id`，写入响应头 `X-Request-ID`、`logx` 上下文与 `audit_logs.request_id`。
- 若客户端请求携带符合安全字符集的 `X-Request-ID`，单独提取并记录为日志字段 `client_request_id`（[`logx.WithClientRequestID`](../../internal/logx/context.go)），既保留跨系统排查线索，又保证审计和内部链路标识不可伪造。

## 不做什么

- **不取消管理写路径的同步写缓存**：虽然已有 `cache.reconcile` 兜底，同步写穿依然能在正常工况下免除 worker 轮询窗口的短时陈旧读。
- **不给 `link.purge_stats` 加跨表外键级联**：坚持 [ADR 0010](0010-app-level-referential-integrity.md) 应用层完整性约束，不把大表级联锁定交回数据库锁管理器。

## 验证标准

1. `internal/postgres` 集成测试覆盖：Redis 不可用时执行 `Update`/`Delete` 仍原子入队 `cache.reconcile`，Redis 恢复后 worker 执行 `Reconcile` 收敛到最新 URL/负缓存；`Ban` -> `Unban` 乱序执行后 `Reconcile` 收敛到数据库真值。
2. `Delete` 后立即验证 `links` 已删除且入队 `link.purge_stats`，执行 `PurgeLinkStatsBatch` 后三张日表残余行归零，并发点击与删除仍无死锁。
3. 未注入 `audit.WithMeta` 的 `Create`/`Update`/`Delete`/`Ban`/`BanIfURL`/`Unban`/`SetTenantLimits`/`RevokeAPIKey` 直接返回 `errors.Is(err, audit.ErrMissingMeta)`。
4. `httpx` 与 `console` 单测确认客户端伪造 `X-Request-ID` 不会覆盖服务端签发编号和审计记录 `request_id`，合法客户端编号保留在 `client_request_id` 日志字段。
