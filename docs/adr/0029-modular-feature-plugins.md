# 0029. 模块化特性插件架构：核心内核与可开关外围能力

- 状态：Accepted
- 日期：2026-11-25
- 相关：`0004`, `0006`, `0007`, `0009`, `0011`, `0012`, `0016`, `0018`, `0019`, `0020`, `0021`, `0022`, `0023`, `0024`, `0025`, `0026`, `0027`, `0028`

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 架构形态 | 采用 Go 风格的 **微内核 / 模块化单体（Modular Monolith）**：启动期在 `cmd/` 组合根显式装配内置模块，不做运行期 `.so` 动态加载或 `init()` 隐式注册 |
| 核心 vs 插件边界 | **核心常驻**：短链 CRUD 与 302 跳转、租户与 RLS（ADR 0028）、链接状态机、月度配额、API Key 鉴权与 RBAC、**审计日志（fail-closed，ADR 0025）**；**可开关模块**：异步 URL 风控、Webhook、异步邀请邮件、点击统计分析、Redis 读缓存、可插拔登录方式（OIDC / GitHub / 未来手机号 OTP 等）、Web 前端托管 |
| 开关单一事实来源 | 有专属外部凭证/依赖的模块沿用“配置存在即启用”（`REDIS_URL`、`WEBHOOK_SECRET_KEY`、`SMTP_HOST`、`OIDC_*`、`GITHUB_*`、`WEB_DIST_DIR`）；无独立凭证的纯内置扩展使用显式布尔开关且默认 `true`（`FEATURE_RISK_SCAN`、`FEATURE_ANALYTICS`），保持合理默认体验 |
| 存储写路径解耦 | 基于提交 `1ea9258` 已完成拆分的单职责事务步骤，在 `internal/postgres` 内引入事务内领域事件钩子 `TxEvent` / `TxHook`（对标 Spring `BEFORE_COMMIT` 事务事件）；SQL 仍全部收敛在 `internal/postgres`，装配权上移至 `cmd/console` 共享构造函数（`serve`/`worker`/CLI 三入口一致） |
| 认证插件两层抽象 | 在 `internal/console` 抽象通用登录插件接口 `AuthMethod` + `SessionIssuer`（统一收敛建会话、种 Cookie 与审计），支持跳转型（OAuth/OIDC）与交互校验型（如未来手机号短信验证码 OTP、邮箱验证码）；另提供 `OAuthAuthMethod` 公共模板适配器复用 OAuth/OIDC 的状态 Cookie、PKCE 与回调编排，并统一跳转路由为 `/api/v1/auth/{id}/login` 与 `/api/v1/auth/{id}/callback` |
| 控制面与能力发现 | 在 `internal/console` 引入窄接口 `Module` 收口可选路由；用语义准确的 **`GET /api/v1/capabilities`** 直接替代旧的 `/api/v1/auth/providers`（下发 `auth_methods` 列表与 `features` 能力清单，同步迁移前端 `web/`、冒烟脚本与测试，不留双存字段或旧路由壳） |
| Worker 与存量任务 | 在 `cmd/console/worker.go` 引入 `WorkerModule` 打包 `job.Handler` 与 `PeriodicTask`；关闭内置异步插件后，为对应已知 `kind` 注册 `disabledSkipHandler`（返回 `job.Skip`），不修改 `claimJobsFairSQL` 索引结构，避免死信告警与 30s `Defer` 空转 |
| 关闭统计的接口契约 | `FEATURE_ANALYTICS=false` 时不挂载 `/api/v1/links/{code}/stats`，列表/详情 SQL 跳过 `link_clicks_daily` 连接，响应 JSON 直接省略 `clicks` 与 `clicks_today`（`*int64` + `omitempty`，不用假 `0` 撒谎），`sort=clicks` 与 `sort=clicks_today` 显式返回 `400 Bad Request` |

---

## 背景

经过 16 个阶段演进，`shortlink` 已经从最初的“创建短链 + 302 跳转”成长为包含 Redis 缓存、异步点击分析、多租户 RBAC、OIDC/GitHub 登录、异步 URL 风控、审计日志、Webhook 投递、异步邀请邮件与月分区治理的完整 SaaS 服务。

