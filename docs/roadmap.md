# 路线图

按顺序做，每个阶段结束跑 `make selfcheck`（没有 Docker 时至少跑 `make verify`）。

## 阶段 1：Base62（第 2、6 课）

- [x] `internal/idgen`：`EncodeBase62`、`DecodeBase62`（阶段 3 改为随机短码后不再使用，现已删除，见 git 历史）
- [x] 表驱动测试，包含非法字符和溢出

## 阶段 2：内存版服务（第 3、4、6 课）

- [x] `internal/link`：`Link`、`NewLink`、`ErrNotFound`、`ValidationError`、URL 校验
- [x] 内存版 Store（map 加 `sync.RWMutex`），放在 `internal/memorystore`
- [x] `httpapi`：`POST /api/v1/links` 和 `GET /r/{code}`，在 `Server` 里定义 `LinkStore` 接口
- [x] 用 httptest 测试：创建成功、非法 URL 返回 400、不存在的短码返回 404、跳转返回 302
- [x] 限制请求体大小；短码统一放在 `/r/` 下，和 `/healthz`、`/api` 路由天然隔离

## 阶段 3：PostgreSQL（第 7 课）

- [x] `go get github.com/jackc/pgx/v5`
- [x] `internal/postgres`：`Store` 实现 `LinkStore`
- [x] 错误处理：`sql.ErrNoRows` 转 `link.ErrNotFound`，`23505` 触发短码重试
- [x] 集成测试（没有 `TEST_DATABASE_URL` 时 `t.Skip`）
- [x] 短码改为随机生成（8 位 Base62），id 仅作内部主键
- [x] 表名按 PostgreSQL 习惯使用 `links`
- [x] 连接池参数从 `.env` 读取（`DB_MAX_OPEN_CONNS` 等）
- [x] 接入迁移工具（golang-migrate）
  - `cmd/migrate`，`make migrate`。版本记在 `schema_migrations`。每个版本一对 `NNN_name.up.sql` / `.down.sql`，不用 `IF NOT EXISTS`。compose 不再把 `migrations/` 挂进初始化目录。HTTP 进程启动时不迁移。没有选 goose：up 和 down 分文件，避免把整份 SQL 交给 psql 时把回滚也执行掉

## 阶段 4：配置、日志、中间件、优雅退出（第 8 课）

- [x] `internal/config`：`LoadConfig` 和测试
- [x] logging、recoverer、request_id 中间件和 `chain`，各自有测试
- [x] `signal.NotifyContext` 加 `Shutdown`，手动验证：请求处理到一半时按 Ctrl+C

## 阶段 5：指标、Redis 缓存与点击统计（第 9、10 课）

指标不在课程里，排在 Redis 前面做：缓存有没有效果要靠数字验收，第 10 课 `ClickRecorder` 的 dropped 计数也要有地方暴露。

- [x] `go get github.com/prometheus/client_golang`；`GET /metrics` 暴露指标（运维端点，不版本化，不对公网开放）
  - Registry 自己建、由 `main` 注入：测试里会创建很多个 `Server`，往同一个 Registry 重复注册同名指标，`MustRegister` 会 panic
  - HTTP 指标在 `NewServer` 里注册一次，`Routes()` 只组装中间件、不注册：现有测试会在同一个 `Server` 上多次调用 `Routes()`（`internal/httpapi/requestid_test.go`），在 `Routes()` 里注册第二次就 panic
  - 自建的 Registry 是空的，Go 运行时指标（`go_goroutines` 等）要自己注册
  - `/metrics` 用 `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})` 暴露，不用 `promhttp.Handler()`（后者默认抓 `DefaultGatherer`）
  - 与业务共用 `ADDR` 端口；“不对公网开放”由部署层保证（反向代理不转发或网络策略限制），应用本身不做鉴权
  - 抓取也走中间件链，会被 logging 记日志、被 HTTP 指标统计一次；暂时接受，噪声影响排查时再挪到链外
- [x] HTTP 指标中间件：按路由模式、方法、状态码记请求数和耗时直方图
  - label 只用取值有限的维度，短码、request_id 不能做 label
  - 路由模式用 `mux.Handler(r)` 查，未匹配的归到一个固定值；`mux.Handler` 对 405 也返回空 pattern，会归进同一个值，状态码 label 仍能区分
  - 外层中间件里读 `r.Pattern` 是空串：`timeout` 在指标中间件里层用 `WithContext` 换过 Request，`ServeMux` 只把 Pattern 写到那份副本上
  - 方法只保留 GET、HEAD、POST，其余记成 `other`。未匹配请求的方法来自客户端，原样做 label 会无限增长；HEAD 要保留是因为 GET 的模式同时匹配 HEAD
  - 中间件放在 `recoverer` 外面（和 `logging` 一样），用 status writer 捕获状态码；否则 panic 被写成 500 时指标会漏掉
  - 耗时直方图从 0.1ms 开始按 2 倍增长；默认桶从 5ms 开始，跳转的 p99 看不出缓存效果
- [x] 存储耗时：写一个实现 `LinkStore` 的装饰器包在 `postgres.Store` 外层，按操作记耗时和结果（`not_found`、`canceled`、`timeout` 都不算失败；耗时直方图带同样的结果标签）
  - 组合顺序固定为 `HTTP -> CachedStore -> 指标装饰器 -> postgres.Store`，存储耗时只算真正打到数据库的调用
