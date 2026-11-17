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
- 用户操作短链的平台不在本阶段。人的登录和 API Key 以后都写成 context 里的同一个租户，见 ADR 0009

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