虽然各领域包本身依赖边界清晰（由 `Makefile` 的 `arch-check` 守门），且提交 `1ea9258` 已将 `internal/postgres` 中多业务域巨型写 CTE 拆分为事务内单职责步骤，但外围能力仍在四个聚合点与核心路径硬绑定：

1. **`internal/postgres` 既当核心仓储，又在事务末尾硬编码全部外围副作用**：
   - [`internal/postgres/store.go`](../../internal/postgres/store.go) 与 [`internal/postgres/admin.go`](../../internal/postgres/admin.go) 的 `Create`/`Update`/`Delete`/`Ban`/`Unban` 在各自事务里直接硬编码调用了风控入队（`link.risk_scan`）、缓存对账（`cache.reconcile`）、统计清理（`link.purge_stats`）与 Webhook 扇出（`enqueueWebhookEventTx`）；[`internal/postgres/invitation.go`](../../internal/postgres/invitation.go) 的 `CreateInvitation` 同样硬编码入队 `invitation.email`；
   - 现有“半开关”存在隐患：例如未配 `WEBHOOK_SECRET_KEY` 时虽关闭了 `/api/v1/webhooks*` 路由，但写路径依然会查 `webhooks` 表扇出任务（若“先开后关”留有历史订阅，worker 会触发 30 秒 `job.Defer` 无限空转）；未配 `SMTP_HOST` 时 `CreateInvitation` 仍无条件往 `jobs` 表插 `invitation.email` 任务；
   - 新增外围能力必须反复改动核心表写方法，关闭外围能力也无法消除其对核心事务的写放大。
2. **认证流程按提供方硬编码，无法平滑承接非跳转型登录（如手机号验证码）**：
   - [`internal/console/authhandler.go`](../../internal/console/authhandler.go) 与 [`server.go`](../../internal/console/server.go) 直接把 `OIDCClient`、`GitHubClient` 写死为两个结构体字段和两组路由；
   - 后续接入**手机号短信验证码（Phone OTP）**等非 OAuth 跳转类登录时，其 HTTP 协议形态是“发验证码 `POST` + 提交校验 `POST`”，而不是“302 跳转外部 IdP + 回调 `GET`”。如果直接把登录抽象成单一的 OAuth 接口，接入手机号登录时必然再次大改。
3. **控制面 `console.Server` 字段膨胀，能力发现端点名不副实**：
   - [`internal/console/server.go`](../../internal/console/server.go) 与 [`routes.go`](../../internal/console/routes.go) 持续堆积可选 handler 字段与 `if s.xxx != nil` 分支；
   - `GET /api/v1/auth/providers` 原本只报告登录提供方，后续陆续塞入 `public_base_url` 与 `webhooks`，既语义失真又缺少统一能力清单；前端 [`ConsoleLayout.tsx`](../../web/src/app/ConsoleLayout.tsx) 侧边栏入口写死，仅部分页面进入后才判断后端能力是否开启。
4. **数据面与基础设施无法渐进退化**：
   - [`internal/redirector/server.go`](../../internal/redirector/server.go) 已经允许 `Clicks == nil` 跳过点击记录，但 [`cmd/redirector/config.go`](../../cmd/redirector/config.go) 强制要求 `VISITOR_SALT` 且无条件启动 `ClickRecorder`；
   - [`internal/config/config.go`](../../internal/config/config.go) 强制要求 `REDIS_URL`，单机/极简部署无法仅凭 PostgreSQL 启动。

我们需要在不破坏核心业务不变量与热路径性能的前提下，把外围能力演进为可清晰开关的模块化插件架构；同时坚持**真兼容与伪兼容分离**：守住外部已发布短链 `/r/{code}` 与跨进程滚动窗口的队列兼容，绝不为同仓库内部自产自销的前端、脚本或旧单测保留双存字段与兼容壳。

---

## 决策

### 1. 核心不变量 vs 可开关模块边界

#### 1.1 不可关闭的核心内核（Core Kernel）

以下能力构成短链服务的业务底线与安全边界，始终常驻启用：

