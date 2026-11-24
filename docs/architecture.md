# 架构

当前结构写在这里：进程、目录、请求怎么走、有哪些配置。为什么这样定写在 [ADR](adr/README.md)，操作和压测写在 [runbook](runbook.md)；决策正文不在这里重复。

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 进程边界

同一个 Go module 产出两个 HTTP 进程，共用 PostgreSQL、Redis 和 `internal/` 里的包，进程之间不调用：

| 进程 | 路由 | 依赖 |
| --- | --- | --- |
| `redirector` | `GET /r/{code}`、`/healthz`、`/readyz`、`/metrics` | PostgreSQL、Redis、`ClickRecorder` |
| `console` | `GET /api/v1/auth/providers`、`GET /api/v1/auth/login`、`GET /api/v1/auth/callback`、`GET /api/v1/auth/github/login`、`GET /api/v1/auth/github/callback`、`POST /api/v1/auth/logout`、`GET /api/v1/me`、`POST /api/v1/orgs`、`GET /api/v1/members`、`PATCH/DELETE /api/v1/members/{user_id}`、`GET/POST/DELETE /api/v1/invitations`、`GET/POST/DELETE /api/v1/api-keys`、`GET /api/v1/audit-logs`（配置 OIDC 或 GitHub 时）、`POST/GET /api/v1/links`、`GET/PATCH/DELETE /api/v1/links/{code}`、`GET /api/v1/links/{code}/stats`、可选 `WEB_DIST_DIR` 前端静态页面（`/`）、`/healthz`、`/readyz`、`/metrics` | PostgreSQL、Redis（创建、修改、删除、封禁、解封维护缓存）、可选 OIDC / GitHub OAuth 2.0 身份提供方；`key`、`tenant` 和 `member` 命令只连 PostgreSQL，`link` 命令和 `worker` 模式还需要 Redis |
| `console worker` | 无公开业务路由；配置 `WORKER_ADDR` 时暴露 `/healthz`、`/readyz`、`/metrics` | PostgreSQL（`jobs` 队列抢占/回写/清理、`sessions` 定期清理、风控 `BanIfURL`）、Redis（写负缓存与 fail-open 风控结果缓存） |

`redirector` 不注册管理路由，API Key 鉴权只在 `console`。反向代理把 `/r/` 转到 redirector、把 `/api/v1/` 转到 console：`deploy/nginx.conf`（宿主机进程）、`deploy/nginx.stack.conf`（容器栈）。

进程怎么拆、凭证怎么发，见 [ADR 0009](adr/0009-process-split-and-credentials.md)。Redis 故障时的降级和就绪检查见 [ADR 0006](adr/0006-redis-cache-design.md)。

## 目录结构

