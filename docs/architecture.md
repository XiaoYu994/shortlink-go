# 架构

当前结构写在这里：进程、目录、请求怎么走、有哪些配置。为什么这样定写在 [ADR](adr/README.md)，操作和压测写在 [runbook](runbook.md)；决策正文不在这里重复。

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 进程边界

同一个 Go module 产出两个 HTTP 进程，共用 PostgreSQL、Redis 和 `internal/` 里的包，进程之间不调用：

| 进程 | 路由 | 依赖 |
| --- | --- | --- |
| `redirector` | `GET /r/{code}`；配置 `OPS_ADDR` 时 `/healthz`、`/readyz`、`/metrics` 隔离到运维端口，未配置时共端口 | PostgreSQL（只读 `pgredirect`）、可选 Redis（只读 `cache.Reader`，`REDIS_URL` 为空时直连 PG）、可选 `ClickRecorder`（`FEATURE_ANALYTICS=false` 时不启动）；不含 `identity`、`risk`、`job`、`audit` |
| `console` | 始终挂载核心路由：`GET /api/v1/capabilities`、`POST/GET /api/v1/links`、`GET/PATCH/DELETE /api/v1/links/{code}`；按需挂载认证插件 `AuthMethod`（跳转型 `GET /api/v1/auth/{id}/login`·`callback` 与交互型 `POST /api/v1/auth/{id}/send-code`·`verify`，涵盖 OIDC、GitHub 与手机号 OTP）、会话底座（`POST /api/v1/auth/logout`、`GET /api/v1/me`）与 `console.Module` 业务模块（`analytics` -> `/links/{code}/stats`、`orgs`、`members`、`invitations`、`api_keys`、`audit`、`webhooks`）、可选 `WEB_DIST_DIR` 前端静态页面（`/`）；`CONSOLE_OPS_ADDR` 可独立隔离运维端点 | PostgreSQL、可选 Redis（配置 `REDIS_URL` 时同步覆盖写缓存，正缓存 5 分钟 TTL 兜底，[ADR 0031](adr/0031-cache-and-worker-assembly-simplification.md)）、可选 OIDC / GitHub / Phone OTP（`internal/sms`）登录插件、`WEBHOOK_SECRET_KEY`（可选）、`SMTP_HOST`（可选，决定是否入队邀请邮件）；`key`、`tenant`、`member` 与无 Redis 时的 `link` 命令仅连 PostgreSQL（[ADR 0029](adr/0029-modular-feature-plugins.md)、[ADR 0030](adr/0030-phone-otp-and-vertical-console-assembly.md)） |
| `console worker` | 无公开业务路由；配置 `WORKER_ADDR` 时暴露 `/healthz`、`/readyz`、`/metrics` | PostgreSQL（核心维护 `sessions`/`jobs`/`invitations`/`phone_otp_challenges` 清理与 `audit_logs` 分区维护）；在 `cmd/console/feature_*.go` 按垂直特性自包含装配 `WorkerModule` 消费 `link.risk_scan`、`webhook.deliver`（+投递清理）、`invitation.email`、`link.purge_stats`（+统计表分区维护），抢占时按已注册 `kind` 白名单过滤隔离滚动发布未知任务（[ADR 0033](adr/0033-job-claim-kind-filtering.md)），关闭的特性自动注册 `disabledSkipHandler` 按 `job.Skip` 收尾（[ADR 0029](adr/0029-modular-feature-plugins.md)、[ADR 0030](adr/0030-phone-otp-and-vertical-console-assembly.md)、[ADR 0031](adr/0031-cache-and-worker-assembly-simplification.md)）；可选 Redis 与 SMTP |

`redirector` 不注册管理路由，API Key 鉴权只在 `console`。反向代理把 `/r/` 转到 redirector、把 `/api/v1/` 转到 console：`deploy/nginx.conf`（宿主机进程）、`deploy/nginx.stack.conf`（容器栈）。