- [x] 连接池指标：注册 `collectors.NewDBStatsCollector`，重点看 `go_sql_in_use_connections` 和 `go_sql_wait_duration_seconds_total`；同时注册 `collectors.NewProcessCollector` 暴露 `process_*`
- [x] `docker-compose.yml` 加 Prometheus 抓取 `/metrics`；Prometheus 在容器里抓宿主机的 app，target 用 `host.docker.internal:8080`（Linux 下加 `extra_hosts: host.docker.internal:host-gateway`），或把 app 也放进 compose；压测一轮，用 PromQL 记下没有缓存时跳转的 p50、p99 作为基线
  - 2026-11-12 基线（50 并发、30 秒、10s 预热、无缓存）：服务端 p50 3.2ms、p99 21.5ms；连接池峰值 10，详见 runbook
- [x] `go get github.com/redis/go-redis/v9`
  - `REDIS_URL` 必填；`internal/cache.Open` 在启动时 Ping，失败则进程退出
- [x] `CachedStore`：命中、负缓存、singleflight、Redis 故障降级
  - 正缓存 1h、负缓存 1m，value 是完整 `Link` 的 JSON；Redis 故障时回源数据库
  - 值格式、接口和测试策略见 [ADR 0006](adr/0006-redis-cache-design.md)
- [x] 缓存指标：命中、未命中、降级次数；同样的压测再跑一轮，和基线对比
  - `shortlink_cache_gets_total{result}`：`hit`（含负缓存）、`miss`（含坏数据）、`fallback`（Redis 读失败）。`shortlink_cache_writes_total{result}` 记录 `success`、`skipped`（`SET NX` 没写进去）、`error`。`shortlink_cache_operation_duration_seconds{operation,result}` 记录缓存操作耗时（`operation` 为 `get`、`set`）。请求取消不计入
  - 2026-11-12 缓存压测：客户端 19,077 req/s（基线 10,105）、服务端跳转 p50 1.4ms / p99 4.7ms（基线 3.2 / 21.5ms），数据库连接池零使用，详见 runbook
- [x] 缓存雪崩防护：正、负缓存 TTL 向下随机抖动最多 10%；Redis 读失败时限制回源并发，超出的快速返回 503
- [x] singleflight 的共享回源改用独立 ctx，领头请求取消不再连累等待者
  - 回源用 `context.WithoutCancel` 派生 ctx 并保留原 deadline；回源许可只包住数据库调用，见 ADR 0006
- [x] `ClickRecorder`：非阻塞投递、批量写出、关停时不丢数据
- [x] `ClickRecorder` 的 dropped 计数暴露到 `/metrics`
  - `shortlink_clicks_dropped_total`：缓冲满时加一。`shortlink_clicks_written_total{result}` 记录写出成功和失败的点击数。关停时排空 channel，受一份 `SHUTDOWN_TIMEOUT` 限制，再关数据库。设计见 [ADR 0007](adr/0007-async-click-recording.md)
- [x] 链接过期：`expires_at` 生效，查询过滤过期链接，缓存 TTL 取剩余时间
- [x] 用泛型的 `doTyped` 去掉类型断言
  - `singleflight.Group.Do` 仍返回 `any`。`doTyped` 把断言收在一处；回调先返回 `*link.Link`，nil 指针装进 `any` 后仍带着类型，断言得到零值
- [ ] 冷缓存/长尾短码的读路径保护（[ADR 0008](adr/0008-cold-cache-read-path.md)，Proposed）
  - 正常 cache miss 没有回源并发上限，只受 `DB_MAX_OPEN_CONNS`（默认 10）约束；大量不同短码时请求在连接池排队到 `REQUEST_TIMEOUT` 后 504
  - 计划：在 singleflight 内加正常 miss 的 bulkhead、独立回源超时、增加 shed 指标；先做多 key 压测再定参数

## 阶段 6：一个 SaaS 功能

- [x] 多租户加 API Key 鉴权（中间件加 context 传值）
  - 只包 `POST /api/v1/links`。`Authorization: Bearer slk_` 加 64 位十六进制，库存 SHA-256。租户放进 context，创建时写入 `links.tenant_id`。`/r/{code}`、`/healthz`、`/metrics` 不鉴权
  - 还没有签发入口。能写数据库的运维人员才能插入哈希；持有密钥的调用方不能再签发
- [ ] 拆分 `cmd/redirector` 和 `cmd/console`（[ADR 0009](adr/0009-process-split-and-credentials.md)）
  - 两个 `main`、两套部署，共用 PostgreSQL、Redis 和 `internal/`，进程之间不调用。redirector 只做公开跳转、缓存和点击；console 做管理 API
- [x] console 的管理员命令：创建租户并签发一把 API Key
  - `cmd/console key issue` 明文只打印一次。`expires_at` 为空表示不过期，`revoked_at` 表示吊销；过期或吊销仍返回 401。`key list` 只显示状态，`key revoke` 按密钥 ID 吊销。没有公开的签发 HTTP 接口
- [ ] （可选）缓存击穿的多副本方案：有了多个 redirector 副本、回源次数真的成为压力之后，比较 Redis 分布式锁（`SET NX PX` 加 Lua 释放）和逻辑过期加后台刷新，按回源次数和可接受的一致性取舍
- [ ] （可选）OpenTelemetry 追踪：拆分后两个进程互不调用，追踪看的是单个请求内部各环节的耗时，等指标解释不了慢请求时再加
- 用户操作短链的平台不在本阶段。人的登录和 API Key 以后都写成 context 里的同一个租户，见 ADR 0009
