# 架构

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 目录结构

```txt
shortlink/
├── cmd/server/          # HTTP 入口：创建短链和跳转
├── cmd/console/         # 租户和 API Key 的运维命令
├── cmd/migrate/         # 数据库迁移。启动 HTTP 进程时不执行
├── internal/
│   ├── httpapi/         # handler、路由、中间件；定义 LinkStore 接口
│   ├── link/            # 领域模型：Link、NewLink、错误、URL 校验
│   ├── tenant/          # 租户和 API Key 的明文格式、哈希
│   ├── idgen/           # 随机短码生成
│   ├── memorystore/     # 内存存储，仅测试和本地开发
│   ├── postgres/        # PostgreSQL 存储，生产使用
│   ├── storemetrics/    # LinkStore 的指标装饰器
│   ├── cache/           # Redis 客户端和读缓存（CachedStore）
│   ├── click/           # 异步批量记录点击（ClickRecorder）
│   └── config/          # 配置加载
├── migrations/          # golang-migrate 的 up/down SQL
├── deploy/              # 本地 Prometheus 配置
├── scripts/             # 测试库准备和冒烟测试脚本
├── .github/workflows/   # CI：静态检查、集成测试、冒烟
├── docs/                # 架构、API、运行手册、路线图、ADR
└── Makefile
```

## 依赖方向

```txt
cmd/server   -> httpapi, postgres, storemetrics, cache, click, config
cmd/console  -> postgres
cmd/migrate  -> golang-migrate
config       -> postgres
httpapi      -> link, idgen, tenant
postgres     -> link, idgen, tenant
memorystore  -> link, idgen
storemetrics -> link
cache        -> link
click        -> 标准库、Prometheus
link         -> 标准库
idgen        -> 标准库
```

`LinkStore` 接口定义在 `httpapi`（使用方），内存版和 PostgreSQL 版都隐式实现它。`link` 不依赖存储和 HTTP。`click` 自己定义写点击的接口，`postgres.Store` 隐式实现它。管理 API 的 `APIKeyAuthenticator` 也定义在 `httpapi`，由同一个 `postgres.Store` 实现；缓存不参与鉴权。

## 请求流程

### 创建短链

```txt
POST /api/v1/links
  -> requestID
  -> logging
  -> metrics
  -> recoverer
  -> timeout
  -> authenticate：Bearer API Key，哈希换租户，放进 context
  -> handleCreate
  -> 读取并限制请求体
  -> json.Unmarshal
  -> link.NewLink 校验
  -> TenantID 取自 context
  -> store.Create 生成随机短码并写库
  -> 201 + Location: /r/{code}
```

### 跳转

```txt
GET /r/{code}
  -> requestID
  -> logging
  -> metrics
  -> recoverer
  -> timeout
  -> handleRedirect
  -> 校验短码格式（8 位 Base62，不合法直接 404）
  -> store.GetByCode（cache -> storemetrics -> postgres）
  -> 客户端已断开则直接返回
  -> 仅 GET 调用 clicks.Record(id)（非阻塞；缓冲满只记 dropped，不影响 302）
  -> 302 Location: 原始 URL
```

GET 模式同时匹配 HEAD。HEAD 返回同样的 302，但不记点击。

`chain` 里靠前的中间件在外层。`requestID` 确定编号，写入响应头 `X-Request-ID` 和请求 context。`logging` 记录方法、路径、状态码和耗时；`metrics` 按路由模式、方法和状态码记录请求数和耗时，方法只保留 GET、HEAD、POST，其余记成 `other`。它放在 `recoverer` 外面，panic 被写成 500 后也能记到这个状态码；`recoverer` 把 handler 的 panic 写成 500。带 context 的日志会自动加上 `request_id`。`http.ErrAbortHandler` 会继续抛出，那是 `net/http` 用来中止当前连接的哨兵。