进程怎么拆、凭证怎么发，见 [ADR 0009](adr/0009-process-split-and-credentials.md)。Redis 故障时的降级和就绪检查见 [ADR 0006](adr/0006-redis-cache-design.md)。

## 目录结构

```txt
shortlink/
├── cmd/redirector/      # 公开跳转进程：GET /r/{code}、/healthz、/readyz、/metrics
├── cmd/console/         # 管理进程：按 feature_*.go 垂直特性自包含装配 serve/worker（ADR 0030），并提供租户、API Key、链接与成员运维命令
├── cmd/migrate/         # 数据库迁移。启动 HTTP 进程时不执行
├── Dockerfile           # redirector、console、migrate 共用的镜像
├── internal/
│   ├── httpx/           # 零业务依赖 HTTP 底座：中间件链、requestid、logging、HTTP 指标、OTel HTTP 入口追踪、recoverer、timeout、BusinessHandler/OpsHandler 独立监听、/healthz、/readyz、/debug/pprof/、/debug/log/level、ClientIP（含可信代理 CIDR 右向剥离）、优雅启停与 JSON/错误写出
│   ├── logx/            # 结构化日志底座：根 slog 构造（text 接入 lmittmann/tint 自动识别 TTY/NO_COLOR、json 保持标准库、service 标签、LevelVar）、WithGroup 顶层 context 字段注入（含 request_id/client_request_id/tenant_id/job_id/job_kind/trace_id/span_id）、RequestEvent 宽事件收集槽与敏感信息脱敏（零业务依赖）
│   ├── tracex/          # OpenTelemetry SDK 与 OTLP/HTTP 导出引导包：全仓唯一持有 otel/sdk 与 exporter，仅由三进程组合根调用，未配置端点时零开销 Noop（ADR 0032）
│   ├── redirector/      # 公开跳转进程 HTTP 层：GET /r/{code}、点击记录与只读 LinkStore、ClickRecorder 接口
│   ├── console/         # 管理进程 HTTP 层：核心短链 CRUD、/api/v1/capabilities、两层登录插件底座（AuthMethod + OAuthAuthMethod + PhoneOTPAuthMethod + SessionIssuer）、console.Module 业务模块（orgs/members/invitations/api_keys/audit/webhooks/analytics）、webhandler、鉴权前有界按 IP 限流与按租户限流；定义各消费方 Store/OAuthClient/Authorizer 接口（ADR 0029、ADR 0030）
│   ├── identity/        # 认证主体（Principal）、API Key 与会话令牌格式/哈希、权限表、MemberScope 与成员规则（零 OAuth/PG 外部库依赖）
│   │   ├── oauth/       # 外部 OAuth/OIDC 客户端实现（go-oidc、golang.org/x/oauth2）
│   │   ├── fakegithub/  # 仅测试用：内存版 GitHub OAuth 2.0 与 REST API 提供方（不进入生产二进制）
│   │   └── fakeoidc/    # 仅测试用：内存版标准 OIDC 提供方（不进入生产二进制）
│   ├── audit/           # 审计日志领域模型、动作枚举、ErrMissingMeta 与 ValidateMeta（零 PG/HTTP 依赖，ADR 0023、0025）
│   ├── link/            # 领域模型：Link、AdminLink、校验、列表和统计类型
│   ├── tenant/          # 租户（组织）领域模型
│   ├── idgen/           # 随机短码生成
│   ├── job/             # 通用可靠后台任务引擎：Worker Pool、指数退避/自定义 RetryAfter、Lease Reaper、周期清理调度与指标（零 PG/Redis 依赖）
│   ├── ratelimit/       # 零业务依赖的泛型按 Key 进程内令牌桶限流器：精确 LRU 有界淘汰与分批惰性过期回收（供 console、safehttp 共用）
│   ├── safehttp/        # 零业务/存储依赖出站安全 HTTP 底座：防 SSRF 拨号校验（含 NAT64/6to4/端口白名单）、禁重定向客户端与基于 ratelimit 的按 host 限速器（供 risk、webhook、console 共用）
│   ├── risk/            # 异步 URL 风控：基于 safehttp 的分层漏斗（L0 黑白名单/关键词、可选 L1 Safe Browsing、L2 防 SSRF 安全抓取 + HTML 净文本提取、可选 L3 OpenAI 兼容 AI 研判）、带规则指纹的 Redis fail-open 缓存与 link.risk_scan 处理器（ADR 0034）
│   ├── webhook/         # 租户 Webhook 订阅校验、AES-256-GCM（tenant_id AAD）密文加解密、HMAC-SHA256 签名校验与 webhook.deliver 处理器（含自动熔断与指标）
│   ├── mailer/          # 仅依赖 Go 标准库的邮件发送器（SMTP StartTLS/SMTPS/Plain 与 CR/LF 注入防护）及邀请邮件正文构造（make arch-check 强制零内部依赖）
│   ├── sms/             # 短信验证码发送通道抽象（Sender）：防 SSRF WebhookSender（经 safehttp）与本地开发 LogSender（make arch-check 强制仅依赖 safehttp/logx，ADR 0030）
│   ├── memorystore/     # 内存存储，仅测试和本地开发；只实现跳转和创建
│   ├── pgdb/            # PostgreSQL 公共连接池参数、约束错误判断、原生 pgx.QueryTracer 追踪与测试库 advisory lock 底座（零业务依赖）
│   ├── pgredirect/      # 跳转数据面专属 PostgreSQL 存储：只读 GetByCode 与 ClickRecorder 批量写出（零管理/身份/风控依赖）
│   ├── postgres/        # PostgreSQL 控制面与存储门面：内嵌 pgredirect.Store，提供带 RLS 事务隔离（tenanttx）的管理查询/CRUD、TxHook 事务内扩展点（风控/Webhook/邀请邮件/统计清理，ADR 0029、ADR 0031）、统计与审计拆分的分区治理、会话、成员邀请、fail-closed 审计写入与 jobs Outbox 队列
│   ├── storemetrics/    # 存储指标装饰器（透传完整读写 Store 与只读 Reader）
│   ├── cache/           # Redis 客户端、读写 Store 与只读 Reader（5 分钟短 TTL 写穿 + 压测验证读路径保护，ADR 0031）
│   ├── click/           # 点击元数据分类和异步批量记录（ClickRecorder、含 recorder_stopped 故障指标）
│   └── config/          # 共用 YAML/Env 分层配置、ProxyConfig 与内置功能开关加载（零业务包依赖；各进程专属配置在 cmd/*/config*.go）
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
cmd/redirector -> redirector, httpx, logx, tracex, pgdb, pgredirect, storemetrics(Reader), cache(Reader), click, config（严格零 identity/risk/job/audit/safehttp/webhook/mailer/sms 依赖，make arch-check 校验）
cmd/console    -> console, httpx, logx, tracex, audit, identity(/oauth), job, link, postgres, risk, safehttp, webhook, mailer, sms, storemetrics, cache, config；key/tenant/member 命令只用 postgres，serve/worker 按 feature_*.go 垂直装配各特性
cmd/migrate    -> golang-migrate
config         -> logx, pgdb, go.yaml.in/yaml/v3, 标准库（零业务包与 OTel 直接引入）
tracex         -> go.opentelemetry.io/otel/sdk, otlptracehttp（全仓唯一持有 OTel SDK，零 internal/* 依赖）
redirector     -> httpx, link, click, idgen, Prometheus
console        -> httpx, logx, audit, link, click, idgen, identity, tenant, ratelimit, safehttp, webhook, sms, Prometheus
httpx          -> logx, 标准库、Prometheus、OTel API
identity       -> tenant, logx（无 OTel 直接引入）
identity/oauth -> identity, logx, golang.org/x/oauth2, github.com/coreos/go-oidc/v3
audit          -> 标准库
job            -> logx, 标准库、Prometheus、OTel API
ratelimit      -> 标准库、golang.org/x/time/rate（严格零 internal/* 与业务/HTTP/存储/OTel 依赖，make arch-check 校验）
safehttp       -> ratelimit, 标准库（严格零业务/存储/cache/config/OTel 依赖）
risk           -> safehttp, cache, audit, job, link, go-redis, golang.org/x/net/html, golang.org/x/time/rate, Prometheus、OTel API
webhook        -> safehttp, audit, job, 标准库、Prometheus、OTel API
mailer         -> 标准库（严格零 internal/* 与第三方依赖，make arch-check 校验）
sms            -> safehttp, logx（严格零 PG/Redis/OAuth 依赖与 OTel 直接引入，make arch-check 校验）
pgdb           -> 标准库、jackc/pgx、OTel API
pgredirect     -> pgdb, link, click
postgres       -> pgdb, pgredirect, audit, link, click, idgen, identity, job, tenant, webhook, mailer、OTel API
memorystore    -> link, idgen
storemetrics   -> link, Prometheus、OTel API
cache          -> link, logx, singleflight, go-redis, Prometheus、OTel API
click          -> link、标准库、Prometheus、OTel API
link           -> 标准库
idgen          -> 标准库
logx           -> 标准库、github.com/lmittmann/tint、OTel trace API
```