```txt
shortlink/
├── cmd/redirector/      # 公开跳转进程：GET /r/{code}、/healthz、/readyz、/metrics
├── cmd/console/         # 管理进程：短链增删改查和统计；另有租户、API Key 和成员邀请运维命令
├── cmd/migrate/         # 数据库迁移。启动 HTTP 进程时不执行
├── Dockerfile           # redirector、console、migrate 共用的镜像
├── internal/
│   ├── httpx/           # 零业务依赖 HTTP 底座：中间件链、requestid、logging、HTTP 指标、recoverer、timeout、/healthz、/readyz、/debug/pprof/、/debug/log/level、ClientIP、优雅启停与 JSON/错误写出
│   ├── logx/            # 结构化日志底座：根 slog 构造（text 接入 lmittmann/tint 自动识别 TTY/NO_COLOR、json 保持标准库、service 标签、LevelVar）、WithGroup 顶层 context 字段注入、RequestEvent 宽事件收集槽与敏感信息脱敏（零业务依赖）
│   ├── redirector/      # 公开跳转进程 HTTP 层：GET /r/{code}、点击记录与只读 LinkStore、ClickRecorder 接口
│   ├── console/         # 管理进程 HTTP 层：authHandler、orgHandler、memberHandler、invitationHandler、apiKeyHandler、auditHandler、webhandler、按租户限流、短链 CRUD 与统计；定义各消费方 Store/Client/Authorizer 接口
│   ├── identity/        # 认证主体（Principal）、API Key 与会话令牌格式/哈希、OIDC 与 GitHub OAuth 2.0 客户端、权限表与成员规则
│   │   ├── fakegithub/  # 仅测试用：内存版 GitHub OAuth 2.0 与 REST API 提供方（不进入生产二进制）
│   │   └── fakeoidc/    # 仅测试用：内存版标准 OIDC 提供方（不进入生产二进制）
│   ├── audit/           # 审计日志领域模型、动作枚举与 context 元数据透传（零 PG/HTTP 依赖，ADR 0023）
│   ├── link/            # 领域模型：Link、AdminLink、校验、列表和统计类型
│   ├── tenant/          # 租户（组织）领域模型
│   ├── idgen/           # 随机短码生成
│   ├── job/             # 通用可靠后台任务引擎：Worker Pool、指数退避、Lease Reaper、周期清理调度与指标（零 PG/Redis 依赖）
│   ├── risk/            # 异步 URL 风控：防 SSRF 的安全 HTTP 客户端、按 host 限速、Redis fail-open 结果缓存、规则检测与 link.risk_scan 处理器
│   ├── memorystore/     # 内存存储，仅测试和本地开发；只实现跳转和创建
│   ├── postgres/        # PostgreSQL 存储：链接、管理查询、点击日聚合、统计、会话、成员邀请与 jobs Outbox 队列
│   ├── storemetrics/    # 存储的指标装饰器，透传完整存储能力
│   ├── cache/           # Redis 客户端和读缓存（CachedStore）
│   ├── click/           # 点击元数据分类和异步批量记录（ClickRecorder）
│   └── config/          # 配置加载
├── migrations/          # golang-migrate 的 up/down SQL
├── web/                 # 独立前端控制台（React 19 + TypeScript + Tailwind v4 + Radix UI + React Router + TanStack Query），产物由 console 通过 WEB_DIST_DIR 可选托管（ADR 0021）
├── deploy/              # 本地 Prometheus、Dex OIDC 配置和反向代理
├── scripts/             # 测试库准备和冒烟测试脚本
├── .github/workflows/   # CI：静态检查、集成测试、冒烟
├── docs/                # 架构、API、运行手册、路线图、ADR
└── Makefile
```

## 依赖方向

```txt
cmd/redirector -> redirector, httpx, logx, postgres, storemetrics, cache, click, config
cmd/console    -> console, httpx, logx, audit, identity, job, link, postgres, risk, storemetrics, cache, config；key/tenant/member 命令只用 postgres，link 命令和 worker 模式还要 cache/risk/job
cmd/migrate    -> golang-migrate
config         -> postgres, cache, job, link, logx, risk
redirector     -> httpx, link, click, idgen, Prometheus
console        -> httpx, logx, audit, link, click, idgen, identity, tenant, golang.org/x/time/rate, Prometheus
httpx          -> logx, 标准库、Prometheus
identity       -> tenant, logx, golang.org/x/oauth2, github.com/coreos/go-oidc/v3
audit          -> 标准库
job            -> logx, 标准库、Prometheus
risk           -> cache, audit, job, link, golang.org/x/time/rate, go-redis, Prometheus
postgres       -> audit, link, click, idgen, identity, job, tenant
memorystore    -> link, idgen
storemetrics   -> link
cache          -> link, logx
click          -> link、标准库、Prometheus
link           -> 标准库
idgen          -> 标准库
logx           -> 标准库、github.com/lmittmann/tint
```

`redirector.LinkStore` 与 `console.Store` 接口分别定义在各自使用方，内存版、PostgreSQL 版及装饰器隐式实现它们；`httpx` 不依赖任何业务包。`link` 与 `audit` 不依赖存储和 HTTP。`click` 自己定义写点击的接口，`postgres.Store` 隐式实现它。管理 API 的 `APIKeyAuthenticator`、`SessionStore`、`MemberStore`、`OrgStore`、`InvitationStore`、`APIKeyStore`、`AuditLogStore`、`OIDCClient`、`GitHubClient` 和 `Authorizer` 定义在 `console`，分别由 `postgres.Store`、`identity.OIDCProvider`、`identity.GitHubProvider` 和 `identity.MapAuthorizer` 实现；缓存不参与鉴权。

## 请求流程

### 创建短链（console）

```txt
POST /api/v1/links
  -> requestID
  -> observe（访问日志 + HTTP 指标）
  -> recoverer
  -> timeout
  -> authenticate：Authorization 优先走 API Key（忽略 sl_session 和 X-Org-ID）；否则校验唯一的 sl_session、CrossOriginProtection 与 X-Org-ID 成员关系，构造 identity.Principal 放进 context。会话路径的响应带 Cache-Control: no-store
  -> rateLimit：按租户的进程内令牌桶，超限 429 + Retry-After
  -> requirePerm：通过 Authorizer 检查 Principal 权限，不足返回 403
  -> handleCreate
  -> 读取并限制请求体
  -> json.Unmarshal
  -> link.NewLink 校验
  -> TenantID 取自 context
  -> store.Create 生成随机短码，在同一语句里占本月配额并写库；配额满返回 403
  -> 201 + Location: /r/{code}
```