`timeout` 在最里层，给请求 context 加处理时限。到点后 handler 里的数据库查询被取消（pgx 会中断正在执行的 SQL），handler 返回 504。`http.Server` 的 `ReadTimeout`、`WriteTimeout` 只管连接的读写，不会取消 handler 的 context，处理时限只能靠这一层。这两个连接超时都是 `REQUEST_TIMEOUT` 加 5 秒：读超时包住请求体，写超时留给 504 的写出。

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发。
- `internal/postgres`：`database/sql` + pgx，生产使用。
- `internal/storemetrics`：实现 `LinkStore` 的装饰器，按操作记录耗时和结果。`not_found`、`canceled`、`timeout` 都不算失败，只有其余错误记成 `error`。耗时直方图带同样的结果标签。组装顺序是 `HTTP -> cache -> storemetrics -> postgres.Store`，缓存命中不会计入存储耗时。
- `internal/cache`：`GetByCode` 的 Redis 读缓存装饰器。key 是 `shortlink:link:<code>`，value 是完整 `link.Link` 的 JSON；正缓存 TTL 取剩余寿命和 1 小时里更短的那个，没有到期时间时是 1 小时，负缓存（JSON `null`）TTL 1 分钟。命中到已经过期的正缓存时删掉 key 并返回找不到。URL 为空或 Code 对不上的 JSON 也删掉，按未命中回源。并发未命中用 singleflight 合并回源；普通未命中和 Redis 读失败各有一条回源限流，超过许可立即返回 503，不排队。普通未命中的许可数由 `CACHE_MISS_CONCURRENCY` 控制（默认 8），Redis 读失败固定为 8，两条路径分开计数。回源使用独立的 `CACHE_FETCH_TIMEOUT`（默认 1s），不再借用 `REQUEST_TIMEOUT`；`singleflight` 的共享回源不跟随领头请求取消。被限流的请求记在 `shortlink_cache_shed_total{path="miss|fallback"}`，按请求计数，等待者也算。创建成功后写穿正缓存，负缓存用 `SET NX`，避免创建和查找并发时后到的负缓存覆盖正缓存。查找结果记在 `shortlink_cache_gets_total`（`hit` 含负缓存、`miss`、`fallback`），写缓存结果记在 `shortlink_cache_writes_total`（`success`、`skipped`、`error`；`skipped` 表示 `SET NX` 没写进去），缓存操作耗时记在 `shortlink_cache_operation_duration_seconds`（`operation` 为 `get`、`set`）。正、负缓存 TTL 向下随机抖动最多 10%。缓存设计见 [ADR 0006](adr/0006-redis-cache-design.md)，回源保护见 [ADR 0008](adr/0008-cold-cache-read-path.md)。
- `internal/click`：GET 跳转成功后把链接 ID 放进缓冲 channel，一条 goroutine 按 100 条或 1 秒把点击加成一批，直接写 `postgres.Store`。缓冲满时 `shortlink_clicks_dropped_total` 加一；写出结果记在 `shortlink_clicks_written_total{result="success|error"}`。设计见 [ADR 0007](adr/0007-async-click-recording.md)。
- 进程指标由 `collectors.NewProcessCollector` 暴露为 `process_*`，连接池指标由 `collectors.NewDBStatsCollector` 暴露为 `go_sql_*`，重点看 `go_sql_in_use_connections` 和 `go_sql_wait_duration_seconds_total`。
- 表 `links`：`id` 自增主键（内部），`code` 唯一约束（对外），`url`、`clicks`、`expires_at`、`created_at`、`tenant_id`；`code` 格式和 `url` 长度由 CHECK 约束兜底。`expires_at` 为 NULL 表示永不过期，查询只返回 `expires_at IS NULL OR expires_at > now()` 的行。`tenant_id` 指向 `tenants.id`，`NOT NULL`，没有数据库外键；`links` 和 `api_keys` 各有 `(tenant_id, id)` 索引。引用完整性由应用层保证，见 [ADR 0010](adr/0010-app-level-referential-integrity.md)。API Key 的 SHA-256 存在 `api_keys.key_hash`，明文不入库。`api_keys.expires_at` 为空表示不过期，到期时刻本身算过期；`api_keys.revoked_at` 非空表示已吊销。鉴权把过期和吊销看成密钥无效。`tenants.external_id` 可空，留给以后的平台租户编号。运维命令在 `cmd/console`：`key issue`、`key list`、`key revoke`。表结构由 `cmd/migrate` 管理，当前版本记在 `schema_migrations`；HTTP 进程启动时不迁移。
- 短码是 8 位 Base62 随机码，`crypto/rand` 生成；碰撞由唯一索引和最多 5 次重试处理。

## 配置

`config.LoadConfig` 在启动时读取并校验：

- `ADDR`：监听地址，默认 `:8080`
- `DATABASE_URL`：必填
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`
- `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME`
- `REQUEST_TIMEOUT`：默认 `5s`，单个请求的处理时限，超时返回 504
- `SHUTDOWN_TIMEOUT`：默认 `10s`，优雅退出时等待正在处理的请求的上限
- `CACHE_MISS_CONCURRENCY`：默认 `8`，普通缓存未命中时进程内同时回源数据库的最大请求数，超出返回 503
- `CACHE_FETCH_TIMEOUT`：默认 `1s`，单次回源超时；必须为正且不超过 `REQUEST_TIMEOUT`
- `REDIS_URL`：必填，`redis://` 或 `rediss://`（TLS），可带用户名、密码和 DB 编号；没有密码就是 `redis://localhost:6379/0`。`main` 用它 Ping，连不上则进程退出
- `LOG_LEVEL`：`debug`、`info`、`warn`、`error`，默认 `info`

## 退出

进程监听 `SIGINT` 和 `SIGTERM`。收到信号后关闭 HTTP 监听，等待正在处理的请求结束，上限是 `SHUTDOWN_TIMEOUT`。`run` 返回后 `defer` 先让 `ClickRecorder` 把缓冲里的点击写完（同样受一份新的 `SHUTDOWN_TIMEOUT` 限制；已经开始的那一批还会用完自己的写入超时，默认 2s），再关闭 Redis 客户端和数据库连接池。`Shutdown` 不会取消这些请求的 context；它们有自己的 `REQUEST_TIMEOUT` 兜底，慢查询不会把退出拖满。再次收到信号时恢复默认行为，进程立刻退出。进程崩溃或收到 SIGKILL 时，缓冲区里还没写完的点击会丢掉。

## 监控

`GET /metrics` 暴露 Go 运行时、进程、HTTP、缓存、存储、连接池和点击指标。Prometheus 每 5 秒一次的抓取不计入访问日志和 HTTP 指标，避免抓取噪声混进业务数据。本地 `make prometheus` 启动 Prometheus，抓取 `host.docker.internal:8080/metrics`，配置在 `deploy/prometheus.yml`。`/metrics` 与业务共用端口，公网隔离由部署层保证。

## 尚未实现

- 拆分 HTTP 进程（阶段 6，[ADR 0009](adr/0009-process-split-and-credentials.md)）。运维命令已经在 `cmd/console`；`cmd/server` 仍同时提供跳转和管理 API。拆开后 redirector 只做公开跳转，管理 HTTP 迁到 console。用户、登录和页面不在这次拆分里
- OpenTelemetry 追踪（可选，ADR 0005）：等指标解释不了慢请求再加
