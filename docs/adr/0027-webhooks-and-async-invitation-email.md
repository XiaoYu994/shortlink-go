# 0027. Webhook 订阅投递与异步成员邀请邮件

- 状态：Accepted
- 日期：2026-11-25
- 相关：`0007`, `0010`, `0018`, `0020`, `0021`, `0022`, `0023`, `0024`, `0025`, `0026`

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 订阅事件范围 | 支持 5 类生命周期事件（`link.created` / `link.updated` / `link.deleted` / `link.banned` / `link.unbanned`）与测试事件 `webhook.ping`；明确排除高频 `link.clicked` |
| Outbox 扇出与幂等 | 复用迁移 `009` 的 `jobs` 表，在业务写事务/多 CTE 内原子扇出 `webhook.deliver`；仅在真实状态跃迁时入队，`Ban`/`Unban` 幂等空操作与冲突回滚均不产生重复事件 |
| 静态加密与签名 | Webhook 签名密钥使用 `WEBHOOK_SECRET_KEY`（32 字节 hex）做 AES-256-GCM 静态加密并绑定 `tenant_id` AAD；出站附带 `X-Shortlink-Signature: t=<ts>,v1=<hex>` 与稳定 `X-Shortlink-Delivery: dlv_<job_id>` |
| 出站 SSRF 底座抽离 | 从 `internal/risk` 抽离安全 `DialContext`、IP/端口拦截与 `HostLimiter` 到零业务依赖包 `internal/safehttp`；Webhook 强制禁止跟随重定向，沿用 `devtest` 构建隔离 `--allow-test-host` |
| 投递重试与熔断 | 投递超时默认 `5s`、最多 6 次独立指数退避重试（`10s..10m`）；`3xx`/`4xx`（`408`/`429` 除外）/SSRF 拦截立即终态失败；连续终态失败达 10 次自动停用订阅并写系统审计日志 |
| 管理接口与权限 | 新增 `webhooks:read` / `webhooks:write`，仅授予 `owner` / `admin` 会话（API Key 不可访问）；测试按钮通过入队 `webhook.ping` 异步走 worker，避免 `console serve` 同步发外网 |
| 异步成员邀请邮件 | `CreateInvitation` 同事务入队 `invitation.email`（按 `invitation:<id>` 去重）；新建零外部依赖的 `internal/mailer`（标准库 SMTP + `ctx` 超时控制），通过 `CONSOLE_PUBLIC_URL` 构造登录链接 |

---

## 背景

阶段 12 与阶段 14 已经建成基于 PostgreSQL `jobs` 表的通用可靠后台任务引擎（[ADR 0020](0020-background-jobs-and-async-risk-scan.md)）和全链路审计日志（[ADR 0023](0023-audit-logs.md)），阶段 13 上线了组织自助闭环与 Web 控制台（[ADR 0021](0021-tenant-self-service-and-web-console.md)）。按照 [路线图](../roadmap.md) 阶段 15 的规划，还存在两块缺口需要在不动数据面（`cmd/redirector`）的前提下补齐：

1. **租户缺少外部系统事件通知能力**：组织接入短链服务后，无法在短链创建、修改、删除或被风控自动封禁/解封时实时回调下游系统（CMS、工单、风控大盘）；而且平台里所有链接状态变更分散在 HTTP API、`console` CLI 与风控 worker 三个入口，必须在统一存储边界保证“状态改了就一定会发出事件，事务失败则绝不发出”。
2. **成员邀请停留在线下人工通知**：[ADR 0021](0021-tenant-self-service-and-web-console.md) 落地了 `POST /api/v1/invitations` 与 `console member invite`，但创建邀请后只是把记录落库，管理员仍需手工复制登录地址发给同事；若直接在 `console serve` 里同步连接 SMTP 服务器，邮件服务器抖动会直接拖慢甚至打爆管理接口请求超时（`REQUEST_TIMEOUT=5s`）。
3. **出站 HTTP 安全能力需要跨功能复用**：`internal/risk` 已经打磨出一套防 DNS Rebinding、封禁私有/回环/CGNAT/NAT64/6to4 IP、限制端口白名单与按目标 host 限速的安全抓取组件；若在 Webhook 投递里复制粘贴一遍，必然产生双份安全规则漂移。同时，相比风控允许有限跳转抓取目标网页，Webhook 投递面对的“3xx 重定向泄露签名头与切换 POST 方法”风险更严格，需要抽象可控的共享传输底座。

---

## 决策

### 1. 事件范围与载荷契约

#### 1.1 事件枚举与数据面隔离

- **可订阅业务事件（5 类）**：
  - `link.created`：新建短链提交成功后触发；
  - `link.updated`：短链改 URL、改过期时间或启用/停用实际更新行后触发；
  - `link.deleted`：非封禁短链删除后触发；
  - `link.banned`：仅当链接从**未封禁变为已封禁**（`newly_banned = true`）时触发（覆盖 `console link ban` CLI 与风控 worker `BanIfURL`）；
  - `link.unbanned`：仅当链接从**已封禁变为未封禁**（`was_banned = true`）时触发（覆盖 `console link unban` CLI）。