### 跳转（redirector）

```txt
GET /r/{code}
  -> requestID
  -> observe（访问日志 + HTTP 指标）
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

GET 模式同时匹配 HEAD。HEAD 记不记点击见 [ADR 0007](adr/0007-async-click-recording.md)；点击落在哪一天见 [ADR 0012](adr/0012-click-analytics.md)。

### 管理 API（console）

```txt
GET /api/v1/links
  -> authenticate -> rateLimit -> requirePerm -> principalFrom(context)
  -> 解析 limit、sort、cursor（非法值 400）
  -> admin.List（cache 透传 -> storemetrics -> postgres，多要一条判断下一页）
  -> 200 {"items":[...],"next_cursor":"..."}

GET/PATCH/DELETE /api/v1/links/{code}
  -> authenticate -> rateLimit -> requirePerm -> 校验短码格式
  -> GetForTenant / Update / Delete，全部带 tenant_id，找不到一律 404
  -> PATCH：RawMessage 区分「没传」和「传了 null」，校验 URL、将来时间和 disabled 布尔值
  -> 被封禁的链接 Update/Delete 返回 403 link is banned，其他租户仍然是 404
  -> Update/Delete 提交后由 cache.Store 覆盖缓存：可跳转写正缓存，否则写负缓存
  -> Delete 在同一事务里删除链接和统计行，不返还本月配额

GET /api/v1/links/{code}/stats
  -> authenticate -> rateLimit -> requirePerm -> 解析 from/to（默认 8 天，跨度不超过 90 天）
  -> 只读 REPEATABLE READ 事务：links.clicks + 三张日表
  -> 200：日趋势和访客补零，设备和客户端固定类别
```

`chain` 里靠前的中间件在外层。`requestID` 确定编号，写入响应头 `X-Request-ID` 和请求 context。`observe` 合并了访问日志（Canonical Log Line）与 HTTP 指标（共用单次 `statusWriter` 与耗时统计）：记录方法、路由模板、路径、状态码、毫秒耗时（`duration_ms`）、响应字节数、客户端 `ip`，并从请求内层的 `RequestEvent` 汇总 `tenant_id`、`actor`、`cache`、`retry_after` 与 `err_msg`；级别按 `>=500 -> Error`、慢请求 / 非放行 `4xx -> Warn`、`2xx/3xx` 与 `redirector` 404 -> `Info`、`/healthz`/`/readyz` 2xx -> `Debug`，且 `redirector` 支持正常 `INFO` 流量采样（`5xx` 与慢请求全保）。指标按路由模式、方法和状态码记录请求数和耗时，方法为 GET、HEAD、POST、PATCH、DELETE，其余记成 `other`。`observe` 放在 `recoverer` 外面，panic 被写成 500 后也能记到状态码和 `err_msg=panic`。各进程根日志器统一通过 `logx` 装配，本地 `text` 模式使用 `lmittmann/tint` 彩色可读格式（非终端或设 `NO_COLOR` 自动去色），生产 `json` 模式输出严格 JSON；带 context 的日志即使经过 `WithGroup` 也会在顶层自动带上 `request_id`、`tenant_id`、`job_id` 和 `job_kind`，并自动脱敏连接串密码与密钥；启用回环 `PPROF_ADDR` 时同时暴露 `GET/PUT /debug/log/level` 支持动态调级。`http.ErrAbortHandler` 会继续抛出，那是 `net/http` 用来中止当前连接的哨兵。

`timeout` 在最里层，给请求 context 加处理时限。到点后 handler 里的数据库查询被取消（pgx 会中断正在执行的 SQL），handler 返回 504。`http.Server` 的 `ReadTimeout`、`WriteTimeout` 只管连接的读写，不会取消 handler 的 context，处理时限只能靠这一层。这两个连接超时都是 `REQUEST_TIMEOUT` 加 5 秒：读超时包住请求体，写超时留给 504 的写出。

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发（[ADR 0004](adr/0004-storage-package-layout.md)）。
- `internal/postgres`：`database/sql` + pgx，生产使用。连接池和迁移见 [ADR 0003](adr/0003-postgres-and-pool-config.md)。
- `internal/storemetrics`：存储耗时装饰器。组装顺序和结果分类见 [ADR 0005](adr/0005-prometheus-before-tracing.md)、[ADR 0014](adr/0014-per-tenant-rate-limit-and-quota.md)、[ADR 0016](adr/0016-link-states-and-takedown.md)。操作有 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`、`ban`、`unban`。管理写复查也会记一次 `get`。
- `internal/cache`：`GetByCode` 的 Redis 读缓存。值、TTL、singleflight、故障降级和指标见 [ADR 0006](adr/0006-redis-cache-design.md)，冷缓存回源保护见 [ADR 0008](adr/0008-cold-cache-read-path.md)，管理写和回源的两种 `SET` 见 [ADR 0011](adr/0011-link-crud-api.md)，下架后的缓存和复查见 [ADR 0016](adr/0016-link-states-and-takedown.md)。
- `internal/click`：点击分类和异步批量写出。投递和关停见 [ADR 0007](adr/0007-async-click-recording.md)，日聚合、维度和访客哈希见 [ADR 0012](adr/0012-click-analytics.md)。
- 进程指标和连接池指标见 [ADR 0005](adr/0005-prometheus-before-tracing.md)。
- 表 `links`、`tenants`、`api_keys`、`users`、`user_identities`、`memberships`、`sessions`、`invitations`、`jobs`、`audit_logs` 的列以 `migrations/` 为准。短码见 [ADR 0002](adr/0002-random-short-codes.md)，不建外键见 [ADR 0010](adr/0010-app-level-referential-integrity.md)，API Key 和运维命令见 [ADR 0009](adr/0009-process-split-and-credentials.md)，停用和封禁见 [ADR 0016](adr/0016-link-states-and-takedown.md)，用户、组织和权限见 [ADR 0018](adr/0018-users-orgs-and-rbac.md)，可靠后台任务与异步 URL 风控见 [ADR 0020](adr/0020-background-jobs-and-async-risk-scan.md)，审计日志见 [ADR 0023](adr/0023-audit-logs.md)。
- 租户配额和限流见 [ADR 0014](adr/0014-per-tenant-rate-limit-and-quota.md)、[ADR 0015](adr/0015-monthly-link-quota.md)。
- 点击日聚合和统计查询见 [ADR 0012](adr/0012-click-analytics.md)。