- **短链核心域**：`links` 创建/查询/修改/删除、状态机（`active`/`expired`/`disabled`/`banned` 与 `Servable`）、`GET /r/{code}` 302 跳转；
- **多租户与安全隔离**：租户模型、PostgreSQL RLS 双防线（[ADR 0028](0028-postgres-rls-tenant-isolation.md)）、API Key 鉴权与 `Authorizer` RBAC（策略可替换实现，但不可关闭鉴权）；
- **月度创建配额**：保留核心防滥用口径（[ADR 0015](0015-monthly-link-quota.md)）；
- **审计日志（`internal/audit`）**：刻意不提供关闭开关。审计承担多租户合规追责职责，且已按 [ADR 0025](0025-reconcile-cache-async-purge-and-audit-hardening.md) 收口为与业务同事务的 `fail-closed`（`audit.ErrMissingMeta`）；允许关闭会破坏整个写路径的元数据安全不变量，得不偿失；
- **后台任务队列底座（`internal/job`）与基础清理**：当部署 `console worker` 时，`sessions`/`jobs`/`invitations` 过期清理与 `audit_logs` 月分区维护作为平台基础治理任务常驻（极简部署若无需任何后台能力，可直接不部署 `console worker` 进程）。

#### 1.2 可开关的外围模块清单

| 模块 | 开关来源（单一事实来源） | 默认行为 | 影响的扩展点 |
| --- | --- | --- | --- |
| **Redis 读缓存** (`cache`) | `REDIS_URL` 非空即启用 | 留空即关，有值即开 | `redirector`/`console`/`worker`/CLI 存储装饰链、`CacheReconcileHook`、`cache.reconcile` worker |
| **点击统计分析** (`analytics`) | `FEATURE_ANALYTICS`（默认 `true`） | 开启 | `redirector` `ClickRecorder` & `VISITOR_SALT` 校验、`console` `/stats` 路由与列表 `todayJoin`/`sort=clicks*`、`StatsPurgeHook`、`link.purge_stats` 与 stats 分区维护 |
| **异步 URL 风控** (`risk_scan`) | `FEATURE_RISK_SCAN`（默认 `true`） | 开启 | `RiskScanHook`、`link.risk_scan` worker |
| **Webhook 订阅与投递** (`webhooks`) | 管理 API 需 `WEBHOOK_SECRET_KEY` 非空且启用至少一种用户登录方式（`console`）；事件扇出与后台投递仅需 `WEBHOOK_SECRET_KEY` 非空（`console`/`worker`/CLI） | 未配则关 | `console` Webhook 模块路由、`WebhookFanoutHook`、`webhook.deliver` 与 `webhooks.cleanup_deliveries` |
| **异步邀请邮件** (`email`) | `SMTP_HOST` 非空即启用（同时要求 `SMTP_FROM` 非空） | 未配则关 | `InvitationEmailHook`、`invitation.email` worker |
| **用户登录方式** (`auth_methods`) | 各提供方配置存在即启用（`OIDC_*`、`GITHUB_*`、未来 `SMS_*` 等） | 未配则关 | `console` `AuthMethod` 注册表、会话鉴权与自助治理模块（有任意登录方式时自动激活） |
| **Web 前端托管** (`web`) | 沿用现有 `WEB_DIST_DIR` 非空即开 | 未配则关 | `console` `/` SPA 路由 |

**原则**：绝不为已经有专属凭证的模块（如 `WEBHOOK_SECRET_KEY`、`SMTP_HOST`、`REDIS_URL`）再额外造一份布尔变量，杜绝配置冲突。各进程启动日志统一打印 `enabled_features` / `disabled_features` 与 `enabled_auth_methods` 便于排障。

---

### 2. 扩展点一：存储事务内领域事件钩子（`TxEvent` / `TxHook`）

对照 Java Spring 的 `@TransactionalEventListener(phase = BEFORE_COMMIT)`，我们在 Go 中用显式函数钩子实现**事务内强一致扩展**，并守住 [ADR 0004](0004-storage-package-layout.md) 的包边界（所有 PostgreSQL SQL 继续留在 `internal/postgres`，业务域包保持零 `database/sql` 依赖）：