`redirector.LinkStore` 与 `console.LinkStore` 接口分别定义在各自使用方，内存版、PostgreSQL 版及装饰器隐式实现它们；`httpx` 不依赖任何业务包。`link` 与 `audit` 不依赖存储和 HTTP。`click` 自己定义写点击的接口，`postgres.Store` 隐式实现它。管理 API 的 `APIKeyAuthenticator`、`SessionStore`、`MemberStore`、`OrgStore`、`InvitationStore`、`APIKeyStore`、`AuditLogStore`、`OAuthClient`（由 `OAuthAuthMethod` 适配为 `AuthMethod`）和 `Authorizer` 定义在 `console`，分别由 `postgres.Store`、`oauth.OIDCProvider`、`oauth.GitHubProvider` 和 `identity.MapAuthorizer` 实现；缓存不参与鉴权。模块化可插拔扩展边界（`postgres.TxHook`、`console.AuthMethod`、`console.Module`、`WorkerModule`）见 [ADR 0029](adr/0029-modular-feature-plugins.md)。

## 请求流程

### 创建短链（console）

```txt
POST /api/v1/links
  -> requestID（始终服务端生成 128 位随机 hex；合法客户端值记入 client_request_id）
  -> observe（客户端 IP 解析 + 访问日志 + HTTP 指标）
  -> recoverer
  -> timeout
  -> ipRateLimit：鉴权前按客户端 IP 有界令牌桶限流（防 401 穿透数据库），超限 429
  -> authenticate：Authorization 优先走 API Key（忽略 sl_session 和 X-Org-ID）；否则校验唯一的 sl_session、CrossOriginProtection 与 X-Org-ID 成员关系，构造 identity.Principal 并注入 audit.WithMeta 放进 context。会话路径的响应带 Cache-Control: no-store
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
  -> observe（客户端 IP 解析 + 访问日志 + HTTP 指标）
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
  -> 解析 limit、sort、status、q、cursor（非法值 400）
  -> admin.List（cache 透传 -> storemetrics -> postgres，多要一条判断下一页）
  -> 200 {"items":[...],"next_cursor":"..."}

GET/PATCH/DELETE /api/v1/links/{code}
  -> authenticate -> rateLimit -> requirePerm -> 校验短码格式
  -> GetForTenant / Update / Delete，全部带 tenant_id，找不到一律 404
  -> PATCH：RawMessage 区分「没传」和「传了 null」，校验 URL、将来时间和 disabled 布尔值
  -> 被封禁的链接 Update/Delete 返回 403 link is banned，其他租户仍然是 404
  -> Update/Delete/Ban/Unban 提交后同步覆盖写缓存（失败按 5 分钟正缓存 TTL 自然收敛，ADR 0031）
  -> Delete 在同一事务内删除 links 主行、写审计日志并入队 link.purge_stats；三张日统计表残余行由 worker 按 PK 元组异步分批清理，不返还本月配额（ADR 0025）

GET /api/v1/links/{code}/stats
  -> authenticate -> rateLimit -> requirePerm -> 解析 from/to（默认 8 天，跨度不超过 90 天）
  -> 只读 REPEATABLE READ 事务：links.clicks + 三张日表
  -> 200：日趋势和访客补零，设备和客户端固定类别
```

