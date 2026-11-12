# 架构

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 目录结构

```txt
shortlink/
├── cmd/server/          # 程序入口，只做组装和启动
├── internal/
│   ├── httpapi/         # handler、路由、中间件；定义 LinkStore 接口
│   ├── link/            # 领域模型：Link、NewLink、错误、URL 校验
│   ├── idgen/           # 随机短码生成
│   ├── memorystore/     # 内存存储，仅测试和本地开发
│   ├── postgres/        # PostgreSQL 存储，生产使用
│   ├── storemetrics/    # LinkStore 的指标装饰器
│   ├── cache/           # Redis 客户端和读缓存（CachedStore）
│   └── config/          # 配置加载
├── migrations/          # 建表 SQL
├── deploy/              # 本地 Prometheus 配置
├── docs/                # 架构、API、运行手册、路线图、ADR
└── Makefile
```

## 依赖方向

```txt
cmd/server   -> httpapi, postgres, storemetrics, cache, config
config       -> postgres
httpapi      -> link, idgen
postgres     -> link, idgen
memorystore  -> link, idgen
storemetrics -> link
cache        -> link
link         -> 标准库
idgen        -> 标准库
```

`LinkStore` 接口定义在 `httpapi`（使用方），内存版和 PostgreSQL 版都隐式实现它。`link` 不依赖存储和 HTTP。

## 请求流程

### 创建短链

```txt
POST /api/v1/links
  -> requestID
  -> logging
  -> metrics
  -> recoverer
  -> timeout
  -> handleCreate
  -> 读取并限制请求体
  -> json.Unmarshal
  -> link.NewLink 校验
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
  -> 302 Location: 原始 URL
```

`chain` 里靠前的中间件在外层。`requestID` 确定编号，写入响应头 `X-Request-ID` 和请求 context。`logging` 记录方法、路径、状态码和耗时；`metrics` 按路由模式、方法和状态码记录请求数和耗时，方法只保留 GET、HEAD、POST，其余记成 `other`。它放在 `recoverer` 外面，panic 被写成 500 后也能记到这个状态码；`recoverer` 把 handler 的 panic 写成 500。带 context 的日志会自动加上 `request_id`。`http.ErrAbortHandler` 会继续抛出，那是 `net/http` 用来中止当前连接的哨兵。

`timeout` 在最里层，给请求 context 加处理时限。到点后 handler 里的数据库查询被取消（pgx 会中断正在执行的 SQL），handler 返回 504。`http.Server` 的 `ReadTimeout`、`WriteTimeout` 只管连接的读写，不会取消 handler 的 context，处理时限只能靠这一层。这两个连接超时都是 `REQUEST_TIMEOUT` 加 5 秒：读超时包住请求体，写超时留给 504 的写出。

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发。
- `internal/postgres`：`database/sql` + pgx，生产使用。
- `internal/storemetrics`：实现 `LinkStore` 的装饰器，按操作记录耗时和结果。`not_found`、`canceled`、`timeout` 都不算失败，只有其余错误记成 `error`。耗时直方图带同样的结果标签。组装顺序是 `HTTP -> cache -> storemetrics -> postgres.Store`，缓存命中不会计入存储耗时。
- `internal/cache`：`GetByCode` 的 Redis 读缓存装饰器。key 是 `shortlink:link:<code>`，value 是完整 `link.Link` 的 JSON；正缓存 TTL 1 小时，负缓存（JSON `null`）TTL 1 分钟。并发未命中用 singleflight 合并回源；Redis 读写失败时直接回源数据库，不影响请求。设计见 [ADR 0006](adr/0006-redis-cache-design.md)。
- 连接池指标由 `collectors.NewDBStatsCollector` 暴露为 `go_sql_*`，重点看 `go_sql_in_use_connections` 和 `go_sql_wait_duration_seconds_total`。
- 表 `links`：`id` 自增主键（内部），`code` 唯一约束（对外），`url`、`clicks`、`expires_at`、`created_at`；`code` 格式和 `url` 长度由 CHECK 约束兜底。
- 短码是 8 位 Base62 随机码，`crypto/rand` 生成；碰撞由唯一索引和最多 5 次重试处理。

## 配置

`config.LoadConfig` 在启动时读取并校验：

- `ADDR`：监听地址，默认 `:8080`
- `DATABASE_URL`：必填
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`
- `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME`
- `REQUEST_TIMEOUT`：默认 `5s`，单个请求的处理时限，超时返回 504
- `SHUTDOWN_TIMEOUT`：默认 `10s`，优雅退出时等待正在处理的请求的上限
- `REDIS_URL`：必填，`redis://` 或 `rediss://`（TLS），可带用户名、密码和 DB 编号；没有密码就是 `redis://localhost:6379/0`。`main` 用它 Ping，连不上则进程退出
- `LOG_LEVEL`：`debug`、`info`、`warn`、`error`，默认 `info`

## 退出

进程监听 `SIGINT` 和 `SIGTERM`。收到信号后关闭 HTTP 监听，等待正在处理的请求结束，上限是 `SHUTDOWN_TIMEOUT`，然后 `run` 返回，`defer` 关闭 Redis 客户端和数据库连接池。`Shutdown` 不会取消这些请求的 context；它们有自己的 `REQUEST_TIMEOUT` 兜底，慢查询不会把退出拖满。再次收到信号时恢复默认行为，进程立刻退出。

## 监控

`GET /metrics` 暴露 Go 运行时、HTTP、存储和连接池指标。本地 `make prometheus` 启动 Prometheus，抓取 `host.docker.internal:8080/metrics`，配置在 `deploy/prometheus.yml`。

## 尚未实现

- Prometheus 业务指标（[ADR 0005](adr/0005-prometheus-before-tracing.md)）：`GET /metrics` 已暴露 Go 运行时、HTTP、存储和连接池指标；缓存命中和 `ClickRecorder` 的 dropped 计数待做。`/metrics` 与业务共用端口，公网隔离由部署层保证
- 点击统计和点击写入（第 10 课）
- 多租户和 API Key 鉴权，拆分 `cmd/redirector` 与 `cmd/console`（阶段 6）
- OpenTelemetry 追踪（可选，ADR 0005）：等指标解释不了慢请求再加