1. **直接复用 `1ea9258` 的单职责事务结构**：
   - `Create`/`Update`/`Delete`/`CreateInvitation` 在 `withTenantTx`（RLS 生效）中完成主表写与必选审计写，随后调用 `s.dispatchTxHooks(ctx, tx, ev)`；
   - `Ban`/`BanIfURL`/`Unban` 为平台级按 `code` 操作（不传 `tenantID`），在显式 `s.db.BeginTx` 中锁行更新并写审计，随后调用 `s.dispatchTxHooks(ctx, tx, ev)`。
2. **定义极简事务事件与钩子类型**（位于 `internal/postgres`）：
   ```go
   type TxEvent interface{ isTxEvent() }

   type TxHook func(ctx context.Context, tx *sql.Tx, ev TxEvent) error
   ```
   首批事件覆盖全部跨模块写边界：`LinkCreatedEvent`、`LinkUpdatedEvent`、`LinkDeletedEvent`、`LinkBannedEvent`、`LinkUnbannedEvent`、`InvitationCreatedEvent`。
3. **提供 5 个内建、可独立插拔的 Hook 构造函数**：
   - `RiskScanHook()`：`LinkCreatedEvent` 与 `LinkUpdatedEvent(URLChanged=true)` 时写入 `link.risk_scan`；
   - `WebhookFanoutHook()`：Link 5 类状态变更事件时扇出 `webhook.deliver`；
   - `InvitationEmailHook()`：`InvitationCreatedEvent` 时写入 `invitation.email`；
   - `CacheReconcileHook()`：`LinkUpdatedEvent` / `LinkDeletedEvent` / `LinkBannedEvent` / `LinkUnbannedEvent` 时写入 `cache.reconcile`；
   - `StatsPurgeHook()`：`LinkDeletedEvent` 时写入 `link.purge_stats`。
4. **多入口一致装配**：
   - `postgres.NewStore(db, opts...)` 支持函数选项（如 `WithTxHooks(...)`、`WithAnalytics(enabled)`）；
   - 在 `cmd/console` 抽出共享装配函数，让 `console serve`、`console worker`（风控 `BanIfURL`）与 `console` CLI（`link ban/unban`、`member invite`）从同一份环境配置推导并挂载相同的 `TxHook`，杜绝某入口漏发 Webhook 或缓存对账事件。

---

### 3. 扩展点二：两层认证插件抽象（`AuthMethod` + `OAuthAuthMethod`）

所有登录方式（OIDC、GitHub OAuth、未来手机号短信验证码、邮箱验证码、Passkey）虽然前半段的 HTTP 交互协议迥异，但**身份核验通过后的下半段完全相同**：产出标准化的 `identity.ExternalIdentity{Issuer, Subject, ...}` → 走 [`CreateSessionForIdentity`](../../internal/postgres/session.go) 落库建会话 → 写 `sl_session` HttpOnly Cookie → 共享 `/api/v1/me`、`/api/v1/auth/logout` 与会话鉴权中间件。

注意 `user_identities (issuer, subject)` 唯一键与 [`identity.ExternalIdentity.Normalize()`](../../internal/identity/session.go) 本身只要求 `Issuer` 与 `Subject` 必填、`Email` 可为空，天然兼容手机号身份（例如未来 `Issuer: "urn:shortlink:phone"`, `Subject: "+8613800138000"`）。

因此认证插件按“**通用契约 + 协议模板**”分两层设计：

#### 3.1 第一层：协议无关的 `AuthMethod` 插件接口（面向全部登录形态）