- **人工联调事件（1 类）**：
  - `webhook.ping`：仅由 `POST /api/v1/webhooks/{id}/test` 触发；**禁止作为持久化订阅事件写入 `webhooks.events`**。
- **不发射 `link.clicked`**：跳转面处于极高 QPS 且只允许异步容错写统计（[ADR 0007](0007-async-click-recording.md)）与禁止依赖任务队列包（[ADR 0024](0024-process-boundary-and-ops-isolation.md)），不把高频点击事件灌入控制面 Outbox。

#### 1.2 统一信封、事件标识与幂等边界

所有出站请求发送同一结构的 UTF-8 JSON 信封，保证下游按单一入口反序列化：

```json
{
  "id": "evt_9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
  "event": "link.banned",
  "api_version": "v1",
  "created_at": "2026-11-25T10:30:00Z",
  "tenant_id": 7,
  "data": {
    "link_id": 42,
    "code": "Ab3xY9zQ",
    "url": "https://example.com/bad",
    "ban_reason": "phishing",
    "ban_source": "risk"
  }
}
```

- **同事件多订阅共享 `event_id`**：一次业务写操作扇出到多条匹配订阅时，在 SQL 语句内用 PostgreSQL 16/17 原生 `gen_random_uuid()` 生成同一个 `event_id`（前缀 `evt_`），沿用 [ADR 0026](0026-timeseries-partitioning-and-retention.md) 零外部扩展原则；每条投递任务各自拥有独立的 `dlv_<job_id>` 作为 `X-Shortlink-Delivery`（跨重试保持不变，供下游做幂等去重）。
- **状态跃迁才扇出（幂等与冲突不漏发、不重发）**：
  - `Create` 短码冲突触发 SQL 回滚重试时，CTE 里的 `jobs` 插入随语句一起回滚；
  - `Ban` / `BanIfURL` 重复封禁已封禁链接（`newly_banned = false`）或因竞态跳过（`url_matched = false`）不入队 `link.banned`；
  - `Unban` 对未封禁链接的空操作（`was_banned = false`）不入队 `link.unbanned`；
  - `Update` 遇到已封禁链接（`is_banned = true`）不修改主行，也不入队 `link.updated`。
  - 这样 `Ban`/`Unban` 哪怕通过 CLI 或风控任务发起，也与 API 保持一致、不会遗漏通知。

### 2. 数据模型与索引（迁移 `015_webhooks`）

沿用 [ADR 0010](0010-app-level-referential-integrity.md) 不建数据库外键，并在高变更计数字段上遵循 PostgreSQL HOT Update 纪律（不给 `consecutive_failures` 建索引）：

#### 2.1 `webhooks` 订阅配置表

```sql
CREATE TABLE webhooks (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id            BIGINT      NOT NULL,
    url                  TEXT        NOT NULL,
    description          TEXT        NOT NULL DEFAULT '',
    events               TEXT[]      NOT NULL,
    secret_ciphertext    BYTEA       NOT NULL,
    secret_prefix        TEXT        NOT NULL,
    enabled              BOOLEAN     NOT NULL DEFAULT TRUE,
    consecutive_failures INT         NOT NULL DEFAULT 0,
    disabled_reason      TEXT        NOT NULL DEFAULT '',
    disabled_at          TIMESTAMPTZ,
    created_by           BIGINT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_webhooks_tenant_positive CHECK (tenant_id > 0),
    CONSTRAINT ck_webhooks_url_length CHECK (octet_length(url) BETWEEN 1 AND 2048),
    CONSTRAINT ck_webhooks_description_length CHECK (octet_length(description) <= 200),
    CONSTRAINT ck_webhooks_events_nonempty CHECK (cardinality(events) BETWEEN 1 AND 16),
    CONSTRAINT ck_webhooks_events_valid CHECK (
        events <@ ARRAY['link.created', 'link.updated', 'link.deleted', 'link.banned', 'link.unbanned']::text[]
    ),
    CONSTRAINT ck_webhooks_secret_ciphertext_len CHECK (octet_length(secret_ciphertext) >= 29),
    CONSTRAINT ck_webhooks_secret_prefix_len CHECK (octet_length(secret_prefix) BETWEEN 6 AND 24),
    CONSTRAINT ck_webhooks_consecutive_failures_nonneg CHECK (consecutive_failures >= 0),
    CONSTRAINT ck_webhooks_disabled_consistency CHECK (
        (enabled = TRUE AND disabled_reason = '' AND disabled_at IS NULL)
        OR (enabled = FALSE AND disabled_reason IN ('manual', 'consecutive_failures') AND disabled_at IS NOT NULL)
    ),
    CONSTRAINT ck_webhooks_created_by_positive CHECK (created_by IS NULL OR created_by > 0)
);

CREATE INDEX idx_webhooks_tenant_id
    ON webhooks (tenant_id, id DESC);

CREATE INDEX idx_webhooks_tenant_enabled
    ON webhooks (tenant_id)
    WHERE enabled = TRUE;
```