## 配置

`config.LoadRedirectorConfig`、`config.LoadConsoleConfig` 与 `config.LoadWorkerConfig` 分别在启动时加载共用基础配置与各自进程的专属配置并校验：

- `ADDR`：跳转进程的监听地址，默认 `:8080`
- `CONSOLE_ADDR`：管理进程的监听地址，默认 `:8081`。不读 `ADDR`，避免和跳转进程抢同一个端口
- `DATABASE_URL`：必填
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`
- `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME`
- `REQUEST_TIMEOUT`：默认 `5s`，单个请求的处理时限，超时返回 504
- `SHUTDOWN_TIMEOUT`：默认 `10s`，优雅退出时等待正在处理的请求的上限
- `DRAIN_DELAY`：默认 `5s`，关闭监听前 `/readyz` 返回 503 的等待，盖住编排器的一个探测周期；`0s` 表示不等
- `CACHE_MISS_CONCURRENCY`：默认 `8`，普通缓存未命中时进程内同时回源数据库的最大请求数，超出返回 503
- `CACHE_FETCH_TIMEOUT`：默认 `1s`，单次回源超时；必须为正且不超过 `REQUEST_TIMEOUT`
- `REDIS_URL`：必填，`redis://` 或 `rediss://`（TLS），可带用户名、密码和 DB 编号；没有密码就是 `redis://localhost:6379/0`。`main` 用它 Ping，连不上则进程退出
- `VISITOR_SALT`：访客哈希的 HMAC 密钥。redirector 启动时必填，为空直接报错；console 不用。生产从密钥管理注入，不能提交真实值
- `TRUST_PROXY_HEADERS`：默认 `false`；为 `true` 时从 `X-Forwarded-For` 最右一段取客户端 IP。只能开在可信反向代理后面。`deploy/nginx.conf` 把这个头覆盖成 `$remote_addr`
- `LOG_LEVEL`：`debug`、`info`、`warn`、`error`，默认 `info`
- `LOG_FORMAT`：`text` 或 `json`，默认 `text`。本地用人读的 text；生产用 `json`，一行一条给采集。两个进程共用这一份构造
- `PPROF_ADDR`：默认空。规则见 [ADR 0013](adr/0013-pprof-on-loopback-listener.md)
- `ACCESS_LOG_SAMPLE_RATE` / `ACCESS_LOG_SLOW_THRESHOLD`：可选，仅 redirector 使用。控制正常 `INFO` 跳转访问日志的采样率（`[0, 1]`，默认 `1`）与慢请求门槛（默认 `200ms`，超门槛及 `5xx` 全量保留）
- `OIDC_ISSUER` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` / `OIDC_REDIRECT_URL`：可选，仅 console 使用。四个要么全配、要么全空（只配一部分时启动失败）；全空时不挂载 OIDC 登录路由（[ADR 0018](adr/0018-users-orgs-and-rbac.md)）
- `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` / `GITHUB_REDIRECT_URL`：可选，仅 console 使用。三个要么全配、要么全空（只配一部分时启动失败）；可与 OIDC 独立启用或同时启用，两者都为空时不挂载登录、登出、/me、自助建组织、成员、邀请与 API Key 管理路由，只支持 API Key（[ADR 0018](adr/0018-users-orgs-and-rbac.md)、[ADR 0021](adr/0021-tenant-self-service-and-web-console.md)）
- `WEB_DIST_DIR`：可选，仅 console 使用。指向 `make web-build` 生成的静态目录后在 `/` 提供最小前端控制台 SPA，同时严格隔离 `/api/*`、`/r/*`、`/healthz`、`/readyz`、`/metrics`（[ADR 0021](adr/0021-tenant-self-service-and-web-console.md)）
- `PUBLIC_BASE_URL`：可选，仅 console 使用。公开短链跳转基地址（如 `https://s.example.com` 或本地 `http://127.0.0.1:8080`），随 `GET /api/v1/auth/providers` 下发给前端拼装可点击短链与二维码；单入口反代时可留空
- `WORKER_ADDR` / `WORKER_CONCURRENCY` / `WORKER_MAX_PER_TENANT` / `WORKER_POLL_INTERVAL` / `WORKER_LEASE_DURATION` / `WORKER_JOB_TIMEOUT` / `WORKER_REAP_INTERVAL` / `WORKER_CLEANUP_INTERVAL` / `WORKER_SESSION_RETENTION` / `WORKER_DONE_JOB_RETENTION` / `RISK_FETCH_TIMEOUT` / `RISK_HOST_RATE_LIMIT` / `RISK_BLOCKED_KEYWORDS`：可选，仅 `console worker` 使用（[ADR 0020](adr/0020-background-jobs-and-async-risk-scan.md)）

## 退出

两个进程都监听 `SIGINT` 和 `SIGTERM`。收到信号后 `/readyz` 先返回 503，等 `DRAIN_DELAY`，再 `Shutdown`；这段时间监听还开着，`/healthz` 仍返回 200，避免编排器把正在退出的进程重启。`Shutdown` 不取消请求的 context，处理时限仍是 `REQUEST_TIMEOUT`。再次收到信号会打断摘流等待和 `Shutdown`。

redirector 在 `run` 返回后排空点击缓冲，时限和丢数见 [ADR 0007](adr/0007-async-click-recording.md)。compose 里两个进程的 `stop_grace_period` 是 30s。这一步只对主动探测 `/readyz` 的编排器生效；`deploy/nginx.conf` 和 `deploy/nginx.stack.conf` 不探这个路径，开源 nginx 也没有主动健康检查，关停时仍会把请求打到已经关掉的监听上。

## 监控

两个进程各自暴露 `GET /metrics`：Go 运行时、进程、HTTP、连接池是两边都有；缓存、存储和点击指标只在会用到它们的进程里产生。抓取不进访问日志、公网不转发运维路径，见 [ADR 0005](adr/0005-prometheus-before-tracing.md)。本地 `make prometheus` 按 `ADDR` 和 `CONSOLE_ADDR` 的端口抓取 `host.docker.internal` 上的 `/metrics`（默认 `8080` 和 `8081`），生成的配置在 `deploy/prometheus.local.yml`；两个 job 的指标名相同，查询时用 `job` 区分。Prometheus 和探活直接打进程端口。宿主机入口：`docker compose --profile edge up -d nginx`，地址 `127.0.0.1:8088`。容器栈：`make stack`，入口 `127.0.0.1:28088`，进程端口 `127.0.0.1:28080` 和 `127.0.0.1:28081`。`make up` 不启动这套栈。

## 尚未实现

- OpenTelemetry 追踪，见 [ADR 0005](adr/0005-prometheus-before-tracing.md)
- Webhook 投递与 PostgreSQL RLS，见 [roadmap](roadmap.md) 阶段 15–16