`chain` 里靠前的中间件在外层。`requestID` 确定编号，写入响应头 `X-Request-ID` 和请求 context。`observe` 合并了访问日志（Canonical Log Line）、HTTP 指标与 OpenTelemetry 入口追踪（共用单次 `routePattern`、`statusWriter` 与耗时统计；提取入站 W3C `traceparent`，跳过 `/metrics` 与探针 `/healthz`/`/readyz`）：记录方法、路由模板、路径、状态码、毫秒耗时（`duration_ms`）、响应字节数、客户端 `ip`，并从请求内层的 `RequestEvent` 汇总 `tenant_id`、`actor`、`cache`、`retry_after` 与 `err_msg`；级别按 `>=500 -> Error`、慢请求 / 非放行 `4xx -> Warn`、`2xx/3xx` 与 `redirector` 404 -> `Info`、`/healthz`/`/readyz` 2xx -> `Debug`，且 `redirector` 支持正常 `INFO` 流量采样（`5xx` 与慢请求全保）。指标按路由模式、方法和状态码记录请求数和耗时，方法为 GET、HEAD、POST、PATCH、DELETE，其余记成 `other`。`observe` 放在 `recoverer` 外面，panic 被写成 500 后也能记到状态码和 `err_msg=panic`。各进程根日志器统一通过 `logx` 装配，本地 `text` 模式使用 `lmittmann/tint` 彩色可读格式（非终端或设 `NO_COLOR` 自动去色），生产 `json` 模式输出严格 JSON；带 context 的日志即使经过 `WithGroup` 也会在顶层自动带上 `request_id`、`tenant_id`、`job_id`、`job_kind`、`trace_id` 和 `span_id`，并自动脱敏连接串密码与密钥；启用回环 `PPROF_ADDR` 时同时暴露 `GET/PUT /debug/log/level` 支持动态调级。`http.ErrAbortHandler` 会继续抛出，那是 `net/http` 用来中止当前连接的哨兵。

