# 架构

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 进程边界

同一个 Go module 产出两个 HTTP 进程，共用 PostgreSQL、Redis 和 `internal/` 里的包，进程之间不调用：

| 进程 | 路由 | 依赖 |
| --- | --- | --- |
| `redirector` | `GET /r/{code}`、`/healthz`、`/metrics` | PostgreSQL、Redis、`ClickRecorder` |
| `console` | `POST/GET /api/v1/links`、`GET/PATCH/DELETE /api/v1/links/{code}`、`GET /api/v1/links/{code}/stats`、`/healthz`、`/metrics` | PostgreSQL、Redis（创建、修改、删除维护缓存）；`key` 命令只连 PostgreSQL |

`redirector` 只挂公开跳转，不注册管理路由；API Key 鉴权只存在于 `console`。创建成功后 `console` 写穿正缓存；修改和删除提交后无条件覆盖缓存（删除写负缓存），`redirector` 读同一份缓存，所以新短码不会在负缓存里继续 404，改过的链接也不会继续跳旧地址。回源写缓存一律用 `SET NX`，在途旧快照盖不掉管理写的新值。两个进程各自监听、各自扩容、各自暴露 `/metrics`，连接池也是各一份，总连接数是两边之和。部署时由反向代理把 `/r/` 转到 redirector、把 `/api/v1/` 转到 console；短链公开地址不变。

这是按流量和故障域把一个服务部署两次，不是微服务：数据所有权没有拆开，进程之间也没有 HTTP/RPC 调用。决策见 [ADR 0009](adr/0009-process-split-and-credentials.md)。

## 目录结构

```txt
shortlink/
├── cmd/redirector/      # 公开跳转进程：GET /r/{code}、/healthz、/metrics
├── cmd/console/         # 管理进程：短链增删改查和统计；另有租户和 API Key 运维命令
├── cmd/migrate/         # 数据库迁移。启动 HTTP 进程时不执行
├── internal/
│   ├── httpapi/         # handler、路由、中间件；定义 LinkStore、LinkAdminStore、LinkStatsStore
│   ├── link/            # 领域模型：Link、AdminLink、校验、列表和统计类型
│   ├── tenant/          # 租户和 API Key 的明文格式、哈希
│   ├── idgen/           # 随机短码生成
│   ├── memorystore/     # 内存存储，仅测试和本地开发；只实现跳转和创建
│   ├── postgres/        # PostgreSQL 存储：链接、管理查询、点击日聚合和统计
│   ├── storemetrics/    # 存储的指标装饰器，透传七个方法
│   ├── cache/           # Redis 客户端和读缓存（CachedStore）
│   ├── click/           # 点击元数据分类和异步批量记录（ClickRecorder）
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
cmd/redirector -> httpapi, postgres, storemetrics, cache, click, config
cmd/console    -> httpapi, postgres, storemetrics, cache, config；key 命令只用 postgres
cmd/migrate    -> golang-migrate
config         -> postgres
httpapi        -> link, click, idgen, tenant
postgres       -> link, click, idgen, tenant
memorystore    -> link, idgen
storemetrics   -> link
cache          -> link
click          -> 标准库、Prometheus
link           -> 标准库
idgen          -> 标准库
```

`LinkStore` 接口定义在 `httpapi`（使用方），内存版和 PostgreSQL 版都隐式实现它。`link` 不依赖存储和 HTTP。`click` 自己定义写点击的接口，`postgres.Store` 隐式实现它。管理 API 的 `APIKeyAuthenticator` 也定义在 `httpapi`，由同一个 `postgres.Store` 实现；缓存不参与鉴权。

## 请求流程

### 创建短链（console）

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

### 跳转（redirector）

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
  -> 仅 GET 调用 click.NewClick（来源、设备、客户端、按天访客哈希）
  -> clicks.Record(click)（非阻塞；缓冲满只记 dropped，不影响 302）
  -> 302 Location: 原始 URL
```

GET 模式同时匹配 HEAD。HEAD 返回同样的 302，但不记点击。`Record` 在进入队列时取 `At`，批量写出的延迟不会把点击挪到第二天。

### 管理 API（console）

```txt
GET /api/v1/links
  -> authenticate -> principalFrom(context)
  -> 解析 limit、sort、cursor（非法值 400）
  -> admin.List（cache 透传 -> storemetrics -> postgres，多要一条判断下一页）
  -> 200 {"items":[...],"next_cursor":"..."}

GET/PATCH/DELETE /api/v1/links/{code}
  -> authenticate -> 校验短码格式
  -> GetForTenant / Update / Delete，全部带 tenant_id，找不到一律 404
  -> PATCH：RawMessage 区分「没传」和「传了 null」，校验 URL 和将来时间
  -> Update/Delete 提交后由 cache.Store 无条件覆盖缓存（删除写负缓存）

GET /api/v1/links/{code}/stats
  -> authenticate -> 解析 from/to（默认 8 天，跨度不超过 90 天）
  -> 只读 REPEATABLE READ 事务：links.clicks + 三张日表
  -> 200：日趋势和访客补零，设备和客户端固定类别