- **单租户订阅上限 `MaxWebhooksPerTenant = 5`**：创建 Webhook 事务先对 `tenants` 对应行加 `FOR UPDATE` 行锁，再统计当前订阅数校验上限，既杜绝并发竞态突破配额，也把单次短链写操作的 Outbox 扇出严格封顶在 5 条以内，防止放大攻击 `jobs` 表。
- **数组匹配无须 GIN 索引**：单租户启用订阅至多 5 行，沿 `idx_webhooks_tenant_enabled (tenant_id) WHERE enabled = TRUE` 读取至多 5 行再用 `$event = ANY(w.events)` 过滤，在 `shared_buffers` 内微秒级完成，避免 GIN 索引额外写开销；且 `consecutive_failures` 无任何二级索引，只更新状态列时可触发 PostgreSQL HOT Update。

#### 2.2 `webhook_deliveries` 投递历史明细表

```sql
CREATE TABLE webhook_deliveries (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        BIGINT      NOT NULL,
    webhook_id       BIGINT      NOT NULL,
    job_id           BIGINT      NOT NULL,
    delivery_id      TEXT        NOT NULL,
    event            TEXT        NOT NULL,
    attempt          INT         NOT NULL,
    status           TEXT        NOT NULL,
    http_status      INT,
    duration_ms      INT         NOT NULL,
    error_message    TEXT        NOT NULL DEFAULT '',
    response_snippet TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_webhook_deliveries_tenant_positive CHECK (tenant_id > 0),
    CONSTRAINT ck_webhook_deliveries_webhook_positive CHECK (webhook_id > 0),
    CONSTRAINT ck_webhook_deliveries_job_positive CHECK (job_id > 0),
    CONSTRAINT ck_webhook_deliveries_delivery_id_len CHECK (octet_length(delivery_id) BETWEEN 1 AND 64),
    CONSTRAINT ck_webhook_deliveries_event_valid CHECK (
        event IN ('link.created', 'link.updated', 'link.deleted', 'link.banned', 'link.unbanned', 'webhook.ping')
    ),
    CONSTRAINT ck_webhook_deliveries_attempt_positive CHECK (attempt >= 1),
    CONSTRAINT ck_webhook_deliveries_status CHECK (status IN ('success', 'retry', 'failed')),
    CONSTRAINT ck_webhook_deliveries_http_status CHECK (http_status IS NULL OR (http_status BETWEEN 100 AND 599)),
    CONSTRAINT ck_webhook_deliveries_duration_nonneg CHECK (duration_ms >= 0),
    CONSTRAINT ck_webhook_deliveries_error_len CHECK (octet_length(error_message) <= 512),
    CONSTRAINT ck_webhook_deliveries_response_len CHECK (octet_length(response_snippet) <= 1024)
);

CREATE INDEX idx_webhook_deliveries_tenant_webhook_id
    ON webhook_deliveries (tenant_id, webhook_id, id DESC);

CREATE INDEX idx_webhook_deliveries_created_at_id
    ON webhook_deliveries (created_at, id);
```

- **`webhook_deliveries` 单独落表而不仅靠 `jobs.last_error`**：控制台需要按订阅回看“每一次尝试的 HTTP 状态码、耗时毫秒、响应片段与错误原因”，而 `jobs` 会按 7 天清理且单条重试会覆盖 `last_error`。将每次尝试记录成不可变追加行，查询直接命中 `(tenant_id, webhook_id, id DESC)`，旧记录由 worker 定期任务 `webhooks.cleanup_deliveries` 按 `created_at` 与保留期（`WORKER_WEBHOOK_DELIVERY_RETENTION`，默认 `168h` = 7 天）沿 `(created_at, id)` 执行 `LIMIT 500` 分批删除。

### 3. 密钥静态加密与 HMAC-SHA256 签名

#### 3.1 订阅密钥与 AES-256-GCM 静态加密

- **订阅签名密钥**：创建和轮换时通过 `crypto/rand` 生成 `whsec_` + 64 个十六进制字符（256 位随机熵）；`secret_prefix` 仅保存前 12 个字符（如 `whsec_a1b2c3`）供控制台展示识别。**明文密钥仅在创建与轮换响应中返回一次**，绝不写入 `audit_logs`、系统日志或配置转储。
- **静态加密方案**：`webhook` 需要向第三方发送对称 HMAC 签名，因此存储层不能像 `api_keys` 那样单向哈希，必须可逆解密。采用 AES-256-GCM：
  - 主密钥来自环境变量 `WEBHOOK_SECRET_KEY`（要求 64 字符 hex，解码后 32 字节），在 `ConsoleConfig` / `WorkerConfig` 的 `LogValue()` 与 `String()` 中用 `logx.RedactSecret` 掩码；
  - 密文二进制格式：`[1 字节版本 0x01][12 字节随机 Nonce][AES-GCM 密文 + 16 字节认证 Tag]`（固定认证开销 29 字节：1+12+16；70 字节 `whsec_` 明文加密后为 99 字节，与库约束 `>= 29` 一致）；
  - 附加认证数据（AAD）固定绑定为 `shortlink:webhook-secret:v1:tenant:<tenant_id>`，防止跨租户密文拷贝替换；密文首字节预留版本号以便后续支持主密钥轮换。