`timeout` 在最里层，给请求 context 加处理时限。到点后 handler 里的数据库查询被取消（pgx 会中断正在执行的 SQL），handler 返回 504。`http.Server` 的 `ReadTimeout`、`WriteTimeout` 只管连接的读写，不会取消 handler 的 context，处理时限只能靠这一层。这两个连接超时都是 `REQUEST_TIMEOUT` 加 5 秒：读超时包住请求体，写超时留给 504 的写出。

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发（[ADR 0004](adr/0004-storage-package-layout.md)）。
- `internal/postgres`：`database/sql` + pgx，生产使用。连接池和迁移见 [ADR 0003](adr/0003-postgres-and-pool-config.md)。
- `internal/storemetrics`：存储耗时装饰器。组装顺序和结果分类见 [ADR 0005](adr/0005-prometheus-before-tracing.md)、[ADR 0014](adr/0014-per-tenant-rate-limit-and-quota.md)、[ADR 0016](adr/0016-link-states-and-takedown.md)。操作有 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`、`ban`、`unban`。
- `internal/cache`：`GetByCode` 的 Redis 读缓存。值、TTL（正缓存 5 分钟 / 负缓存 1 分钟）、singleflight、故障降级和指标见 [ADR 0006](adr/0006-redis-cache-design.md)，冷缓存回源保护见 [ADR 0008](adr/0008-cold-cache-read-path.md)，管理写和回源的两种 `SET` 见 [ADR 0011](adr/0011-link-crud-api.md)，写路径减法与短 TTL 取舍见 [ADR 0031](adr/0031-cache-and-worker-assembly-simplification.md)。
- `internal/click`：点击分类和异步批量写出。投递和关停见 [ADR 0007](adr/0007-async-click-recording.md)，日聚合、维度和访客哈希见 [ADR 0012](adr/0012-click-analytics.md)。
- 进程指标和连接池指标见 [ADR 0005](adr/0005-prometheus-before-tracing.md)。
- 表 `links`、`tenants`、`api_keys`、`users`、`user_identities`、`memberships`、`sessions`、`invitations`、`phone_otp_challenges`、`jobs`、`webhooks`、`webhook_deliveries`、按月范围分区表 `link_clicks_daily` / `link_dimension_daily` / `link_visitors_daily` / `audit_logs` 的列与分区定义以 `migrations/` 为准。短码见 [ADR 0002](adr/0002-random-short-codes.md)，不建外键见 [ADR 0010](adr/0010-app-level-referential-integrity.md)，API Key 和运维命令见 [ADR 0009](adr/0009-process-split-and-credentials.md)，停用和封禁见 [ADR 0016](adr/0016-link-states-and-takedown.md)，用户、组织和权限见 [ADR 0018](adr/0018-users-orgs-and-rbac.md)，可靠后台任务与异步 URL 风控见 [ADR 0020](adr/0020-background-jobs-and-async-risk-scan.md) 及按 `kind` 抢占隔离见 [ADR 0033](adr/0033-job-claim-kind-filtering.md)，审计日志与 fail-closed 元数据约束见 [ADR 0023](adr/0023-audit-logs.md)、[ADR 0025](adr/0025-reconcile-cache-async-purge-and-audit-hardening.md)，时序按月分区与自动淘汰见 [ADR 0026](adr/0026-timeseries-partitioning-and-retention.md)，Webhook 订阅投递与异步邀请邮件见 [ADR 0027](adr/0027-webhooks-and-async-invitation-email.md)，PostgreSQL 行级安全（RLS）第二道租户隔离防线（`shortlink_tenant` 角色与 `SET LOCAL` 事务上下文）见 [ADR 0028](adr/0028-postgres-rls-tenant-isolation.md)。
- 租户配额和限流见 [ADR 0014](adr/0014-per-tenant-rate-limit-and-quota.md)、[ADR 0015](adr/0015-monthly-link-quota.md)。
- 点击日聚合和统计查询见 [ADR 0012](adr/0012-click-analytics.md)。

## 配置

配置采用**「代码默认值 < 可选共享 YAML（`--config` 优先于 `SHORTLINK_CONFIG`） < 环境变量」**分层模型（[ADR 0035](adr/0035-layered-config-and-secret-separation.md)）：`config.example.yaml` 承载结构化调优与策略参数，`.env` / 环境变量仅保留 10 个连接串与密钥及 13 个高频运行期覆盖（未指定 YAML 时可零配置按默认值启动）。`config.LoadBaseConfig` 与各进程入口的 `cmd/redirector/config.go`（`loadRedirectorConfig`）、`cmd/console/config.go`（`loadConsoleConfig`）及 `cmd/console/config_worker.go`（`loadWorkerConfig`）分别按 `yaml.Node` 延迟解码公共节与自身专属节，保持 `redirector` 零控制面依赖（可通过 `console config dump` 校验并脱敏输出生效配置）：

- **Env-only 连接串与密钥（10 个，禁止写入 YAML）**：
  - `DATABASE_URL`：必填，PostgreSQL 连接串
  - `REDIS_URL`：可选，`redis://` 或 `rediss://`（TLS），留空时跳过缓存层直连 PostgreSQL（[ADR 0029](adr/0029-modular-feature-plugins.md)、[ADR 0031](adr/0031-cache-and-worker-assembly-simplification.md)）
  - `VISITOR_SALT`：访客哈希 HMAC 密钥；`analytics=true` 时 redirector 启动必填，关闭统计时可留空
  - `OIDC_CLIENT_SECRET` / `GITHUB_CLIENT_SECRET` / `SMS_WEBHOOK_TOKEN`：登录认证插件外部密钥
  - `WEBHOOK_SECRET_KEY`：64 位小写 hex 主密钥；配置后启用 Webhook 订阅管理（若同时启用用户登录）、事务内扇出与后台投递（[ADR 0027](adr/0027-webhooks-and-async-invitation-email.md)）
  - `SMTP_PASSWORD`：SMTP 认证密码（与 YAML `smtp.username` 同开或同空）
  - `RISK_GSB_API_KEY` / `RISK_LLM_API_KEY`：Google Safe Browsing v4 与 OpenAI 兼容 LLM 风控 API 密钥
