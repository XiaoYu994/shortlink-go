# 架构决策记录（ADR）

ADR 用来记录“为什么这样设计”，避免以后只看到代码不知道背景。

## 状态

- Proposed：提议中
- Accepted：已采纳
- Superseded：被新 ADR 取代
- Deprecated：不再推荐

## 命名

`NNNN-短标题.md`，例如 `0002-random-short-codes.md`。编号递增，不重复使用。

## 索引

| 编号 | 标题 | 状态 |
| --- | --- | --- |
| [0001](0001-api-versioning-and-route-namespace.md) | API 版本和路由命名空间 | Accepted |
| [0002](0002-random-short-codes.md) | 随机短码 | Accepted |
| [0003](0003-postgres-and-pool-config.md) | PostgreSQL 和连接池配置 | Accepted |
| [0004](0004-storage-package-layout.md) | 存储包的目录结构 | Accepted |
| [0005](0005-prometheus-before-tracing.md) | 先做 Prometheus 指标，追踪以后再说 | Accepted |
| [0006](0006-redis-cache-design.md) | Redis 读缓存的设计 | Accepted |
| [0007](0007-async-click-recording.md) | 异步批量记录点击 | Accepted |
| [0008](0008-cold-cache-read-path.md) | 冷缓存和长尾短码的读路径保护 | Accepted |
| [0009](0009-process-split-and-credentials.md) | 两个进程、一种租户身份 | Accepted |
| [0010](0010-app-level-referential-integrity.md) | 引用完整性放在应用层，不使用数据库外键 | Accepted |
| [0011](0011-link-crud-api.md) | 短链管理 API 的增删改查和排序 | Accepted |
| [0012](0012-click-analytics.md) | 点击统计：日聚合、客户端维度和统计接口 | Accepted |
| [0013](0013-pprof-on-loopback-listener.md) | 剖析端点放在单独的回环监听上 | Accepted |
| [0014](0014-per-tenant-rate-limit-and-quota.md) | 按租户限流和链接配额 | Accepted |
| [0015](0015-monthly-link-quota.md) | 链接配额按月计创建数，删除不返还 | Accepted |
| [0016](0016-link-states-and-takedown.md) | 链接状态和下架：过期、停用、封禁 | Accepted |
| [0017](0017-defer-click-write-optimization.md) | 点击写入暂不改，保留单写入者 | Accepted |
| [0018](0018-users-orgs-and-rbac.md) | 用户、组织和权限 | Accepted |
| [0019](0019-httpapi-package-evolution.md) | 暂不拆分 httpapi，先拆结构体再按依赖分叉拆包 | Accepted |
| [0020](0020-background-jobs-and-async-risk-scan.md) | 可靠后台任务与异步 URL 风控 | Accepted |
| [0021](0021-tenant-self-service-and-web-console.md) | 租户自助闭环与最小前端控制台 | Accepted |
| [0022](0022-postgres-query-and-lease-hardening.md) | PostgreSQL 查询路径、索引与后台任务租约加固 | Accepted |
| [0023](0023-audit-logs.md) | 组织操作审计日志 | Accepted |
| [0024](0024-process-boundary-and-ops-isolation.md) | 进程边界净化、独立运维端口、可信代理链与鉴权前 IP 限流 | Accepted |
| [0025](0025-reconcile-cache-async-purge-and-audit-hardening.md) | 缓存电平对账、异步统计清理与审计元数据 fail-closed | Accepted |
| [0026](0026-timeseries-partitioning-and-retention.md) | 时序统计表与审计日志的按月范围分区及保留期治理 | Accepted |
| [0027](0027-webhooks-and-async-invitation-email.md) | Webhook 订阅投递与异步成员邀请邮件 | Accepted |
| [0028](0028-postgres-rls-tenant-isolation.md) | PostgreSQL 行级安全（RLS）作为租户隔离第二道防线 | Accepted |
| [0029](0029-modular-feature-plugins.md) | 模块化特性插件架构：核心内核与可开关外围能力 | Accepted |
| [0030](0030-phone-otp-and-vertical-console-assembly.md) | 手机号短信验证码登录（Phone OTP）与 `cmd/console` 垂直特性装配演进 | Accepted |
| [0031](0031-cache-and-worker-assembly-simplification.md) | 缓存写路径与 Worker 模块装配做减法 | Accepted |
| [0032](0032-opentelemetry-tracing-and-log-correlation.md) | OpenTelemetry 分布式追踪与日志关联（Phase C） | Accepted |
| [0033](0033-job-claim-kind-filtering.md) | 按任务类型（Kind）隔离抢占，防止滚动发布旧版 Worker 误杀新任务 | Accepted |
| [0034](0034-layered-risk-scan-and-ai-judge.md) | 分层 URL 风控漏斗（规则 + Safe Browsing + HTML 净文本提取 + OpenAI 兼容 AI 研判）与死信告警治理 | Accepted |

## 模板

见 [template.md](template.md)。

参考：https://adr.github.io/