- **未配置 `WEBHOOK_SECRET_KEY` 的运行策略**：
  - 延续 `OIDC_*` / `GITHUB_*` 的可选功能模式：未配置时 `console serve` 正常启动但不挂载 `/api/v1/webhooks*` 路由，并在 `GET /api/v1/auth/providers` 返回 `"webhooks": false`（告知前端隐藏/提示未启用）；
  - `console worker` 启动打印 `webhooks disabled: WEBHOOK_SECRET_KEY is not set`；若此前已有 `webhook.deliver` 任务留在队列，handler 返回 `job.Defer(30*time.Second, "webhook secret key not configured")` **不消耗重试次数也不进入死信**，补配密钥重启即可无缝恢复；
  - 若 `WEBHOOK_SECRET_KEY` 已设置但不是合法 64 字符 hex，两进程均启动即失败（fail-fast）。

#### 3.2 HMAC-SHA256 签名与请求头规范

每次出站投递请求头如下：

- `Content-Type: application/json; charset=utf-8`
- `User-Agent: shortlink-webhook/1.0`
- `X-Shortlink-Event: <event>`
- `X-Shortlink-Delivery: dlv_<job_id>`
- `X-Shortlink-Signature: t=<unix_sec>,v1=<hex(HMAC-SHA256(secret, "<unix_sec>.<raw_body>"))>`

设计约束：
- **签名载荷包含时间戳和原始字节体**：`"<unix_sec>.<raw_body>"` 能防止中间人抓包后跨窗口重放或篡改载荷空白；每次退避重试都用当前时刻重新签发 `t`，避免几分钟后的重试被下游“5 分钟防重放窗口”误拒，同时保持 `X-Shortlink-Delivery` 不变供接收端幂等去重。
- **`internal/webhook` 提供标准验签工具函数 `VerifySignature`**：供内部测试与下游 Go 示例复用，使用 `hmac.Equal` 常量时间比对，默认重放容忍窗口为 5 分钟。

### 4. 抽离共享出站安全底座 `internal/safehttp`

- 将 [`internal/risk/ssrf.go`](../../internal/risk/ssrf.go) 与 [`internal/risk/hostlimit.go`](../../internal/risk/hostlimit.go) 中的纯出站安全原语抽离到零业务依赖包 `internal/safehttp`：
  - `IsBlockedAddr`（含 `Unmap()` 还原 IPv4-mapped IPv6，封禁回环/私网/链路本地/CGNAT/Teredo/6to4/NAT64 等保留网段）；
  - `HostResolver` 接口与防 DNS Rebinding 安全 `DialContext`（强制 `Proxy: nil` 绕开环境变量代理、全解析 IP 核验通过后再直连已校验 IP、出站端口白名单默认 `80,443`）；
  - `ValidateURL`（校验仅 `http`/`https`、无 userinfo 与 `#fragment`、非 `localhost` 且非受禁字面量 IP、端口在白名单内，供 Webhook 订阅创建/修改时在入口即拦下明显的非法 URL）；
  - `HostLimiter`（进程内惰性清理按目标 host 令牌桶限速）。
- **Redirect 策略分叉**：
  - `internal/risk` 继续允许最多 5 跳仅 `http`/`https` 的安全重定向（覆盖短链目标页常见的营销跳转落地页）；
  - `safehttp.ClientOptions` 新增 `DisableRedirects bool`：Webhook 客户端强制设为 `true`（`CheckRedirect` 返回 `http.ErrUseLastResponse`），**严禁把带 HMAC 签名和业务载荷的 POST 请求重定向到其他目标**；收到 `3xx` 时直接按失败停止重试。
- **`devtest` 隔离与 `make arch-check` 边界扩展**：
  - 复用现有 `//go:build devtest` 构建标签：[`cmd/console/worker_allowtest_dev.go`](../../cmd/console/worker_allowtest_dev.go) 保留的 `--allow-test-host` 同时传入风控与 Webhook 客户端；同步允许 `devtest` 构建的 `console serve` 传入 `--allow-test-host` 以便冒烟测试把本地回环测试桩 URL 注册为 Webhook 订阅（`console serve` 在入口校验订阅 URL 时对生产环境依然严格拦截内网/回环 URL）。**正式无 tag 编译产物中完全不存在任何放行内网开关**。
  - 更新 `Makefile` `arch-check`：新增限制 `cmd/redirector`、`internal/httpx`、`internal/config` 均不得依赖 `safehttp`/`webhook`/`mailer`；`internal/safehttp` 不得依赖业务与存储包；`internal/mailer` 保持仅依赖标准库。