- **配置入口与高频运行期覆盖（13 个，非空时覆盖 YAML/默认值）**：
  - `SHORTLINK_CONFIG`：可选共享 YAML 文件路径（CLI `--config` 优先级更高）
  - `ADDR` / `OPS_ADDR`：跳转进程业务监听地址（默认 `:8080`）与可选独立运维端口（[ADR 0024](adr/0024-process-boundary-and-ops-isolation.md)）
  - `CONSOLE_ADDR` / `CONSOLE_OPS_ADDR`：管理进程业务监听地址（默认 `:8081`）与可选独立运维端口（不读 `ADDR`）
  - `WORKER_ADDR`：后台任务进程可选运维端口（默认空不开启）
  - `LOG_LEVEL`（`debug`/`info`/`warn`/`error`，默认 `info`）/ `LOG_FORMAT`（`text`/`json`，默认 `text`）
  - `DRAIN_DELAY`（对应 `http.drain_delay`，默认 `5s`，关闭监听前 `/readyz` 返回 503 的等待；`0s` 表示不等）
  - `TRUSTED_PROXIES`（对应 `trusted_proxies`，默认空不信任转发头；`*` 表示单跳反代；逗号分隔 CIDR 列表表示多跳右向剥离，[ADR 0024](adr/0024-process-boundary-and-ops-isolation.md)）
  - `OTEL_EXPORTER_OTLP_ENDPOINT`（对应 `tracing.endpoint`，默认空关闭 OTLP/HTTP Traces；采样率由 `tracing.sample_rate` 控制，默认 `1`，[ADR 0032](adr/0032-opentelemetry-tracing-and-log-correlation.md)）
  - `FEATURE_ANALYTICS` / `FEATURE_RISK_SCAN`（对应 `features.analytics` / `features.risk_scan`，默认全开 `true`，[ADR 0029](adr/0029-modular-feature-plugins.md)）