```

`chain` 里靠前的中间件在外层。`requestID` 确定编号，写入响应头 `X-Request-ID` 和请求 context。`logging` 记录方法、路径、状态码和耗时；`metrics` 按路由模式、方法和状态码记录请求数和耗时，方法只保留 GET、HEAD、POST，其余记成 `other`。它放在 `recoverer` 外面，panic 被写成 500 后也能记到这个状态码；`recoverer` 把 handler 的 panic 写成 500。带 context 的日志会自动加上 `request_id`。`http.ErrAbortHandler` 会继续抛出，那是 `net/http` 用来中止当前连接的哨兵。

`timeout` 在最里层，给请求 context 加处理时限。到点后 handler 里的数据库查询被取消（pgx 会中断正在执行的 SQL），handler 返回 504。`http.Server` 的 `ReadTimeout`、`WriteTimeout` 只管连接的读写，不会取消 handler 的 context，处理时限只能靠这一层。这两个连接超时都是 `REQUEST_TIMEOUT` 加 5 秒：读超时包住请求体，写超时留给 504 的写出。

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发。
- `internal/postgres`：`database/sql` + pgx，生产使用。
- `internal/storemetrics`：实现 `LinkStore` 的装饰器，按操作记录耗时和结果，操作有 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`。`not_found`、`canceled`、`timeout` 都不算失败，只有其余错误记成 `error`。耗时直方图带同样的结果标签。组装顺序是 `HTTP -> cache -> storemetrics -> postgres.Store`，缓存命中不会计入存储耗时。
- `internal/cache`：`GetByCode` 的 Redis 读缓存装饰器。key 是 `shortlink:link:<code>`，value 是完整 `link.Link` 的 JSON；正缓存 TTL 取剩余寿命和 1 小时里更短的那个，没有到期时间时是 1 小时，负缓存（JSON `null`）TTL 1 分钟。命中到已经过期的正缓存时删掉 key 并返回找不到。URL 为空或 Code 对不上的 JSON 也删掉，按未命中回源。并发未命中用 singleflight 合并回源；普通未命中和 Redis 读失败各有一条回源限流，超过许可立即返回 503，不排队。普通未命中的许可数由 `CACHE_MISS_CONCURRENCY` 控制（默认 8），Redis 读失败固定为 8，两条路径分开计数。回源使用独立的 `CACHE_FETCH_TIMEOUT`（默认 1s），不再借用 `REQUEST_TIMEOUT`；`singleflight` 的共享回源不跟随领头请求取消。被限流的请求记在 `shortlink_cache_shed_total{path="miss|fallback"}`，按请求计数，等待者也算。
  写入分两种模式：回源（正、负缓存）用 `SET NX`，只有 key 不存在时才写；创建、修改、删除用无条件 `SET`，在数据库提交之后覆盖缓存。修改后的链接如果已经过期，写负缓存。这样在途回源读到的旧快照不会盖掉管理写的新值，删除也不需要额外的 `DEL`。Redis 写失败只影响一致性，旧值最多保留到 TTL。查找结果记在 `shortlink_cache_gets_total`（`hit` 含负缓存、`miss`、`fallback`），写缓存结果记在 `shortlink_cache_writes_total`（`success`、`skipped`、`error`；`skipped` 表示 `SET NX` 没写进去，说明已有更新的值），缓存操作耗时记在 `shortlink_cache_operation_duration_seconds`（`operation` 为 `get`、`set`）。正、负缓存 TTL 向下随机抖动最多 10%。缓存设计见 [ADR 0006](adr/0006-redis-cache-design.md)，回源保护见 [ADR 0008](adr/0008-cold-cache-read-path.md)，管理写一致性见 [ADR 0011](adr/0011-link-crud-api.md)。
