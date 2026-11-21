# 路线图

按顺序做，每个阶段结束跑 `make selfcheck`（没有 Docker 时至少跑 `make verify`）。

这里只记做到哪了，一项一行。为什么这样做写在 [ADR](adr/README.md)，压测数据和操作步骤写在
[runbook](runbook.md)，实现时要注意的细节写在代码注释里；条目只链接过去，不重复内容。

## 阶段 1：Base62（第 2、6 课）

- [x] `internal/idgen`：`EncodeBase62`、`DecodeBase62`（阶段 3 改为随机短码后已删除，见 git 历史）
- [x] 表驱动测试，包含非法字符和溢出

## 阶段 2：内存版服务（第 3、4、6 课）

- [x] `internal/link`：`Link`、`NewLink`、`ErrNotFound`、`ValidationError`、URL 校验
- [x] 内存版 Store（map 加 `sync.RWMutex`），放在 `internal/memorystore`
- [x] `httpapi`：`POST /api/v1/links` 和 `GET /r/{code}`，在 `Server` 里定义 `LinkStore` 接口（[ADR 0001](adr/0001-api-versioning-and-route-namespace.md)）
- [x] 用 httptest 测试：创建成功、非法 URL 返回 400、不存在的短码返回 404、跳转返回 302
- [x] 限制请求体大小

## 阶段 3：PostgreSQL（第 7 课）

- [x] `internal/postgres`：`Store` 实现 `LinkStore`，连接池参数从环境变量读取（[ADR 0003](adr/0003-postgres-and-pool-config.md)、[ADR 0004](adr/0004-storage-package-layout.md)）
- [x] 错误处理：`sql.ErrNoRows` 转 `link.ErrNotFound`，唯一约束冲突触发短码重试
- [x] 集成测试（没有 `TEST_DATABASE_URL` 时 `t.Skip`）
- [x] 短码改为随机生成，id 仅作内部主键（[ADR 0002](adr/0002-random-short-codes.md)）
- [x] 接入迁移工具 golang-migrate：`cmd/migrate`、`make migrate`

## 阶段 4：配置、日志、中间件、优雅退出（第 8 课）

- [x] `internal/config`：`LoadConfig` 和测试
- [x] logging、recoverer、request_id 中间件和 `chain`，各自有测试
- [x] `signal.NotifyContext` 加 `Shutdown`，手动验证：请求处理到一半时按 Ctrl+C

## 阶段 5：指标、Redis 缓存与点击统计（第 9、10 课）

指标不在课程里，排在 Redis 前面做：缓存有没有效果要靠数字验收（[ADR 0005](adr/0005-prometheus-before-tracing.md)）。
指标名和 label 见 [api.md](api.md)。

- [x] `GET /metrics`：自建 Registry，由 `main` 注入
- [x] HTTP 指标中间件：按路由模式、方法、状态码记请求数和耗时直方图
- [x] 存储耗时：`internal/storemetrics` 装饰器，按操作记耗时和结果
- [x] 连接池和进程指标
- [x] Prometheus 抓取，压一轮无缓存基线（runbook“基线压测”）
- [x] `CachedStore`：命中、负缓存、singleflight、Redis 故障降级（[ADR 0006](adr/0006-redis-cache-design.md)）
- [x] 缓存指标；同样的压测再跑一轮和基线对比（runbook“缓存压测”）
- [x] 缓存雪崩防护：TTL 随机抖动；Redis 读失败时限制回源并发
- [x] singleflight 的共享回源改用独立 ctx，领头请求取消不再连累等待者
- [x] `ClickRecorder`：非阻塞投递、批量写出、关停时不丢数据，dropped 计数暴露到 `/metrics`（[ADR 0007](adr/0007-async-click-recording.md)）
- [x] 链接过期：`expires_at` 生效，查询过滤过期链接，缓存 TTL 取剩余时间
- [x] 用泛型的 `doTyped` 去掉 singleflight 的类型断言
- [x] 冷缓存/长尾短码的读路径保护（[ADR 0008](adr/0008-cold-cache-read-path.md)，runbook“多 key 压测”）

## 阶段 6：一个 SaaS 功能

- [x] 多租户加 API Key 鉴权（中间件加 context 传值）
- [x] 拆分 `cmd/redirector` 和 `cmd/console`（[ADR 0009](adr/0009-process-split-and-credentials.md)）
- [x] console 的管理员命令：创建租户，签发、查看和吊销 API Key（runbook“签发 API Key”）
- [ ] （可选）缓存击穿的多副本方案：有了多个 redirector 副本、回源次数真的成为压力之后，比较 Redis 分布式锁（`SET NX PX` 加 Lua 释放）和逻辑过期加后台刷新，按回源次数和可接受的一致性取舍
- [ ] （可选）OpenTelemetry 追踪：拆分后两个进程互不调用，追踪看的是单个请求内部各环节的耗时，等指标解释不了慢请求时再加
- 用户操作短链的平台不在本阶段。人的登录和 API Key 以后都写成 context 里的同一个租户，见 ADR 0009（已在阶段 11 落地规划）

## 阶段 7：短链管理和点击统计（第 11 课）

决策见 [ADR 0011](adr/0011-link-crud-api.md)、[ADR 0012](adr/0012-click-analytics.md)，接口见 [api.md](api.md)。