```go
type AuthFlowKind string

const (
    AuthFlowRedirect AuthFlowKind = "redirect"  // OIDC、GitHub、Google、飞书等浏览器跳转授权
    AuthFlowPhoneOTP AuthFlowKind = "phone_otp" // 预留：手机号短信验证码等表单校验形态
)

type AuthMethodDescriptor struct {
    ID       string       `json:"id"`                  // 如 "oidc"、"github"、"phone"
    Kind     AuthFlowKind `json:"kind"`                // 决定前端如何渲染该登录方式
    LoginURL string       `json:"login_url,omitempty"` // redirect 类直接下发跳转入口路径
}

type IssueSessionOptions struct {
    SecureCookie bool
    ReturnTo     string // 非空时 302 跳转至已校验站内路径；为空时返回 loginResponse JSON
}

// SessionIssuer 把“认证成功后的统一建会话、审计上下文与写 Cookie”下沉到认证底座，
// 各登录插件只负责核验凭证并产出 identity.ExternalIdentity。
type SessionIssuer interface {
    IssueSession(w http.ResponseWriter, r *http.Request, id identity.ExternalIdentity, opts IssueSessionOptions)
}

type AuthToolkit struct {
    Mux         *http.ServeMux
    IPRateLimit func(http.Handler) http.Handler
    CSRF        *http.CrossOriginProtection
    Issuer      SessionIssuer
    Logger      *slog.Logger
}

type AuthMethod interface {
    Descriptor() AuthMethodDescriptor
    SecureCookies() bool
    RegisterRoutes(tk AuthToolkit)
}
```

- `authHandler` 只持有 `sessions SessionManager`、`methods []AuthMethod`（按注册顺序保序且校验 `ID` 唯一）、`csrf` 与 `sessionTTL`，不再出现任何 `oidc` / `github` 硬编码字段；
- `len(methods) > 0` 即表示启用用户会话能力，自动挂出 `/api/v1/me`、`/api/v1/auth/logout` 及组织自助治理模块；`logoutSecureCookies()` 由所有已注册 `methods` 汇总推导；
- 未来接入手机号验证码登录时，只需新增一个实现 `AuthMethod` 的 `phoneOTPMethod`（自行挂 `POST /api/v1/auth/phone/send-code` 与 `POST /api/v1/auth/phone/verify`，复用 `tk.IPRateLimit`、`tk.CSRF` 和 `tk.Issuer.IssueSession`），核心会话层零改动。

#### 3.2 第二层：跳转型 OAuth/OIDC 公共模板 `OAuthAuthMethod`（模板方法模式）

为避免 OIDC、GitHub 及未来 Google/飞书重复编写状态 Cookie 签名、PKCE、state 常量时间比对与错误映射，将现有 [`authhandler.go`](../../internal/console/authhandler.go) 中的 `handleLoginFlow` / `handleCallbackFlow` 抽成统一的 `OAuthAuthMethod` 适配器，面向极简的 `OAuthClient` 接口：

```go
type OAuthClient interface {
    AuthCodeURL(state, codeVerifier, nonce string) string
    Exchange(ctx context.Context, code, codeVerifier, expectedNonce string) (identity.ExternalIdentity, error)
    RequiresNonce() bool
    SecureCookies() bool
    FlowCookieKey() []byte
}
```

- `oauth.OIDCProvider`（`RequiresNonce() == true`）与 `oauth.GitHubProvider`（`RequiresNonce() == false`）统一满足 `OAuthClient`，彻底合并原先的 `OIDCClient` 与 `GitHubClient` 双接口；
- 统一所有跳转型登录路由命名空间为 **`GET /api/v1/auth/{id}/login`** 与 **`GET /api/v1/auth/{id}/callback`**（即 OIDC 路由由 `/api/v1/auth/login`·`/callback` 统一为 `/api/v1/auth/oidc/login`·`/api/v1/auth/oidc/callback`，同步更新 `deploy/dex.yaml`、`.env.example`、`web/`、`scripts/smoke.sh` 与 `docs/`，消除 OIDC 的历史特殊路径）。

---

### 4. 扩展点三：`console` 模块注册与 `GET /api/v1/capabilities`

1. **窄接口 `console.Module`**：
   ```go
   type RouteRegistrar struct {
       Mux         *http.ServeMux
       IPRateLimit func(http.Handler) http.Handler
       Console     func(perm identity.Perm, h http.HandlerFunc) http.Handler
   }

   type Module interface {
       Name() string
       RegisterRoutes(r RouteRegistrar)
   }
   ```
   - 将 `webhooks`、`audit`、`invitations`、`api-keys`、`members`、`orgs`、`stats` 各自收口为实现 `Module` 的小结构体；
   - `console.Server` 不再持有 7 个可选业务 handler 字段，只需持有核心依赖（`store`、`admin`、认证与限流底座）和 `modules []Module`，显著缩减 `console.Options` 与 `serverDeps`。