- `internal/click`：负责点击元数据分类和异步批量记录。GET 跳转成功后构造 `Click`（来源 host、设备、客户端、按天访客哈希），放进缓冲 channel；一条 goroutine 按 100 条或 1 秒把点击交给 `postgres.Store.RecordClicks`。缓冲满时 `shortlink_clicks_dropped_total` 加一；写出结果记在 `shortlink_clicks_written_total{result="success|error"}`。分类是启发式，不是安全边界；User-Agent 截到 512 字节，Referer 超长归 `other`。访客哈希是 HMAC-SHA256 的前 16 字节，盐按天参与哈希，不存原始 IP 和 UA。设计见 [ADR 0007](adr/0007-async-click-recording.md)、[ADR 0012](adr/0012-click-analytics.md)。
- 进程指标由 `collectors.NewProcessCollector` 暴露为 `process_*`，连接池指标由 `collectors.NewDBStatsCollector` 暴露为 `go_sql_*`，重点看 `go_sql_in_use_connections` 和 `go_sql_wait_duration_seconds_total`。
- 表 `links`：`id` 自增主键（内部），`code` 唯一约束（对外），`url`、`clicks`、`expires_at`、`created_at`、`tenant_id`；`code` 格式和 `url` 长度由 CHECK 约束兜底。`expires_at` 为 NULL 表示永不过期，跳转查询只返回 `expires_at IS NULL OR expires_at > now()` 的行；管理查询不按过期时间过滤。`tenant_id` 指向 `tenants.id`，`NOT NULL`，没有数据库外键；`links` 和 `api_keys` 各有 `(tenant_id, id)` 索引，`links` 另有 `(tenant_id, clicks DESC, id DESC)` 服务历史排序。引用完整性由应用层保证，见 [ADR 0010](adr/0010-app-level-referential-integrity.md)。API Key 的 SHA-256 存在 `api_keys.key_hash`，明文不入库。`api_keys.expires_at` 为空表示不过期，到期时刻本身算过期；`api_keys.revoked_at` 非空表示已吊销。鉴权把过期和吊销看成密钥无效。`tenants.external_id` 可空，留给以后的平台租户编号。运维命令在 `cmd/console`：`key issue`、`key list`、`key revoke`。表结构由 `cmd/migrate` 管理，当前版本记在 `schema_migrations`；HTTP 进程启动时不迁移。
- 点击日聚合：`link_clicks_daily` 主键 `(link_id, day)`，冗余 `tenant_id` 由写库时的 `JOIN links` 填充，索引 `(tenant_id, day, clicks DESC, link_id DESC)` 服务今日排序；`link_dimension_daily` 主键 `(link_id, day, dimension, value)`，`dimension` 是 `referrer`、`device`、`client`；`link_visitors_daily` 主键 `(link_id, day, visitor_hash)`，哈希 16 字节。三张表都不建外键。`links.clicks` 是终身累计的读模型，和日表在同一个事务里更新，一个批次要么全写要么全不写。删除链接时在同一事务里删三张统计表和 `links` 行。统计接口在只读的 `REPEATABLE READ` 事务里查询，同一批点击的各维度是同一个快照。见 [ADR 0012](adr/0012-click-analytics.md)。
- 短码是 8 位 Base62 随机码，`crypto/rand` 生成；碰撞由唯一索引和最多 5 次重试处理。

## 配置

`config.LoadConfig` 在启动时读取并校验：

- `ADDR`：监听地址，默认 `:8080`。两个 HTTP 进程各自读取自己的环境；本机开发时 console 由 Makefile 固定为 `:8081`
- `DATABASE_URL`：必填
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`
- `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME`
- `REQUEST_TIMEOUT`：默认 `5s`，单个请求的处理时限，超时返回 504
- `SHUTDOWN_TIMEOUT`：默认 `10s`，优雅退出时等待正在处理的请求的上限
- `CACHE_MISS_CONCURRENCY`：默认 `8`，普通缓存未命中时进程内同时回源数据库的最大请求数，超出返回 503
- `CACHE_FETCH_TIMEOUT`：默认 `1s`，单次回源超时；必须为正且不超过 `REQUEST_TIMEOUT`
- `REDIS_URL`：必填，`redis://` 或 `rediss://`（TLS），可带用户名、密码和 DB 编号；没有密码就是 `redis://localhost:6379/0`。`main` 用它 Ping，连不上则进程退出
- `VISITOR_SALT`：访客哈希的 HMAC 密钥。redirector 启动时必填，为空直接报错；console 不用。生产从密钥管理注入，不能提交真实值
- `TRUST_PROXY_HEADERS`：默认 `false`；为 `true` 时从 `X-Forwarded-For` 第一段取客户端 IP。只能开在可信反向代理后面
- `LOG_LEVEL`：`debug`、`info`、`warn`、`error`，默认 `info`

## 退出

两个进程都监听 `SIGINT` 和 `SIGTERM`。收到信号后关闭 HTTP 监听，等待正在处理的请求结束，上限是 `SHUTDOWN_TIMEOUT`。`run` 返回后 `defer` 先让 `ClickRecorder` 把缓冲里的点击写完（只有 redirector 有这一步；同样受一份新的 `SHUTDOWN_TIMEOUT` 限制，已经开始的那一批还会用完自己的写入超时，默认 2s），再关闭 Redis 客户端和数据库连接池。`Shutdown` 不会取消这些请求的 context；它们有自己的 `REQUEST_TIMEOUT` 兜底，慢查询不会把退出拖满。再次收到信号时恢复默认行为，进程立刻退出。进程崩溃或收到 SIGKILL 时，缓冲区里还没写完的点击会丢掉。

## 监控

两个进程各自暴露 `GET /metrics`：Go 运行时、进程、HTTP、连接池是两边都有；缓存、存储和点击指标只在会用到它们的进程里产生。Prometheus 每 5 秒一次的抓取不计入访问日志和 HTTP 指标，避免抓取噪声混进业务数据。本地 `make prometheus` 启动 Prometheus，分别抓取 `host.docker.internal:8080/metrics`（redirector）和 `host.docker.internal:8081/metrics`（console），配置在 `deploy/prometheus.yml`；两个 job 的指标名相同，查询时用 `job` 区分。`/metrics` 与业务共用端口，公网隔离由部署层保证。

## 尚未实现

- OpenTelemetry 追踪（可选，ADR 0005）：等指标解释不了慢请求再加
- 用户、登录和短链管理页面（ADR 0009 把边界留出来，留到后续阶段）