- [x] 迁移 003：三张日聚合表和列表排序索引
- [x] 管理 API：列表、详情、修改、删除；租户隔离、游标分页、过期链接管理可见
- [x] 缓存一致性：回源用 `SET NX`，更新/删除后无条件 `SET`；补并发语义和测试
- [x] 管理接口的存储实现和测试
- [x] 点击元数据：Referer host、设备分类、客户端分类、按天盐的访客哈希
- [x] `ClickRecorder` 和 `postgres.Store` 在同一事务里批量写日聚合表与 `links.clicks`
- [x] 统计接口 `GET /api/v1/links/{code}/stats`
- [x] 列表按历史点击和今日点击排序
- [x] 配置和文档同步
- [x] 集成测试和 `make smoke` 覆盖管理、统计和租户隔离；压一轮观察 dropped 和连接池（runbook“点击写入压测”）

## 阶段 8：性能剖析

- [x] redirector 的 pprof 端点：单独的回环监听，默认不开（[ADR 0013](adr/0013-pprof-on-loopback-listener.md)）
- [x] 点击写入的 benchmark 和剖析，定位单写入者上限的来源（runbook“点击写入剖析”）
- [x] 按剖析结论决定是否改点击写入：暂不改，写明重新评估的条件和到时的顺序（[ADR 0017](adr/0017-defer-click-write-optimization.md)）

## 阶段 9：限流和链接配额

决策见 [ADR 0014](adr/0014-per-tenant-rate-limit-and-quota.md)，操作见 [runbook](runbook.md)。

- [x] 每租户链接配额：`tenants.link_count` 和 `link_quota`，创建时原子占配额、删除释放，超限 403
- [x] 管理 API 按租户限流：进程内令牌桶，超限 429 带 `Retry-After`，空闲桶自动清理

## 阶段 10：配额口径和下架

决策见 [ADR 0015](adr/0015-monthly-link-quota.md)、[ADR 0016](adr/0016-link-states-and-takedown.md)，两篇都已采纳。

- [x] 配额改为每月创建数：迁移 005，创建语句里跨月重置，删除不返还
- [x] 链接状态：迁移 006，`Status` 和 `Servable`，跳转过滤停用和封禁
- [x] 租户停用和启用：`PATCH` 的 `disabled` 字段，响应带 `status`
- [x] 平台封禁和解封：`console link ban/unban`，被封禁的链接对租户只读
- [x] 下架的缓存一致性：封禁报告缓存写失败，管理写写完正缓存后复查

## 阶段 11：用户、组织和权限

决策见 [ADR 0018](adr/0018-users-orgs-and-rbac.md)；`httpapi` 的结构体与分包演进见 [ADR 0019](adr/0019-httpapi-package-evolution.md)（本阶段登录和成员 API 先在包内拆小结构体，不往 `Server` 堆字段）。

- [x] `Principal` 替换 context 里的 `tenant.Tenant`，`Authorizer` 接口加 map 实现，handler 改成 `Can` 检查；行为不变
- [x] 迁移 007：`users`、`user_identities`、`memberships`、`sessions`、`invitations`；`api_keys` 加 `scopes`、`created_by`
- [x] API Key scope：存量 Key 全部 scope，`console key issue --scope`
- [x] compose 加 Dex 和测试用户；`internal/identity` 的假 OIDC 服务（httptest）
- [ ] 登录流程：login 跳转（state、PKCE、nonce）、callback 校验 ID Token、建会话
- [ ] 会话鉴权中间件、`X-Org-ID` 成员校验、`CrossOriginProtection`；logout 和 `GET /api/v1/me`
- [ ] 邀请：`console member invite`，首次登录按已验证邮箱兑现
- [ ] 成员管理 API：列表、改角色、移出组织；立刻生效的集成测试
- [ ] `make smoke` 走一次 Dex 登录；配置和文档同步
- [ ] （可选）把 `Authorizer` 换成 Casbin 实现，验证策略模式接口边界
- [ ] （可选）接入 GitHub 作为第二个身份提供方，对比纯 OAuth 2.0 和 OIDC
- [ ] （可选）用 `testing/synctest` 重写限流器和 `ClickRecorder` 依赖时间的测试

## 阶段 12：审计日志（待立 ADR）

开工前先立 ADR，定事件口径、写入事务边界和查询接口；同步评估是否按 [ADR 0019](adr/0019-httpapi-package-evolution.md) 拆出 `internal/httpx` 并分离 redirector/console HTTP 包。

- [ ] 审计表和查询接口：记录操作人、动作、目标、时间和请求编号；与业务写在同一事务里提交

## 阶段 13：Webhook 与可靠后台任务（待立 ADR）

开工前先立 ADR，定 Outbox 表结构、投递重试与退避、签名和关停语义。

- [ ] 租户 Webhook 订阅配置与 HMAC 签名校验
- [ ] Outbox 模式 + worker pool：可靠投递事件、指数退避重试、优雅退出；顺带承载过期会话清理等后台任务

## 阶段 14：自定义域名（待立 ADR）

开工前先立 ADR，定域名校验、路由兼容（ADR 0001）、短码唯一约束和缓存 key 演进。

- [ ] 租户绑定自定义域名（DNS TXT 所有权校验）
- [ ] redirector 按 Host 路由，缓存 key 纳入域名，支持按域名隔离短码

## 阶段 15：套餐与用量计费（待立 ADR）

开工前先立 ADR，定套餐模型、用量计量（metering）和配额/限流的统一方式。

- [ ] 套餐（Free / Pro 等）关联月度配额、速率限制和功能开关（如自定义域名、Webhook）
- [ ] 用量计量和账单周期统计

## 阶段 16：租户隔离兜底与可观测性（待立 ADR）

开工前先立 ADR，定 RLS 连接上下文约定和追踪采样策略。

- [ ] PostgreSQL 行级安全（RLS）作为租户隔离的第二道防线（`SET LOCAL` 绑定当前租户）
- [ ] OpenTelemetry 追踪：串联跳转和管理请求里的缓存、singleflight 和数据库 span，日志带 `trace_id`