2. **用 `GET /api/v1/capabilities` 直接替代 `GET /api/v1/auth/providers`**：
   - 本仓库所有调用方均为同仓可控代码（`web/`、`scripts/smoke.sh`、单测与 `docs/api.md`），无外部不可控消费方；直接替换并删除旧路由，不保留双存字段或旧路由兼容壳（遵守 `AGENTS.md` 反模式红线 #1、#2）；
   - 响应结构按单一事实来源从已注册 `AuthMethod` 与 `Module` 自动推导：
     ```json
     {
       "auth_methods": [
         { "id": "oidc", "kind": "redirect", "login_url": "/api/v1/auth/oidc/login" },
         { "id": "github", "kind": "redirect", "login_url": "/api/v1/auth/github/login" }
       ],
       "features": {
         "analytics": true,
         "webhooks": true,
         "risk_scan": true,
         "email": false
       },
       "public_base_url": "http://127.0.0.1:8080"
     }
     ```
   - 前端 `web/` 依据 `auth_methods` 动态渲染登录页按钮/表单（不再硬编码跳转 URL），依据 `features` 动态控制导航栏入口（关闭 Webhooks 时隐藏菜单）和短链页控件（关闭 Analytics 时隐藏点击排序与“查看统计”入口）。

---

### 5. 扩展点四：`worker` 模块注册与存量任务关闭策略

1. **`WorkerModule` 打包注册**：
   - 每个启用的异步模块统一注册自己的 `job.Handler` 与配套 `job.PeriodicTask`：
     - `RiskModule` → `link.risk_scan`；
     - `WebhookModule` → `webhook.deliver` + `webhooks.cleanup_deliveries`；
     - `EmailModule` → `invitation.email`；
     - `CacheModule` → `cache.reconcile`；
     - `AnalyticsModule` → `link.purge_stats` + Stats 月分区维护；
   - 将 [`internal/postgres/partition.go`](../../internal/postgres/partition.go) 中的 `EnsureMonthlyPartitions` 拆为 `EnsureStatsPartitions` 与 `EnsureAuditPartitions`，使统计分区生命周期完全归属 `AnalyticsModule`。
2. **“先开后关”遗留 pending 任务处理策略**：
   - 对已知内置但当前关闭的异步任务 `kind`，显式注册一个 `disabledSkipHandler(featureName)`（记录一条信息日志并返回 `job.Skip("feature disabled: " + featureName)`）。
   - **取舍理由**：
     - 避免未注册 `kind` 在 [`internal/job/runner.go`](../../internal/job/runner.go) 落入 `Permanent` 死信触发误告警；
     - 消除 [`internal/webhook/handler.go`](../../internal/webhook/handler.go) 在 `Cipher == nil` 时的 30 秒 `Defer` 空转；
     - 避免未消费的残留任务长期滞留导致 `shortlink_jobs_oldest_pending_age_seconds` 指标误报；
     - 完全不改动 [`internal/postgres/job.go`](../../internal/postgres/job.go) 的 `claimJobsFairSQL` 递归松散索引扫描（[ADR 0022](0022-postgres-query-and-lease-hardening.md)），零索引回退风险。
   - 若未来需要把不同业务拆到**独立专职 Worker 进程池**（如风控 Worker 池与 Webhook Worker 池分进程扩缩容），再另立 ADR 为 `Claim` 增加 `Kinds []string` 过滤及配套复合部分索引。

---

### 6. 扩展点五：数据面与缓存可选化

1. **点击统计（`FEATURE_ANALYTICS=false`）**：
   - `cmd/redirector`：不校验 `VISITOR_SALT` 必填、不启动 `click.New` 后台协程、向 `redirector.New` 传入 `Clicks: nil`，热路径保持零额外抽象和零内存分配；
   - `internal/postgres`：`List`、`GetForTenant` 与 `Update` 在 `analytics=false` 时跳过对 `link_clicks_daily` 的 `LEFT JOIN` / 子查询；
   - `internal/console`：
     - 不注册 `StatsModule`（`/api/v1/links/{code}/stats` 不挂载）；
     - 响应 [`linkResponse`](../../internal/console/linkresponse.go) 将 `Clicks` 与 `ClicksToday` 改为 `*int64` + `json:"...,omitempty"`：开启统计时返回真实数值（含 `0`），关闭统计时直接省略字段，不用假 `0` 混淆“未启用”与“零点击”；
     - `GET /api/v1/links` 在关闭统计时对 `sort=clicks` 与 `sort=clicks_today` 显式返回 `400 Bad Request`（`analytics is disabled`），不静默降级为 `sort=id`，避免破坏游标格式契约与误导调用方。