### 5. `webhook.deliver` 可靠投递、退避与熔断策略

#### 5.1 最小 Payload 与二次读库

- `jobs.payload` 只保存事件事实，**严禁把目标 URL 和加密密钥复制进 `jobs.payload`**：

```json
{
  "webhook_id": 12,
  "event_id": "evt_9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
  "event": "link.created",
  "occurred_at": "2026-11-25T10:30:00Z",
  "data": { ... }
}
```

- 这样做有三个收益：订阅修改 URL、轮换密钥、人工停用或删除后，排队中的任务立即生效（读不到或已手动停用时返回 `job.Skip` 退出）；同时 `jobs` 历史清理前不会长期滞留敏感配置。

#### 5.2 流控、超时、错误分类与重试退避

1. **按 host 限速（`WEBHOOK_HOST_RATE_LIMIT`，默认 `5` req/s）**：独立于风控 `HostLimiter`；触发限速时返回 `job.Defer(delay, ...)`，不消耗尝试次数。
2. **HTTP 超时与响应截断**：单次投递使用 `WEBHOOK_TIMEOUT`（默认 `5s`，配置校验要求严格小于 `WORKER_JOB_TIMEOUT`）；响应体通过 `io.LimitReader(resp.Body, 4096)` 限流读取，剔除 NUL 字节并按 UTF-8 边界安全截断至 1024 字节记入 `webhook_deliveries.response_snippet`。
3. **响应判定与重试矩阵**：
   - **成功**：HTTP `200..299` -> 记录 `webhook_deliveries(status='success')`；若订阅当前 `consecutive_failures > 0`，条件执行 `UPDATE webhooks SET consecutive_failures = 0, updated_at = now() WHERE id = $1 AND consecutive_failures > 0`；返回 `nil` 标记任务 `done`。
   - **不可重试永久失败**：载荷损坏、SSRF 拦截（`safehttp.ErrBlockedAddress`/`ErrBlockedPort`/`ErrInvalidScheme`）、HTTP `3xx` 重定向、除 `408`/`429` 外的 HTTP `4xx` 客户端错误 -> 记录 `webhook_deliveries(status='failed')`，返回 `job.Permanent(err)` 立即进入 `dead`。
   - **可重试暂时性失败**：网络超时/连接错误、TLS 握手失败、HTTP `408`、`429` 或 `5xx` -> 计算 Webhook 专用退避（在 `internal/job` 新增轻量 `job.RetryAfter(delay, err)` 包装错误，不影响已有全局 `Runner` 默认退避参数）；默认入队 `max_attempts = 6`、退避区间 `WEBHOOK_RETRY_BASE_DELAY=10s` 至 `WEBHOOK_RETRY_MAX_DELAY=10m`（带 `[0.8, 1.0]` 抖动）：
     - 若仍有剩余尝试次数（`j.Attempts < j.MaxAttempts`），写 `webhook_deliveries(status='retry')` 并返回 `job.RetryAfter(delay, err)`；
     - 若已达末次尝试（`j.Attempts >= j.MaxAttempts`），写 `webhook_deliveries(status='failed')` 并交由计数熔断逻辑处理后进 `dead`。

#### 5.3 自动熔断机制与测试 Ping

- **连续终态失败自动停用（默认阈值 `WEBHOOK_MAX_CONSECUTIVE_FAILURES = 10`，设为 `0` 关闭）**：
  - 注意不是按单次 retry 计数，而是**一条投递任务最终失败时**（即进入 `dead` 的那一次：永久失败或 6 次重试耗尽）原子执行 `consecutive_failures = consecutive_failures + 1`；
  - 若 `consecutive_failures + 1 >= threshold` 且当前仍为 `enabled = TRUE`，同一事务内把订阅置为 `enabled = FALSE`、`disabled_reason = 'consecutive_failures'`、`disabled_at = now()`，写入 `webhook.auto_disable` 系统审计日志（`audit.SystemMeta("job:" + j.ID)`）并递增指标 `shortlink_webhook_auto_disabled_total`；
  - 管理员排查修好接收端并重新通过 `PATCH` 启用订阅时，条件重置 `consecutive_failures = 0`、`disabled_reason = ''`、`disabled_at = NULL`。
- **`POST /api/v1/webhooks/{id}/test`（异步测试 Ping）**：
  - 不在 `console serve` 里直接发起外网 HTTP 请求，而是同事务写 `webhook.test` 审计日志并入队一条 `max_attempts = 1` 的 `webhook.deliver`（`event = "webhook.ping"`），接口立即返回 `202 Accepted`（含 `job_id` 与 `delivery_id`）；
  - `webhook.ping` 允许对已停用（`enabled = FALSE`）的订阅发测试，方便管理员修好接收端后先试通再启；且 `webhook.ping` 的成败不修改 `consecutive_failures`、不触发自动熔断，但完整落库 `webhook_deliveries` 供控制台展示结果。