- **YAML 结构化与低频调优节（见 [config.example.yaml](../config.example.yaml)）**：
  - `database.*`（连接池）、`http.*`（`request_timeout` 默认 `5s`、`shutdown_timeout` 默认 `10s`、`drain_delay`）、`cache.fetch_timeout`（默认 `1s`）、`outbound.allowed_ports`（统一供 `risk`/`webhook`/`sms` 共用的出站白名单端口，默认空即 `80,443`）
  - `redirector.*`：`pprof_addr`（[ADR 0013](adr/0013-pprof-on-loopback-listener.md)）、`cache_miss_concurrency`（默认 `8`）、`access_log_sample_rate`（默认 `1`）、`access_log_slow_threshold`（默认 `200ms`）
  - `console.*`：`public_url`（**单一事实来源**，自动推导 OIDC/GitHub `/api/v1/auth/{id}/callback` 回调、Phone OTP `Secure` Cookie 与邀请邮件登录链接）、`public_base_url`（前端短链/二维码基址）、`web_dist_dir`（[ADR 0021](adr/0021-tenant-self-service-and-web-console.md)）、`ip_rate_limit` / `ip_rate_burst`（鉴权前按 IP 限流，默认 `60`/`120`）、`auth.oidc`、`auth.github`、`auth.phone_otp`（[ADR 0030](adr/0030-phone-otp-and-vertical-console-assembly.md)）
  - `worker.*`：调度参数（`concurrency`、`max_per_tenant`、`poll_interval`、`lease_duration`、`job_timeout`、`reap_interval`、`cleanup_interval`）、4 档清理保留期 `retention.{short, long, stats_days, audit_days}`（`short` 默认 `168h` 覆盖 sessions/done_jobs/webhook_deliveries、`long` 默认 `720h` 覆盖 dead_jobs/invitations；OTP 固定 24h，[ADR 0026](adr/0026-timeseries-partitioning-and-retention.md)）、原生 YAML 规则映射 `risk.{allowed_hosts, blocked_hosts, blocked_keywords}` 与 `gsb` / `llm`（[ADR 0034](adr/0034-layered-risk-scan-and-ai-judge.md)）、`webhook.*` 与顶层 `smtp.*`