2. **Redis 读缓存（`REDIS_URL=""`）**：
   - [`internal/config/config.go`](../../internal/config/config.go) 将 `REDIS_URL` 改为可选（为空关闭缓存，非空则照常校验与探活）；
   - `redirector` / `console serve` / `console worker` / CLI `link` 在 `REDIS_URL == ""` 时跳过 `cache` 装饰器、直连 `storemetrics -> pgStore`；`risk.VerdictCache` 支持 `rdb == nil` 时恒 miss 降级；
   - **部署一致红线**：所有进程必须对 `REDIS_URL` 保持一致（全开同一 Redis 或全关），并在 runbook 中明确说明：禁止仅 `redirector` 开缓存而 `console` 不开（会导致修改/下架在 `positiveTTL` 最长 1 小时内不生效）。

---

### 7. 分阶段演进顺序

按风险从低到高、每步独立可交付验收拆为 4 个实施阶段：

1. **阶段 A（存储事务解耦）**：基于 `1ea9258` 的单职责事务步骤引入 `TxEvent`/`TxHook` + 5 个内建 Hook + `cmd/console` 三入口统一装配。
2. **阶段 B（认证插件与控制面模块化）**：两层 `AuthMethod`/`OAuthAuthMethod` 抽象（统一 `/api/v1/auth/{id}/login`·`/callback`）+ `console.Module` 瘦身 + `GET /api/v1/capabilities` 替换 `/api/v1/auth/providers` + 前端 `web/` 同步改造。
3. **阶段 C（Worker 模块化与关闭治理）**：`WorkerModule` 装配 + 分区接口拆分 + `disabledSkipHandler` 存量任务保护。
4. **阶段 D（数据面轻量化）**：`FEATURE_ANALYTICS` 全链路开关（含响应字段 `omitempty` 与 `sort=clicks*` 拒绝）+ `REDIS_URL` 可选化 + 极简无 Redis 冒烟验证。

---

## 不做什么

1. **不在本 ADR 中实现手机号登录本身**：本 ADR 只落地兼容非跳转型登录的 `AuthMethod` + `SessionIssuer` 扩展边界；具体短信验证码发送、防刷限流、`users.phone` 列设计、账号绑定与邀请兑现策略留待后续手机号登录专项 ADR 决策。
2. **不使用 Go 标准库 `plugin`（`.so`）、HashiCorp `go-plugin`（gRPC 子进程）或 WASM 运行期动态插件**：本项目所有扩展均为自研内置模块，不是第三方开发者上传未知代码的开放平台，动态加载会破坏 `-race`、跨平台构建和排障清晰度。
3. **不做插件级独立数据库迁移（Migration）**：`migrations/` 保持单一线性版本序列。关闭某模块时表照常存在（空表几乎零开销），应用层不读写即可，避免迁移版本组合爆炸。
4. **不使用 `init()` 全局注册表**：拒绝隐式魔法注册，所有启用的模块、认证方式与 Hook 必须在 `cmd/` 入口显式可见。
5. **不保留 `/api/v1/auth/providers` 及 `/api/v1/auth/login` 旧路径兼容壳**：同仓库前端、Dex 配置、冒烟脚本与测试一步到位迁至 `GET /api/v1/capabilities` 与 `/api/v1/auth/{id}/...`。
6. **暂不改 `jobs.Claim` 支持按 `kind` 过滤**：当前全部后台任务由统一 `console worker` 消费，先用 `disabledSkipHandler` 保持 `claimJobsFairSQL` 索引计划稳定。

---

## 怎么验证

每阶段完成后跑 `make selfcheck`（涉及前端加跑 `make web-check`），并满足以下专项验收标准：