#### 5.4 Prometheus 指标与告警调整

- 新增指标：
  - `shortlink_webhook_deliveries_total{event,result}`（`result`: `success` / `retry` / `failed` / `skipped` / `deferred`）
  - `shortlink_webhook_delivery_duration_seconds{event}`
  - `shortlink_webhook_auto_disabled_total`
- 更新 [`deploy/alerts.yml`](../../deploy/alerts.yml)：已有 `ShortlinkJobsDead` 告警调整为排除第三方端点错误噪声较大的 `webhook.deliver`（`kind!="webhook.deliver"`），单独增设 Webhook 连续投递失败告警与自动熔断通知规则，避免单个租户配置错误 Webhook URL 引发平台值班误报。

### 6. RBAC、管理 HTTP API、审计日志与控制台

#### 6.1 权限矩阵与纵深防御

- 在 `internal/identity` 新增权限：`PermWebhooksRead = "webhooks:read"`、`PermWebhooksWrite = "webhooks:write"`。
- `MapAuthorizer` 仅将两项权限授予 **`RoleOwner` 与 `RoleAdmin`**；沿用 `apikeys:*` 与 `audit:read` 边界，**不加入 `AllAPIKeyScopes()`**；handler 内二次校验 `p.ActorKind == identity.ActorUser` 并统一返回 `Cache-Control: no-store`。

#### 6.2 HTTP 接口设计（`/api/v1/webhooks`）

| 方法与路径 | 权限 | 语义与返回 |
| --- | --- | --- |
| `GET /api/v1/webhooks` | `webhooks:read` | 列出当前组织订阅列表（不含密文，含 `secret_prefix`、`enabled`、`consecutive_failures`、`disabled_reason`） |
| `POST /api/v1/webhooks` | `webhooks:write` | 创建订阅（校验 URL 与事件去重、锁租户检查上限 5 条、AES-GCM 加密入库、写审计），返回 `201 Created` 与**一次性明文 `secret`** |
| `PATCH /api/v1/webhooks/{id}` | `webhooks:write` | 修改 `url`、`description`、`events`、`enabled`（重新启用时清零失败计数与停用原因，手动停用设 `disabled_reason = 'manual'`），写审计 |
| `DELETE /api/v1/webhooks/{id}` | `webhooks:write` | 删除订阅（写 `webhook.delete` 审计），返回 `204 No Content`（投递中排队任务回表后自动 `Skip`） |
| `POST /api/v1/webhooks/{id}/rotate-secret` | `webhooks:write` | 生成新 `whsec_` 密钥加密覆盖并更新 `secret_prefix`，写审计，返回 `200 OK` 与**一次性新明文 `secret`** |
| `POST /api/v1/webhooks/{id}/test` | `webhooks:write` | 入队 `max_attempts = 1` 的 `webhook.ping` 任务并写审计，返回 `202 Accepted`（含 `job_id` / `delivery_id`） |
| `GET /api/v1/webhooks/{id}/deliveries` | `webhooks:read` | 按 `id DESC` Keyset 游标分页（`cursor` / `limit`，默认 20 最大 100）查询单条订阅近期投递日志 |

#### 6.3 审计日志与 Web 控制台

- `internal/audit` 新增 `TargetWebhook = "webhook"` 与 6 个审计动作：
  - `webhook.create`、`webhook.update`、`webhook.delete`、`webhook.rotate_secret`、`webhook.test`、`webhook.auto_disable`（全部同事务 fail-closed，严禁把明文密钥或完整密钥前 12 位以外信息写入审计 metadata）。
- 前端控制台新增 `web/src/features/webhooks/` 页面与路由，仅当 `owner`/`admin` 且 `GET /api/v1/auth/providers` 返回 `webhooks: true` 时提供完整管理（含一次复制签名密钥弹窗、订阅开关/熔断提示、测试按钮、最近投递记录抽屉/列表），未启用时给出明确配置提示；同步在 `AuditLogsPage` 补齐 6 个新动作文案。

### 7. 异步成员邀请邮件（`invitation.email` 与 `internal/mailer`）

#### 7.1 Outbox 原子入队与状态二次核验

- `postgres.Store.CreateInvitation` 在事务内登记邀请行、写入 `invitation.create` 审计日志的同时，原子插入一条 `job.KindInvitationEmail = "invitation.email"` 任务：
  - `tenant_id = inv.TenantID`
  - `payload = {"invitation_id": inv.ID}`（**不在 `jobs.payload` 冗余受邀人邮箱与组织名，PII 随主表生命周期管理**）
  - `dedup_key = "invitation:" + strconv.FormatInt(inv.ID, 10)`