## 退出

两个进程都监听 `SIGINT` 和 `SIGTERM`。收到信号后 `/readyz` 先返回 503，等 `http.drain_delay` / `DRAIN_DELAY`，再 `Shutdown`；这段时间监听还开着，`/healthz` 仍返回 200，避免编排器把正在退出的进程重启。`Shutdown` 不取消请求的 context，处理时限仍是 `http.request_timeout`。再次收到信号会打断摘流等待和 `Shutdown`。

redirector 在 `run` 返回后排空点击缓冲，时限和丢数见 [ADR 0007](adr/0007-async-click-recording.md)。compose 里两个进程的 `stop_grace_period` 是 30s。这一步只对主动探测 `/readyz` 的编排器生效；`deploy/nginx.conf` 和 `deploy/nginx.stack.conf` 不探这个路径，开源 nginx 也没有主动健康检查，关停时仍会把请求打到已经关掉的监听上。

## 监控

两个进程各自暴露 `GET /metrics`：Go 运行时、进程、HTTP、连接池是两边都有；缓存、存储和点击指标只在会用到它们的进程里产生。抓取不进访问日志、公网不转发运维路径，见 [ADR 0005](adr/0005-prometheus-before-tracing.md)。本地 `make prometheus` 按 `OPS_ADDR`/`ADDR`、`CONSOLE_OPS_ADDR`/`CONSOLE_ADDR` 和 `WORKER_ADDR` 的端口抓取 `host.docker.internal` 上的 `/metrics`（默认 `8080`、`8081`、`8082`），生成的配置在 `deploy/prometheus.local.yml`；各 job 指标名相同，查询时用 `job` 区分。Prometheus 和探活直接打运维端口。宿主机入口：`docker compose --profile edge up -d nginx`，地址 `127.0.0.1:8088`。容器栈：`make stack`，入口 `127.0.0.1:28088`，业务端口映射 `127.0.0.1:28080` / `28081`（运维端点留在 compose 内网 `:9090` / `:9091`），`worker` 运维端口映射 `127.0.0.1:28082`。`make up` 不启动这套栈。

链路追踪默认关闭；配置 `OTEL_EXPORTER_OTLP_ENDPOINT` 后，三进程通过 `internal/tracex` 以 OTLP/HTTP 上报 Span，同步请求涵盖 `httpx`、`cache`（含 `singleflight` 等待者 Span Link）、`storemetrics` 与 `pgdb`（`pgx.QueryTracer`），异步 `jobs` 持久化 `traceparent` 并由 `worker` 以 `SpanKindConsumer` + Span Link 关联源请求；`logx` 自动将 `trace_id`/`span_id` 注入结构化日志（[ADR 0032](adr/0032-opentelemetry-tracing-and-log-correlation.md)）。本地可用 `make tracing` 按需启动 Jaeger。