1. **阶段 A（`TxHook` 与负向副作用断言）**：
   - `internal/postgres` 集成测试：分别关闭 `RiskScanHook`、`WebhookFanoutHook`、`InvitationEmailHook`、`CacheReconcileHook`、`StatsPurgeHook` 时，执行对应核心业务写成功，且断言 `jobs` 表中对应 `kind` 行数为 `0`；任一 Hook 返回错误时主表与审计日志整事务回滚；
   - CLI（`link ban/unban`、`member invite`）与 `worker`（`BanIfURL`）均通过共享装配触发预期 Hook。
2. **阶段 B（认证插件、控制面与前端）**：
   - `httptest` 验证：
     - OIDC 与 GitHub 均通过 `OAuthAuthMethod` 挂在 `/api/v1/auth/{id}/login`·`/callback` 完成完整登录与回调；
     - 用一个轻量级测试替身 `AuthMethod`（模拟非跳转型的 `POST` 表单登录插件）直接通过 `AuthToolkit.Issuer.IssueSession` 签发会话并访问 `/api/v1/me`，证明非 OAuth 登录形态（如未来手机号 OTP）无需修改 `console.Server` 或 `authHandler`；
     - 重复注册同名 `AuthMethod` ID 启动报错/拦截；未注册模块路由返回 `404`；旧路由 `/api/v1/auth/providers` 与 `/api/v1/auth/login` 不再存在；`GET /api/v1/capabilities` 与注册状态严格一致；
   - 前端 Vitest 覆盖 `auth_methods` 动态渲染登录入口、`features.webhooks=false` 时侧边栏隐藏 Webhooks 项、`features.analytics=false` 时隐藏统计入口与点击排序。
3. **阶段 C（Worker 与存量任务）**：
   - 单测/集成测试验证预置历史 `webhook.deliver` / `link.risk_scan` pending 任务后关闭对应开关启动 Worker，任务被 `disabledSkipHandler` 标记 `done`（指标记为 `skipped`），既不进 `dead` 也不循环 `Defer`。
4. **阶段 D（极简无 Redis / 无统计模式）**：
   - `FEATURE_ANALYTICS=false` 时 `redirector` 无需 `VISITOR_SALT` 可正常启动跳转且零点击写库，`console` 列表/详情/更新不查 `link_clicks_daily`、JSON 响应省略 `clicks` 与 `clicks_today`，且 `sort=clicks` / `sort=clicks_today` 返回 `400`；
   - `REDIS_URL=""` 时 `redirector` 与 `console serve` 仅凭 PostgreSQL 正常启动并通过创建 + 跳转 + 更新 + 下架端到端验证；
   - 保持 `make arch-check` 全绿（`cmd/redirector` 继续零控制面依赖，领域包继续零 SQL 依赖）。

---

## 后果

### 好处

- **核心写路径彻底减负**：`postgres.Store` 不再因每加一个外围能力就改动核心方法，单职责清晰易测；
- **认证扩展面向多协议**：无论是未来的 Google/飞书 OAuth，还是手机号短信验证码/Passkey，都统一插在 `AuthMethod` + `SessionIssuer` 上，复用整套会话、Cookie、CSRF、组织与 RBAC 底座；
- **消除“半开关”与“伪兼容”包袱**：关闭 Webhook、邮件、风控、统计或缓存时，生产端、消费端、清理协程、API 语义与前端界面全链路一致；
- **渐进式部署能力**：同一份二进制既能跑“单 PG 极简短链服务”，也能跑“完整企业多租户 SaaS”。

### 代价与后续注意

- **跨进程配置一致性责任**：`redirector`、`console serve`、`console worker` 与 CLI 必须共享一致的 `REDIS_URL` 与 `FEATURE_*` 环境变量（例如使用同一份 `.env` 或 ConfigMap）；
- **OIDC 回调路径变更**：阶段 B 统一 OIDC 回调为 `/api/v1/auth/oidc/callback` 时，需同步更新 `deploy/dex.yaml`、`.env.example` 与本地 `.env` 中的 `OIDC_REDIRECT_URL`；
- **测试覆盖维度增加**：除默认全开路径外，需要维护“关闭各核心插件”与“极简无 Redis 模式”的专项单测/冒烟用例，避免新功能破坏关停分支。