- 因此无论从 Web 控制台（`POST /api/v1/invitations`）还是运维 CLI（`console member invite`）发起邀请，都会统一进入异步发信队列。
- Worker 消费 `invitation.email` 时先联表查询邀请最新状态、所属组织名称以及可选邀请人名称/邮箱：
  - 若邀请已不存在、已兑现（`accepted_at`）、已撤销（`revoked_at`）、已被重邀替代（`superseded_at`）或已过期（`expires_at <= now()`），直接返回 `job.Skip(...)` 不发出无效邮件；
  - 发信成功记录结构化 INFO 日志（含 `invitation_id`、`tenant_id`，邮箱域名或按既有 `logx` 习惯脱敏规则不泄露凭据）。

#### 7.2 零外部依赖邮件包 `internal/mailer` 与 SMTP 契约

- 新建仅依赖 Go 标准库的 `internal/mailer` 包（参考 Java 里 `JavaMailSender` 的接口抽象，但在 Go 中按使用方定义最小接口）：
  - `Sender` 接口仅包含 1 个方法：`Send(ctx context.Context, msg Message) error`；
  - `SMTPConfig` + `SMTPSender` 实现：使用 `net.Dialer.DialContext` 建立连接并用 `conn.SetDeadline(deadline)` 同步驱动 `ctx` 超时（弥补标准库 `net/smtp.SendMail` 不接受 `context.Context` 无法响应超时与停机取消的缺陷）；
  - 支持 `SMTP_TLS_MODE`：`starttls`（默认，明文连入后升级 TLS）、`smtps`（隐式 TLS 直连）与 `none`（仅推荐内网中继或本地测试桩）；使用 `smtp.PlainAuth`（未设用户名则跳过认证）；
  - 发送前严格校验发件人/收件人/主题不含 `\r` / `\n` 与控制字符，防范 SMTP 头部注入；
  - 通过类型化 `smtpStatusError` 识别 SMTP 状态码：**`5xx` 永久拒绝（如无效域、邮箱不存在配置错误）包装为 `job.Permanent`**，**`4xx` 限流/临时拒绝与网络/TLS 超时按可重试退避重投**。

#### 7.3 环境变量约定

- `SMTP_HOST`：可选；为空时 `invitation.email` handler 返回 `job.Skip("smtp not configured")`，不阻断无邮件环境的邀请落库与手工复制链接流程；配置了 `SMTP_HOST` 时强制要求配置合法 `SMTP_FROM`（否则启动 fail-fast）。
- `SMTP_PORT`：默认 `587`。
- `SMTP_USERNAME` / `SMTP_PASSWORD`：可选认证凭据，`WorkerConfig.LogValue()` 与 `String()` 中用 `logx.RedactSecret` 掩码密码。
- `SMTP_TLS_MODE`：默认 `starttls`（允许 `starttls` / `smtps` / `none`）。
- `SMTP_TIMEOUT`：默认 `10s`（校验必须 `> 0` 且 `< WORKER_JOB_TIMEOUT`）。
- `CONSOLE_PUBLIC_URL`：可选，用于邮件正文拼装控制台登录地址 `<CONSOLE_PUBLIC_URL>/login`（校验只允许 `http`/`https`，仅保留干净的基址路径）；**有意与 `PUBLIC_BASE_URL` 分离**，因为后者专门指向 `cmd/redirector` 短链域名/端口，多入口部署时两者常常不同。未配置 `CONSOLE_PUBLIC_URL` 时，邮件正文保留清晰组织名、角色、登录账号说明与提示向管理员索取控制台入口的降级文案，而非输出错误域名。

---

## 不做什么

- **不投递高频 `link.clicked` 事件**：严守数据面与控制面边界，点击流水继续走异步内存批汇总（[ADR 0007](0007-async-click-recording.md)）。
- **第一期不做双密钥重叠签名窗口（`v1=new,v1=old`）或主密钥在线双密钥解密**：订阅密钥轮换立即切换新密文，密文首字节保留版本号 `0x01` 以便未来平滑升级主密钥，当前不过早引入多密钥配置复杂度。
- **不对 `webhook_deliveries` 做按月范围分区**：Webhook 投递明细属于控制面较低频历史日志，按 `created_at` 加 7 天 `LIMIT 500` 分批清理即可控制体积，留给高基数点击/审计表的分区方案（[ADR 0026](0026-timeseries-partitioning-and-retention.md)）不乱套用。
- **不引入第三方邮件商 SDK 或复杂 HTML 模板引擎**：`internal/mailer` 保持标准库零第三方依赖，发送简洁防注入的纯文本 UTF-8 邮件即可满足团队邀请通知。
- **不在 `console serve` 里同步发 Webhook 或发 SMTP 邮件**：所有外发网络 I/O（含 Webhook 测试 Ping 与成员邀请通知）一律经 Outbox 交给 `console worker` 异步执行与限流。
- **不提供自定义出站 HTTP Header、mTLS 客户端证书或批量重放死信 UI**：保持配置面紧凑安全，待真实业务出现强需求再演进。

---

## 怎么验证

动手实施时按以下入口与观测条件逐项核验：

1. **迁移可逆性（迁移 `015`）**：
   - `./scripts/prepare-test-db.sh` 验证 `up -> down -> up` 全程无错；`cmd/migrate/main_test.go` 同步更新版本数为 15。
2. **单元测试（`make test-unit` / `make verify`）**：
   - `internal/safehttp`：覆盖私有/回环/NAT64/6to4/Teredo IP 阻断、`Unmap()` 防绕过、`DisableRedirects: true` 阻断 `301/302/307/308`、端口白名单与 `HostLimiter`；
   - `internal/webhook`：覆盖 AES-256-GCM 加解密与跨 `tenant_id` AAD 篡改失败、HMAC-SHA256 签名与 5 分钟重放窗口校验、以及 `2xx`/`3xx`/`4xx`/`408`/`429`/`5xx`/超时/停用跳过/`10` 次连续终态失败自动熔断状态机；
   - `internal/mailer`：本地回环 TCP 桩覆盖 `STARTTLS`/`none` 协议交互、`\r`/`\n` 头部注入拒绝、`ctx` 超时打断、SMTP `5xx`（`job.Permanent`）与 `4xx`（可重试）区分；
   - `internal/console`（`httptest`）：验证 RBAC（`owner`/`admin` 允许，`member`/`viewer`/API Key 一律 403）、`WEBHOOK_SECRET_KEY` 缺失时不挂路由且 `/auth/providers` 暴露 `webhooks: false`、密钥明文仅返回一次、5 条配额上限 409、测试 Ping 返回 202；
   - `cmd/console`：覆盖 `ConsoleConfig`/`WorkerConfig` 密钥掩码、`--allow-test-host` 编译期隔离（含 `!devtest` 构建验证）。
3. **集成测试与真实 `EXPLAIN (ANALYZE, BUFFERS)`（`make test-integration`）**：
   - 验证 `Create`（含短码冲突重试回滚）、`Update`（含已封禁拒改与不改 URL/仅启用停用场景）、`Delete`、`Ban`、`BanIfURL`、`Unban` 与 `webhooks` 匹配订阅列表的原子扇出，确认幂等重复 `Ban`/`Unban` 不发重复任务；
   - 验证并发创建第 6 条订阅时，锁租户行校验严格返回配额超限且最多落库 5 条；
   - 在千行级合成订阅与万行级投递历史表上实测 `EXPLAIN (ANALYZE, BUFFERS)`，确认扇出查询命中 `idx_webhooks_tenant_enabled`、投递日志翻页命中 `idx_webhook_deliveries_tenant_webhook_id`、过期日志批删命中 `idx_webhook_deliveries_created_at_id`，无退化 `Seq Scan`。
4. **真实多进程冒烟与前端检查（`make selfcheck` 与 `make web-check`）**：
   - `scripts/smoke.sh` 启动本地回环 Webhook HTTP 接收桩（第一请求返回 `500`、重试后返回 `200`，逐个核验 `X-Shortlink-Event`、`X-Shortlink-Delivery` 不变、`X-Shortlink-Signature` 验签通过与载荷字段），覆盖 `link.created` / `link.updated` / `link.banned`（风控自动封禁）/ `link.unbanned` / `link.deleted` 与 `POST /webhooks/{id}/test`；
   - `scripts/smoke.sh` 启动最小本地回环 SMTP 测试桩，验证 `POST /api/v1/invitations` 后由 worker 异步发出包含正确收件人、组织名与登录链接的邮件；
   - `make arch-check`、`make verify`、`make web-check` 全部通过且无告警。

---

## 后果

- **收益**：
  - 补全短链生命周期与异步风控对下游系统的可验证事件通知闭环，且通过事务内 Outbox 与状态跃迁条件确保全入口一致、不重不漏；
  - 成员邀请正式形成异步闭环，与管理 API 延迟完全解耦；
  - 抽离 `internal/safehttp` 消除未来多处出站 HTTP 重复实现安全基线的问题，同时维持 `cmd/redirector` 二进制零出站/零身份/零队列污染的严格架构边界。
- **代价与运维注意**：
  - 引入两类新增环境凭据：`WEBHOOK_SECRET_KEY`（需通过 KMS/密钥注入妥善备份；**若丢失且更换新密钥，既有订阅密文将无法解密，需要管理员重新调用轮换密钥接口恢复**）与 `SMTP_*`；
  - 写短链时若租户配有启用的 Webhook，每匹配一条会额外向 `jobs` 多写 1 行，依赖 `MaxWebhooksPerTenant = 5` 与按租户软并发上限防止单租户冲击队列；
  - `console serve`、`console worker` 部署多副本需保持 NTP 时钟基本同步，避免签名时间戳 `t` 与接收端 5 分钟窗偏移过大。
- **后续演进方向**：
  - 若未来需要跨组织零停机轮换平台主密钥，可利用密文前缀版本字节升级为 `WEBHOOK_SECRET_KEYS` 密钥环（新版加密、按首字节版本查找旧钥解密）；
  - 未来若单个下游端点需要更高吞吐，可在不改动 `jobs` 表结构的前提下为 `webhook.deliver` 引入独立队列消费池或优先级通道。
