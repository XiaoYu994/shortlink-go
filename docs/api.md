# HTTP API

## 路由

| 进程 | 方法 | 路径 | 说明 |
| --- | --- | --- | --- |
| redirector | `GET` | `/healthz` | 存活检查，不访问依赖，返回 `ok` |
| redirector | `GET` | `/readyz` | 就绪检查，启动完成后返回 `ok`，不探依赖 |
| redirector | `GET` | `/metrics` | Prometheus 指标，运维端点 |
| redirector | `GET` | `/r/{code}` | 302 跳转到原始 URL |
| console | `GET` | `/healthz` | 存活检查，不访问依赖，返回 `ok` |
| console | `GET` | `/readyz` | 就绪检查，PostgreSQL 能 Ping 通才返回 `ok` |
| console | `GET` | `/metrics` | Prometheus 指标，运维端点 |
| console | `GET` | `/api/v1/capabilities` | 查询当前已挂载的登录方式插件（`auth_methods`）、特性开关（`features`：`analytics`/`webhooks`/`risk_scan`/`email`）与公开跳转基地址（`public_base_url`），无需鉴权（[ADR 0029](adr/0029-modular-feature-plugins.md)） |
| console | `GET` | `/api/v1/auth/oidc/login` | 发起 OIDC 授权码登录（支持可选相对路径 `?return_to=/...`，仅配置完整 `OIDC_*` 时注册） |
| console | `GET` | `/api/v1/auth/oidc/callback` | OIDC 回调：校验 state、PKCE 与 ID Token 并建立服务端会话（有 `return_to` 时 302 跳转，否则返回 200 JSON；仅配置完整 `OIDC_*` 时注册） |
| console | `GET` | `/api/v1/auth/github/login` | 发起 GitHub OAuth 2.0 登录（支持可选 `?return_to=/...`，仅配置完整 `GITHUB_*` 时注册） |
| console | `GET` | `/api/v1/auth/github/callback` | GitHub 回调并建立服务端会话（仅配置完整 `GITHUB_*` 时注册） |
| console | `POST` | `/api/v1/auth/logout` | 吊销当前服务端会话并清除会话 Cookie（配置 OIDC 或 GitHub 时注册） |
| console | `GET` | `/api/v1/me` | 返回当前登录用户信息与所属组织列表（配置 OIDC 或 GitHub 时注册） |
| console | `POST` | `/api/v1/orgs` | 当前登录用户自助创建组织并成为 `owner`（仅会话 + CSRF，不要求 `X-Org-ID`） |
| console | `GET` | `/api/v1/members` | 当前组织成员列表（配置 OIDC 或 GitHub 时注册，需要 `members:read`） |
| console | `PATCH` | `/api/v1/members/{user_id}` | 修改当前组织成员角色（配置 OIDC 或 GitHub 时注册，需要 `members:write`） |
| console | `DELETE` | `/api/v1/members/{user_id}` | 将成员移出当前组织（配置 OIDC 或 GitHub 时注册，需要 `members:write`） |
| console | `GET` | `/api/v1/invitations` | 当前组织邀请列表（配置 OIDC 或 GitHub 时注册，需要 `members:read`） |
| console | `POST` | `/api/v1/invitations` | 发起当前组织邮箱邀请（配置 OIDC 或 GitHub 时注册，需要 `members:write`） |
| console | `DELETE` | `/api/v1/invitations/{id}` | 撤销待兑现邀请（配置 OIDC 或 GitHub 时注册，需要 `members:write`） |
| console | `GET` | `/api/v1/api-keys` | 当前组织 API Key 列表（不含明文，需要 `apikeys:read`，仅 `owner`/`admin` 会话） |
| console | `POST` | `/api/v1/api-keys` | 为当前组织签发 API Key（明文只返回一次，需要 `apikeys:write`，仅 `owner`/`admin` 会话） |
| console | `DELETE` | `/api/v1/api-keys/{id}` | 吊销当前组织的 API Key（需要 `apikeys:write`，仅 `owner`/`admin` 会话） |
| console | `GET` | `/api/v1/audit-logs` | 查询当前组织的操作审计日志（支持按 `action` 筛选与 Keyset 游标分页，需要 `audit:read`） |
| console | `GET` | `/api/v1/webhooks` | 当前组织 Webhook 订阅列表（不含密文，需同时配置 `WEBHOOK_SECRET_KEY` 且启用 OIDC/GitHub 登录，需要 `webhooks:read`，仅 `owner`/`admin` 会话） |
| console | `POST` | `/api/v1/webhooks` | 创建 Webhook 订阅（`secret` 明文仅返回一次，最多 5 个，需要 `webhooks:write`，仅 `owner`/`admin` 会话） |
| console | `PATCH` | `/api/v1/webhooks/{id}` | 修改 Webhook `url`、`description`、`events` 或 `enabled`（手动启用清零连续失败计数，需要 `webhooks:write`） |
| console | `DELETE` | `/api/v1/webhooks/{id}` | 删除 Webhook 订阅并写审计日志（排队任务自动跳过，历史投递明细不可再查并由后台保留期任务分批清理；返回 204，需要 `webhooks:write`） |
| console | `POST` | `/api/v1/webhooks/{id}/rotate-secret` | 轮换签名密钥（旧密钥立即失效，新 `secret` 明文仅返回一次，需要 `webhooks:write`） |
| console | `POST` | `/api/v1/webhooks/{id}/test` | 异步入队一条 `webhook.ping` 测试任务（返回 `202 Accepted`，需要 `webhooks:write`） |
| console | `GET` | `/api/v1/webhooks/{id}/deliveries` | 按 Keyset 游标分页查询指定 Webhook 的投递历史（需要 `webhooks:read`） |
| console | `GET` | `/`（及前端路径） | 可选：配置 `WEB_DIST_DIR` 时托管最小前端控制台 SPA，`/api/*`、`/r/*`、运维路径严格保持隔离 |
| console | `POST` | `/api/v1/links` | 创建短链（需要 `links:write`） |
| console | `GET` | `/api/v1/links` | 当前租户的链接列表，支持排序和游标分页（需要 `links:read`） |
| console | `GET` | `/api/v1/links/{code}` | 单条详情，包含已过期、已停用和已封禁的链接（需要 `links:read`） |
| console | `PATCH` | `/api/v1/links/{code}` | 修改 `url`、`expires_at` 和/或 `disabled`（需要 `links:write`；同时有 `links:read` 才返回链接，否则 204） |
| console | `DELETE` | `/api/v1/links/{code}` | 硬删除，返回 204（需要 `links:write`） |
| console | `GET` | `/api/v1/links/{code}/stats` | 点击统计：日趋势、来源、设备、客户端、访客（需要 `stats:read`；`FEATURE_ANALYTICS=false` 时不注册返回 404） |

本地开发时 redirector 默认监听 `:8080`，console 默认监听 `:8081`；生产可分别配置 `OPS_ADDR`、`CONSOLE_OPS_ADDR` 将 `/healthz`、`/readyz`、`/metrics` 与业务端口彻底物理隔离（未设时共端口，由 `deploy/nginx.conf` / `deploy/nginx.stack.conf` 不转发运维路径）。管理面除按租户限流外，在 `authenticate` 之前还设有按客户端 IP 的鉴权前有界限流（超限返回 429）。

管理 API 支持两种凭证，按 `Authorization` 头优先且不回退：

- **API Key**（带 `Authorization: Bearer slk_...`）：租户与权限只来自密钥本身，`X-Org-ID` 请求头和 `sl_session` Cookie 一律忽略，也不受跨站写保护约束。如果请求带了 `Authorization` 头但格式不对或密钥无效，直接返回 401，不会回退到会话 Cookie。
- **用户会话**（不带 `Authorization` 头、携带 `sl_session` Cookie）：需要同时带 `X-Org-ID: <组织 ID>` 请求头。这条路径的响应一律带 `Cache-Control: no-store`。同名 Cookie 多于一份时返回 401，不选用其中任何一份，也不查库。缺失或格式非法的 `X-Org-ID` 返回 400，此时不查会话是否已失效。非安全方法（`POST`/`PATCH`/`DELETE`）会经过 Go 标准库 `http.CrossOriginProtection` 检查，跨站写请求返回 403 `{"error":"cross-origin request rejected"}`。令牌格式非法，或 `X-Org-ID` 合法但令牌无效、过期或已登出，返回 401，且不带 `WWW-Authenticate`。没有 Cookie，或未启用会话存储时，401 仍带 `Bearer`，方便只使用 API Key 的客户端。用户不是该组织成员或组织不存在一律返回 404 `{"error":"organization not found"}`，不暴露组织是否存在。成员角色变更、移出组织、登出或调整租户限流后，下一次请求立刻生效；管理 API 要带上合法的 `X-Org-ID` 才会表现为 401，否则仍是 400。

链接的 `tenant_id` 由服务端从鉴权结果写入，请求体、查询参数和路径里都不能传。其他租户的链接和不存在的链接一律返回 404，不透露链接是否存在；自己的被封禁链接是例外，修改和删除返回 403。每个管理请求按顺序经过鉴权、按租户限流和权限校验：凭证无效返回 401，超限返回 429 和 `Retry-After`，缺少当前接口所需权限（API Key 看 `scopes`，用户看组织角色映射）返回 403 和 `{"error":"forbidden"}`。每个租户还有每月创建数配额，满了返回 403。配额和限流都由租户配置决定，查看和调整见 [runbook](runbook.md)。公开跳转、探针、指标和未鉴权的 401 流量不经过限流。

## 创建短链

```bash
curl -i -X POST localhost:8081/api/v1/links \
  -H 'Authorization: Bearer slk_...' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/path?q=1"}'
```

`slk_` 后面是 64 位小写十六进制。明文只在 `cmd/console key issue` 成功时出现一次，服务里只存 SHA-256。密钥代表一个租户，链接的 `tenant_id` 由服务端写入，请求体里不能传。签发、查看和吊销见 [runbook](runbook.md)。

成功：

```http
HTTP/1.1 201 Created
Content-Type: application/json; charset=utf-8
Location: /r/AbC123xy
X-Request-ID: 6f1c0b5e9a2d47c8b3e1d4a607958f12

{"code":"AbC123xy","url":"https://example.com/path?q=1","status":"active","created_at":"2026-11-15T10:00:00Z","clicks":0,"clicks_today":0}
```

规则：

- 请求体最多约 12KB。
- `url` 必填，最多 2048 字节。
- 只允许 `http` / `https` 绝对 URL。
- URL 内部不能有空白；空格需要百分号编码。
- 请求体必须恰好是一个 JSON 值，尾部多余内容返回 400。
- `expires_at` 可选，用 RFC3339 时间，例如 `"2026-12-01T00:00:00Z"`。省略或 `null` 表示永不过期。必须是将来的时间，到期时刻本身算过期。格式不对时错误信息是 `invalid expires_at`。过期后短码仍被占着，跳转返回 404。响应里带上同一个时刻；没有设置时响应不包含这个字段。
- 未知字段返回 400。`expires_at` 拼错时如果被静默忽略，会创建一条永不过期的链接。
- 响应新增 `status`、`created_at`、`clicks` 和 `clicks_today`；新链接的 `status` 是 `active`，计数都是 0。这是兼容扩展，老客户端可以忽略。

配额和限流：

- 租户每个 UTC 自然月最多创建 `link_quota` 条链接（默认 1000），计的是创建动作：链接之后过期、停用、被封禁或删除都不返还额度。每月 1 日 00:00 UTC 起用量重新从 0 计，没用完的额度不结转。用完返回 403：

  ```http
  HTTP/1.1 403 Forbidden
  Content-Type: application/json; charset=utf-8

  {"error":"link quota exceeded"}
  ```

- 管理 API 按租户限流。超限返回 429，`Retry-After` 是整数秒，至少 1：

  ```http
  HTTP/1.1 429 Too Many Requests
  Retry-After: 1
  Content-Type: application/json; charset=utf-8

  {"error":"rate limit exceeded"}
  ```

- 限制是每租户的配置，`rate_limit_per_second` 为 0 表示不限流，`link_quota` 为 0 表示不能创建。console 多副本时每个副本各自限流，实际总量是配置值乘以副本数。
- 403 和 429 都是租户策略，不是存储故障。403 只能等下个月 1 日 00:00 UTC 重置，或由运维调高配额；删除链接不会返还额度。429 按 `Retry-After` 等待后重试。

## 管理接口的链接表示

创建、详情、修改和列表项使用同一个对象：

```json
{
  "code": "AbC123xy",
  "url": "https://example.com/path?q=1",
  "status": "active",
  "expires_at": "2026-12-01T00:00:00Z",
  "created_at": "2026-11-15T10:00:00Z",
  "clicks": 42,
  "clicks_today": 3
}
```

`expires_at`、`disabled_at`、`banned_at` 和 `ban_reason` 为空时省略。启用统计（`FEATURE_ANALYTICS=true`）时 `clicks` 是终身累计、`clicks_today` 是 UTC 今天的点击数（包括 `0`）；关闭统计（`FEATURE_ANALYTICS=false`）时响应直接省略 `clicks` 与 `clicks_today` 字段，不输出假默认值 `0`（[ADR 0029](adr/0029-modular-feature-plugins.md)）。

`status` 总是有，按优先级取第一个成立的值：

| `status` | 含义 | 谁可以撤销 |
| --- | --- | --- |
| `banned` | 平台封禁，附 `banned_at` 和 `ban_reason` | 只有平台 |
| `disabled` | 租户停用，附 `disabled_at` | 租户 |
| `expired` | 已过 `expires_at` | 租户改 `expires_at` |
| `active` | 可以跳转 | — |

几种状态可以同时成立，`status` 只是选择一个对外展示；比如已停用的链接再被封禁，解封后仍然是 `disabled`。`ban_reason` 取值为 `phishing`、`malware`、`adult`、`gambling`、`drugs`、`spam`、`other`。封禁来源 `ban_source` 不对外。客户端不要再根据 `expires_at` 自己推状态，直接读 `status`。

## 列表

```bash
curl -s localhost:8081/api/v1/links?limit=20 \
  -H 'Authorization: Bearer slk_...'
```

查询参数：

| 参数 | 说明 |
| --- | --- |
| `limit` | 每页条数，默认 20，最大 100；其他值返回 400 |
| `sort` | `id`（默认）、`clicks`（历史累计）、`clicks_today`（今日）；当 `FEATURE_ANALYTICS=false` 时传入 `clicks` 或 `clicks_today` 返回 `400 Bad Request`（`analytics is disabled`） |
| `status` | 可选状态过滤：`active`、`disabled`、`expired`、`banned`（按 `banned > disabled > expired > active` 互斥分类）；非法值返回 400 |
| `q` | 可选关键词搜索：在当前租户内对短码 `code` 和目标 `url` 做大小写不敏感子串匹配（首尾空白忽略，最长 200 字节且不可含控制字符，超限返回 400） |
| `cursor` | 上一页响应里的 `next_cursor`，原样传回；不透明 |

列表固定降序，未传 `status` 时包含全部状态的链接。列表只有当前租户的行。

```json
{
  "items": [
    {"code":"AbC123xy","url":"https://example.com/path","status":"active","created_at":"2026-11-15T10:00:00Z","clicks":42,"clicks_today":3}
  ],
  "next_cursor": "42:7"
}
```

没有下一页时省略 `next_cursor`。不返回总数。`sort=id` 的游标稳定，适合翻完整列表；`clicks` 和 `clicks_today` 会随点击变化，翻页时可能出现重复或漏项，适合看板 Top N，不适合精确导出。非法 `limit`、`sort` 或游标返回 400。

## 详情

```bash
curl -s localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...'
```

返回 200 和链接对象。过期、停用和封禁的链接在管理视角仍然可见，`status` 标出它们的派生状态；被封禁的链接只读。不存在、已删除或其他租户的链接返回 404。

## 修改

```bash
curl -i -X PATCH localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/new","expires_at":null}'
```

只处理 `url`、`expires_at` 和 `disabled`：

- `url` 出现时按创建规则校验。
- `expires_at` 出现且为字符串时必须是将来时间。
- `expires_at` 出现且为 `null` 时清除过期时间，改为永不过期。
- `disabled` 出现且为 `true` 时停用，为 `false` 时启用；重复停用保留第一次的 `disabled_at`。`null` 和非布尔值返回 400。
- 字段没出现时保持原值；三个字段都可以在一次请求里一起改。
- 空对象、三个字段都没出现、未知字段、尾部多余内容都返回 400。
- 不能改 `code`、`tenant_id`、`created_at`、`clicks`、`clicks_today`。

成功时，密钥同时有 `links:read` 则返回 200 和修改后的链接对象；只有 `links:write` 则返回 204，响应体为空。写权限不能借这次响应读出 `url`、过期时间、停用时间和点击数。不存在或其他租户的链接仍然是 404。修改 URL 不改变过期状态；把 `expires_at` 改到将来可以让过期链接重新生效。停用的链接仍然可以改 URL、改过期时间和删除。修改采用最后提交者生效，不做 `If-Match` 或版本号。

被封禁的链接对租户只读：`PATCH` 返回 403 和 `{"error":"link is banned"}`，任何字段都不改。其他租户的链接不管封没封都是 404。封禁由平台运维命令执行，没有 HTTP 接口；命令和原因取值见 [runbook](runbook.md) 的「封禁和解封链接」。

修改提交后 console 会覆盖 Redis 缓存，之后开始的跳转看到新值；链接不可跳转（过期、停用或封禁）时写负缓存，跳转直接 404。写完正缓存后还会按跳转查询复查一次库：如果链接已经被并发删除、停用或封禁，补写负缓存，盖掉这次晚到的正缓存（ADR 0016）。已经发出、正在处理的跳转仍可能按旧 URL 返回一次 302。

## 删除

```bash
curl -i -X DELETE localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...'
```

成功返回 204，无响应体。硬删除会同时删除链接和它的统计行，删除后不能恢复。不存在或其他租户的链接返回 404。

被封禁的链接不能删除，返回 403 和 `{"error":"link is banned"}`：链接这一行就是封禁记录，删掉之后违规 URL、原因和归属租户都没有依据。需要删除时先由平台解封。

删除不是强撤销：已经在途的跳转仍可能返回 302；删除提交后 console 写负缓存，之后的新请求返回 404。Redis 写失败时旧值可能保留到缓存 TTL，下一次写路径会自愈。

## 统计

```bash
curl -s 'localhost:8081/api/v1/links/AbC123xy/stats?from=2026-11-08T00:00:00Z&to=2026-11-15T00:00:00Z' \
  -H 'Authorization: Bearer slk_...'
```

查询参数：

- `from`、`to`：RFC3339，转成 UTC 后按天取整。默认 `to` 为当前 UTC 日期，`from` 为 `to - 7 天`。
- 要求 `from < to`，跨度不超过 90 天；非法参数返回 400。
- 两端所在的整日都计入。链接按 `code + tenant_id` 查；过期链接可以看，已删除或其他租户的链接返回 404。

```json
{
  "code": "AbC123xy",
  "total_clicks": 1234,
  "range_clicks": 120,
  "from": "2026-11-08T00:00:00Z",
  "to": "2026-11-15T00:00:00Z",
  "clicks": [{"day":"2026-11-08","clicks":10}],
  "referrers": [{"referrer":"news.ycombinator.com","clicks":30}],
  "devices": [{"device":"desktop","clicks":40},{"device":"mobile","clicks":50}],
  "clients": [{"client":"browser","clicks":80},{"client":"bot","clicks":5}],
  "daily_visitors": [{"day":"2026-11-08","visitors":8}],
  "unique_visitors": 42
}
```

- `total_clicks` 取 `links.clicks`，是终身累计；`range_clicks` 是本次时间范围内的日表之和。迁移前的点击只有 `total_clicks`，没有时间分布。
- `clicks`、`daily_visitors` 按天补零；`devices` 固定返回 `desktop`、`mobile`、`tablet`、`bot`、`other` 五个类别，`clients` 固定返回 `browser`、`bot`、`cli`、`other` 四个类别，没有数据的补 0。
- `referrers` 取范围内点击数最多的 10 个 host，按点击数降序、host 升序；不返回合并的 long tail 行。
- `unique_visitors` 是 `daily_visitors` 之和，按天去重后相加，不是跨天唯一人数。同一访客换 IP、换 User-Agent 或跨天都会算成多个。
- 统计来自写入时的日聚合，只能看聚合结果，不能回查单次点击。来源、设备、客户端和访客从这一版上线后开始记录；迁移前的历史只有 `total_clicks`。

## 能力发现与回调跳转

`GET /api/v1/capabilities` 始终注册、无需鉴权，响应带 `Cache-Control: no-store`，内容由服务端实际挂载的 `AuthMethod` 与 `Module` 自动推导（[ADR 0029](adr/0029-modular-feature-plugins.md)）：

```json
{
  "auth_methods": [
    {"id": "oidc", "kind": "redirect", "login_url": "/api/v1/auth/oidc/login"}
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

未配置 `PUBLIC_BASE_URL` 时省略 `public_base_url`；未配置登录插件时 `auth_methods` 为 `[]`。前端控制台据该端点动态渲染登录按钮、侧边栏菜单与统计控件。

所有跳转型登录插件（`/api/v1/auth/{id}/login`，例如 `/api/v1/auth/oidc/login` 与 `/api/v1/auth/github/login`）支持可选查询参数 `?return_to=/...`：

- 仅接受以单个 `/` 开头的相对路径，拒绝 `//`、`\`、控制字符、超过 256 字节以及 `/api`、`/api/*`、`/r`、`/r/*`，不合法返回 `400`。
- 合法值随 `state`/`nonce` 一起写入 HMAC 签名的流程 Cookie。回调建完会话并写入 `sl_session` 后，若 `return_to` 非空则返回 `302 Found` 跳回该相对路径；省略时维持原样返回 `200 OK` JSON，兼容现有脚本和 `curl` 流程。

## OIDC 登录

只有在 `OIDC_ISSUER`、`OIDC_CLIENT_ID`、`OIDC_CLIENT_SECRET`、`OIDC_REDIRECT_URL` 四个环境变量全部配置时，console 才注册 OIDC 登录和回调路由；全部为空时这两个路径返回 404，不影响纯 API Key 部署；只配一部分时启动失败。两个端点都不走 API Key 鉴权和按租户限流，响应一律带 `Cache-Control: no-store`，并且只接受 `GET`（`HEAD` 返回 405，避免探针消费授权码）。

发起登录：

```bash
curl -i -c cookies.txt localhost:8081/api/v1/auth/oidc/login
```

返回 `302 Found`，`Location` 指向身份提供方的授权端点（带 `response_type=code`、`scope=openid email profile`、随机 `state`、PKCE `S256` 的 `code_challenge` 和随机 `nonce`），同时写入短期流程 Cookie `sl_oidc_flow`：

- `HttpOnly`、`SameSite=Lax`、`Path=/api/v1/auth/oidc/callback`、`Max-Age=600`（10 分钟）。
- `OIDC_REDIRECT_URL` 为 `https://` 时带 `Secure`；本地 HTTP 开发和冒烟（`http://127.0.0.1:...`）不带 `Secure`。
- 值是 HMAC 签过的，客户端不能改 `state`、PKCE verifier、nonce 或过期时间。

回调建会话：

```http
GET /api/v1/auth/oidc/callback?code=...&state=...
Cookie: sl_oidc_flow=...
```

服务端行为：

1. 读取流程 Cookie。缺失、HMAC 不符、损坏或超过 10 分钟返回 400 `{"error":"invalid login state"}`，并且不写 `Set-Cookie`，避免清掉浏览器里仍有效的登录。
2. 常量时间比对回调 `state` 与流程 Cookie 中的 `state`。不符同样返回 400，不删除 Cookie，也不联系身份提供方。
3. `state` 相符后立即删除一次性流程 Cookie。上游返回 `error` 参数（如 `access_denied`）返回 401 `{"error":"authentication failed"}`；缺少 `code` 返回 400 `{"error":"missing authorization code"}`。
4. 用 `code` 和 PKCE `code_verifier` 向令牌端点换令牌，用 `go-oidc` 校验 ID Token 的签名（JWKS）、`iss`、`aud`、`exp`，再与流程 Cookie 中的 `nonce` 做常量时间比对。ID Token 只在回调里消费一次，不持久化。
5. 按 `(issuer, subject)` 在 PostgreSQL 里查找或创建用户（不按邮箱自动关联不同提供方）。再次登录只在新声明非空时覆盖邮箱和昵称；邮箱缺失时保留原来的 `email_verified`，同一邮箱已经验证过的不会被后来的 `false` 降回去。如果本次已校验的 ID Token 同时满足 `email_verified` 为 `true` 且邮箱非空，会在同一事务里把该邮箱名下尚未过期、未撤销的待兑现组织邀请全部兑现成 `memberships`（已经是组织成员时保留原有角色）。每次登录都用 `crypto/rand` 签发全新的服务端会话令牌（`sls_` 加 64 位小写十六进制，库里只存 SHA-256，默认有效期 7 天）。
6. 写入会话 Cookie `sl_session`（`HttpOnly`、`SameSite=Lax`、`Path=/api/`、`Max-Age` 取数据库时钟下 `expires_at - now()`，`OIDC_REDIRECT_URL` 为 `https://` 时带 `Secure`），返回 `200 OK` 和用户摘要：

```json
{
  "user": {
    "id": 1,
    "email": "alice@example.com",
    "name": "Alice"
  },
  "expires_at": "2026-11-28T14:15:00Z"
}
```

响应体不包含会话明文令牌。授权码过期/重放或 ID Token 校验失败返回 401 `{"error":"authentication failed"}`；身份提供方不可达、JWKS 拉取失败、令牌端点返回 `server_error` / `temporarily_unavailable`、5xx 或客户端密钥配置错误返回 502 `{"error":"identity provider unavailable"}`；超时返回 504。

## GitHub OAuth 2.0 登录

只有在 `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET`、`GITHUB_REDIRECT_URL` 三个环境变量全部配置时，console 才注册 GitHub 登录和回调路由（全部为空时返回 404，只配一部分时启动失败），可与 `OIDC_*` 独立启用或同时启用。两个端点同样不走 API Key 鉴权和按租户限流，响应一律带 `Cache-Control: no-store` 且只接受 `GET`。

- `GET /api/v1/auth/github/login`：返回 `302 Found` 跳转到 `https://github.com/login/oauth/authorize`（`scope=read:user user:email`，带随机 `state` 与 PKCE `S256` `code_challenge`，**不传 `nonce`**），并写入短期 `sl_github_flow` Cookie（`Path=/api/v1/auth/github/callback`、`HttpOnly`、`SameSite=Lax`、`Max-Age=600`、`GITHUB_REDIRECT_URL` 为 `https://` 时带 `Secure`）。
- `GET /api/v1/auth/github/callback`：校验 `sl_github_flow` Cookie 与 `state` 后立即清除该 Cookie；用 `code` + PKCE `code_verifier` 换取 GitHub access token，再在线调用 `GET /user`（取不可变的数字用户 `id` 作为 `subject`、`name` 为空时回退到 `login`）与 `GET /user/emails`（只信任 `primary` 且 `verified` 的主邮箱），以 `(issuer="https://github.com", subject="<numeric id>")` 查找或创建用户、按已验证主邮箱兑现待兑现邀请，并签发同一个 `sl_session` 服务端会话（access token 只在本请求使用，不落库、不打日志）。
- 与标准 OIDC 的协议差异对照：

| 对比维度 | 标准 OIDC（Dex） | GitHub 纯 OAuth 2.0 |
| --- | --- | --- |
| 发现与密钥 | `OIDC_ISSUER/.well-known/openid-configuration` + JWKS 自动拉取 | 无发现文档，硬编码官方授权/令牌/REST 地址 |
| 身份承载与重放防护 | 签名 JWT（ID Token）+ `nonce` 常量时间比对 | 无 ID Token 与 `nonce`；换回 `access_token` 后在线查 API |
| 令牌端点错误语义 | HTTP 400（`invalid_grant` 等） | 授权码/凭据错误常返回 **HTTP 200** + 表单 `error=...` |
| 主体与邮箱验证 | 声明自带 `sub`、`email`、`email_verified` | `/user` 缺可信验证状态，须调 `/user/emails` 取 `primary && verified` |

- 账号隔离：按 `(issuer, subject)` 区分身份，GitHub 与 OIDC 即使主邮箱相同也不会自动合并成同一个平台用户。

## 当前用户与登出

配置完整 `OIDC_*` 或 `GITHUB_*` 之一后，console 同时注册 `GET /api/v1/me` 和 `POST /api/v1/auth/logout`（两者都未配置时返回 404）。两个端点只认唯一的 `sl_session` Cookie，不走 `X-Org-ID` 校验和按租户限流，响应一律带 `Cache-Control: no-store`。同名 Cookie 多于一份时返回 401，不查询、不吊销。

### 查询当前用户

```bash
curl -s -b cookies.txt localhost:8081/api/v1/me
```

不需要传 `X-Org-ID`（传了也会忽略），即使尚未加入任何组织也能调用；只带 `Authorization: Bearer slk_...` 而没有有效会话 Cookie 时返回 401 `{"error":"unauthorized"}`。成功返回 `200 OK`：

```json
{
  "user": {
    "id": 1,
    "email": "alice@example.com",
    "name": "Alice"
  },
  "expires_at": "2026-11-28T14:15:00Z",
  "organizations": [
    {
      "id": 1,
      "name": "acme",
      "role": "owner",
      "link_usage": 12,
      "link_quota": 1000
    }
  ]
}
```

尚未加入任何组织时 `organizations` 为 `[]`。

### 登出

```bash
curl -i -X POST -b cookies.txt -c cookies.txt localhost:8081/api/v1/auth/logout
```

经过 `http.CrossOriginProtection` 检查，跨站 `POST` 返回 403 `{"error":"cross-origin request rejected"}`（不吊销会话也不清 Cookie）。同源或非浏览器请求在数据库里将当前会话标记为已吊销并清除 `sl_session` Cookie，返回 `204 No Content`。无 Cookie、Cookie 格式无效或已吊销时同样返回 204（幂等）。请求里有多份 `sl_session` 时返回 401，不吊销、也不清 Cookie。登出后，下一次 `/api/v1/me` 立刻返回 401。管理 API 在 `X-Org-ID` 合法时立刻返回 401；`X-Org-ID` 缺失或非法时仍返回 400，不因会话已失效而改成 401。

## 成员管理

配置完整 `OIDC_*` 或 `GITHUB_*` 之一后，console 同时注册以下成员管理接口（两者都未配置时返回 404）。三个接口都走管理 API 鉴权链（要求 `sl_session` + `X-Org-ID`、受按租户限流和 `CrossOriginProtection` 保护，响应带 `Cache-Control: no-store`）。API Key 永远不包含 `members:read` / `members:write` scope，携带有效 API Key 访问这三个接口固定返回 403 `{"error":"forbidden"}`。

### 列出成员

```bash
curl -s -b cookies.txt -H 'X-Org-ID: 1' localhost:8081/api/v1/members
```

需要 `members:read`（`owner`、`admin`、`member`、`viewer` 均可）。按 `user_id` 升序返回当前组织的全部成员：

```json
{
  "items": [
    {
      "user_id": 1,
      "email": "alice@example.com",
      "name": "Alice",
      "role": "owner",
      "joined_at": "2026-11-22T04:00:00Z"
    }
  ]
}
```

### 修改成员角色

```bash
curl -i -X PATCH -b cookies.txt -H 'X-Org-ID: 1' \
  -H 'Content-Type: application/json' \
  -d '{"role":"member"}' \
  localhost:8081/api/v1/members/2
```

需要 `members:write`（`owner`、`admin`）。成功后返回 `200 OK` 和更新后的单个成员对象；修改在数据库提交，目标成员原有会话的下一次请求立刻按新角色生效。

资源级规则与错误码：

- `owner` 可以修改任何成员的角色（含自我降级），但不能把组织最后一名 `owner` 降成非 `owner`（返回 409 `{"error":"organization must have at least one owner"}`）。
- `admin` 不能修改 `owner`，也不能把任何人（包括不存在的目标）设为 `owner`，否则返回 403 `{"error":"forbidden"}`；可以修改 `admin`、`member`、`viewer`（含自我降级）。
- `{user_id}` 非正整数、JSON 非法、包含未知字段或 `role` 非法时返回 400；目标用户不属于当前组织返回 404 `{"error":"member not found"}`。

### 移出成员

```bash
curl -i -X DELETE -b cookies.txt -H 'X-Org-ID: 1' \
  localhost:8081/api/v1/members/2
```

需要 `members:write`（`owner`、`admin`）。成功返回 `204 No Content`，无响应体。只删除当前组织的 `memberships` 行，不吊销该用户的会话：

- 被移出成员的现有会话再带当前组织的 `X-Org-ID` 访问任何管理接口，下一次请求立刻返回 404 `{"error":"organization not found"}`，`GET /api/v1/me` 里也不再包含该组织。
- 该用户在其他组织中的成员关系和会话访问不受影响。
- `admin` 不能移出 `owner`（返回 403 `{"error":"forbidden"}`）；不能移出组织最后一名 `owner`（返回 409 `{"error":"organization must have at least one owner"}`）；目标不是当前组织成员返回 404 `{"error":"member not found"}`。

## 租户自助闭环（组织、邀请与 API Key）

配置完整 `OIDC_*` 或 `GITHUB_*` 之一后，console 同时注册以下自助管理接口（详见 [ADR 0021](adr/0021-tenant-self-service-and-web-console.md)）。

### 自助创建组织

```bash
curl -i -X POST -b cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Corp"}' \
  localhost:8081/api/v1/orgs
```

- 只认唯一的 `sl_session` Cookie，不要求 `X-Org-ID`，拒绝带 `Authorization` 头的请求（返回 401）；经过 `http.CrossOriginProtection` 检查，响应带 `Cache-Control: no-store`。
- 名称去除首尾空白后不能为空且不超过 100 字节（否则 400）；每个用户作为 `owner` 最多拥有 10 个组织（并发加行锁校验，超限返回 409 `{"error":"organization limit exceeded"}`）。
- 成功在同一事务内创建 `tenants` 与 `memberships(role='owner')`，返回 `201 Created`：`{"id":2,"name":"Acme Corp","role":"owner","created_at":"..."}`。

### 组织邀请管理

三个接口均走管理 API 鉴权链（要求 `sl_session` + `X-Org-ID`，受限流和跨站写保护），权限沿用 `members:read` / `members:write`：

- `GET /api/v1/invitations`（需要 `members:read`）：按 `id` 降序返回当前组织的全部邀请，含派生状态 `status`（`pending`/`accepted`/`revoked`/`expired`）、`invited_by`、`created_at`、`expires_at`、`accepted_at`、`revoked_at`。
- `POST /api/v1/invitations`（需要 `members:write`）：请求体 `{"email":"bob@example.com","role":"member","expires_at":"..."}`（`expires_at` 可选，须为将来时间）。自动写入 `invited_by = 当前用户 ID`；遵循角色等级（`admin` 不可邀请 `owner`，返回 403）；同组织同邮箱已存在待兑现邀请返回 409。同一事务原子入队 `invitation.email` 任务；当 `console worker` 配置了 `SMTP_*` 时异步发送包含组织名与登录链接（`CONSOLE_PUBLIC_URL`）的邀请邮件（未配置 SMTP 时直接跳过，不影响人工复制登录入口）。受邀用户随后以该已验证邮箱登录时自动兑现为组织成员。
- `DELETE /api/v1/invitations/{id}`（需要 `members:write`）：撤销当前组织的待兑现邀请，成功返回 `204 No Content`。跨组织或不存在返回 404；非 `pending` 状态返回 409；`admin` 不可撤销 `owner` 邀请（403）。

### 组织 API Key 自助管理

三个接口均走管理 API 鉴权链，使用新增权限 `apikeys:read` / `apikeys:write`（**仅授 `owner` 与 `admin` 会话**；API Key 永远不能拥有这两个 scope，携带 API Key 调用一律 403），响应一律带 `Cache-Control: no-store`：

- `GET /api/v1/api-keys`（需要 `apikeys:read`）：返回当前组织全部 API Key 的元数据（`id`、可选 `name`、`scopes`、`status`、`created_by`、`created_at`、可选 `last_used_at`、`expires_at`、`revoked_at`），永不返回明文或哈希。`last_used_at` 在密钥通过鉴权时按 5 分钟节流窗口更新。
- `POST /api/v1/api-keys`（需要 `apikeys:write`）：请求体 `{"name":"prod-ci","scopes":["links:read","links:write"],"expires_at":"..."}`（`name` 可选，最长 100 字节且不可含控制字符；`scopes` 必填且仅允许 `links:read`/`links:write`/`stats:read`；`expires_at` 可选且须为将来时间）。写入 `created_by = 当前用户 ID`，返回 `201 Created` 与仅此一次出现的明文 `"api_key":"slk_..."`。
- `DELETE /api/v1/api-keys/{id}`（需要 `apikeys:write`）：按 `(id, tenant_id)` 隔离吊销当前组织的 API Key，成功返回 `204 No Content`（立即生效）；其他组织或不存在的 ID 返回 404，已吊销返回 409。

### 组织操作审计日志

```bash
curl -i -b cookies.txt -H 'X-Org-ID: 1' \
  'localhost:8081/api/v1/audit-logs?limit=20&action=link.ban'
```

- 需要 `audit:read`（**仅授 `owner` 与 `admin` 会话**；`member`、`viewer` 与任何 API Key 一律返回 403），走管理 API 鉴权链并附带 `Cache-Control: no-store`（详见 [ADR 0023](adr/0023-audit-logs.md)）。
- 查询参数：
  - `limit`：每页条数，默认 `20`，最大 `100`，非法值返回 400 `{"error":"invalid limit"}`。
  - `action`：可选动作过滤，枚举值：`org.create`、`tenant.limits_update`、`link.create`、`link.update`、`link.delete`、`link.ban`、`link.unban`、`member.role_update`、`member.remove`、`invitation.create`、`invitation.revoke`、`invitation.redeem`、`apikey.issue`、`apikey.revoke`、`webhook.create`、`webhook.update`、`webhook.delete`、`webhook.rotate_secret`、`webhook.test`、`webhook.auto_disable`；非法值返回 400 `{"error":"invalid action"}`。
  - `cursor`：可选游标（上一页返回的 `next_cursor`，即最后一条审计记录的正整数 `id`）；非法值返回 400 `{"error":"invalid cursor"}`。
- 返回 `200 OK`：`{"items":[...],"next_cursor":"..."}`（无下一页时省略 `next_cursor`）。每项包含 `id`、`actor_kind`（`user` / `api_key` / `operator` / `system`）、可选 `actor_id` / `actor_email` / `actor_name`、`action`、`target_type`、`target_id`、可选 `request_id`（HTTP 请求 ID 或 worker `job:<id>`）、非敏感 `metadata` 对象与 `created_at`（UTC RFC3339）。
- 审计事件与业务变更在同一数据库事务内原子提交，仅记录真实发生的状态变更（重复幂等 `ban`/`unban` 或校验失败的写操作不写入审计行）。

## Webhooks

仅当 `console` 同时配置了 64 位小写 hex 主密钥 `WEBHOOK_SECRET_KEY` 且启用了至少一种会话登录方式（`OIDC_*` 或 `GITHUB_*`）时，才会挂载 `/api/v1/webhooks*`（任一缺失时路由返回 404，且 `GET /api/v1/capabilities` 返回 `"webhooks":false`）。详见 [ADR 0027](adr/0027-webhooks-and-async-invitation-email.md)。

全部 Webhook 接口走管理 API 鉴权链（`sl_session` + `X-Org-ID` + 按租户限流 + `CrossOriginProtection`），**仅授予 `owner` 与 `admin` 会话**（`webhooks:read` / `webhooks:write`；API Key 永远返回 403），响应一律带 `Cache-Control: no-store`。每个组织最多配置 `5` 个 Webhook（并发加行锁校验，超限返回 409 `{"error":"webhook limit exceeded"}`）。

### 订阅管理接口

- `GET /api/v1/webhooks`（`webhooks:read`）：按 `id` 降序返回当前组织全部订阅元数据（`id`、`url`、可选 `description`、`events`、`secret_prefix`、`enabled`、`consecutive_failures`、可选 `disabled_reason`/`disabled_at`、可选 `created_by`、`created_at`、`updated_at`）。
- `POST /api/v1/webhooks`（`webhooks:write`）：请求体 `{"url":"https://hooks.example.com/shortlink","description":"prod","events":["link.created","link.banned"]}`。URL 经过严格 SSRF 校验（禁止内网/回环/元数据网段，只允许 80/443 或 `WEBHOOK_ALLOWED_PORTS` 白名单端口，最长 2048 字节）；`events` 为非空子集（可选：`link.created`、`link.updated`、`link.deleted`、`link.banned`、`link.unbanned`）。成功返回 `201 Created` 与仅出现一次的明文 `"secret":"whsec_<64位hex>"`（AES-256-GCM 密文绑定 `tenant_id` AAD 存库）。
- `PATCH /api/v1/webhooks/{id}`（`webhooks:write`）：部分更新 `url`、`description`、`events`、`enabled`（至少传一个字段）。将 `enabled` 设为 `true` 时会清零 `consecutive_failures` 并清空 `disabled_reason`/`disabled_at`。
- `DELETE /api/v1/webhooks/{id}`（`webhooks:write`）：在单事务内删除 `webhooks` 订阅行并写入 `webhook.delete` 审计日志，返回 `204 No Content`。尚未投递的排队任务回表查不到订阅时自动 `Skip`；关联的历史 `webhook_deliveries` 行不再对管理接口可见，由 worker 周期保留期清理任务分批删除。
- `POST /api/v1/webhooks/{id}/rotate-secret`（`webhooks:write`）：立即生成新密钥覆盖密文并返回 `200 OK` + 新 `"secret":"whsec_..."`。
- `POST /api/v1/webhooks/{id}/test`（`webhooks:write`）：无论订阅当前是否启用，立即入队一条 `webhook.ping` 任务（单次尝试不重试，不影响连续失败计数），返回 `202 Accepted`：`{"job_id":123,"delivery_id":"dlv_123"}`。
- `GET /api/v1/webhooks/{id}/deliveries`（`webhooks:read`）：按 `id` 降序分页查询指定订阅的投递尝试记录（支持 `limit` 默认 20/最大 100、`cursor` 游标），每项含 `id`、`webhook_id`、`job_id`、`delivery_id`、`event`、`attempt`、`status`（`success`/`retry`/`failed`）、可选 `http_status`、`duration_ms`、可选 `error_message`/`response_snippet`（已脱敏截断）、`created_at`。

### 投递格式与 HMAC 签名校验

短链写入路径（`link.created`、`link.updated`、`link.deleted`、`link.banned`、`link.unbanned`）在 PostgreSQL 业务事务内按条件 `enabled = TRUE AND events @> ARRAY[$event]` 原子扇出 `webhook.deliver` 任务到 `jobs` 队列。`console worker` 投递 HTTP POST 请求：

- 固定禁用自动重定向（收到 `3xx` 直接判终态失败），默认单次超时 `5s`，按目标 `host` 令牌桶限速（超限调用 `job.Defer` 延迟调度，不消耗尝试次数）。
- 请求头包含：`Content-Type: application/json; charset=utf-8`、`User-Agent: shortlink-webhook/1.0`、`X-Shortlink-Event`、`X-Shortlink-Delivery: dlv_<job_id>`（同一任务跨重试保持不变，供接收端幂等去重）与 `X-Shortlink-Signature: t=<unix_sec>,v1=<hex_hmac>`。
- 签名明文为 `"<unix_sec>." + <原始请求体字节>`，使用订阅明文密钥做 `HMAC-SHA256`（十六进制小写）。建议接收端先检查 `|now - t| <= 300s`，再对完整原始请求体计算 HMAC 并用常量时间比较（如 Go `hmac.Equal` / Python `hmac.compare_digest`）。
- 投递结果判定：`2xx` 记成功（并清零 `consecutive_failures`）；网络超时、`408`、`429`、`5xx` 按独立指数退避重试（默认最多 `6` 次）；`3xx`、除 `408`/`429` 外的 `4xx` 与 SSRF 拦截直接标记终态失败。非 `webhook.ping` 终态失败连续达到阈值（默认 `10`，配置 `0` 关闭）时自动熔断停用订阅并写 `webhook.auto_disable` 系统审计日志。

## 跳转

```bash
curl -i localhost:8080/r/AbC123xy
```

成功返回 302，`Location` 是原始 URL。只有 GET 记一次点击。`GET /r/{code}` 也匹配 HEAD（探针、预览、扫描器），HEAD 同样返回 302，但不记点击。查出链接时客户端已经断开的，不记点击。短码不存在、已过期、已停用或已封禁返回 404，响应不区分这几种状态；存储错误返回 500，处理超时返回 504；回源并发已满返回 503（临时过载，可重试），包括普通缓存未命中超过 `CACHE_MISS_CONCURRENCY` 和 Redis 不可用时的回源。错误体都是 JSON。

点击记录带这些元数据：

- 来源：`Referer` 的 host，转小写并去掉一个前导 `www.`；空值记 `direct`，非法或超长记 `other`。
- 设备：`desktop`、`mobile`、`tablet`、`bot`、`other`。
- 客户端：`browser`、`bot`、`cli`、`other`。
- 访客：`HMAC-SHA256(VISITOR_SALT, 按天盐 + IP + User-Agent)` 的前 16 字节。不存原始 IP 和 User-Agent。

客户端 IP 默认取连接地址。只有 `TRUST_PROXY_HEADERS=true` 时才解析 `X-Forwarded-For`：结合 `TRUSTED_PROXY_CIDRS`（配置了可信代理网段时从右往左剥离可信代理跳，否则取最右一段合法 IP），既防客户端伪造，也防止多层反代（CDN + Nginx）导致 UV 塌缩（[ADR 0024](adr/0024-process-boundary-and-ops-isolation.md)）。`VISITOR_SALT` 在 `FEATURE_ANALYTICS=true` 时由 redirector 启动必填，关闭点击统计（`FEATURE_ANALYTICS=false`）时可留空且不启动点击记录器。

## 健康检查

```bash
curl -i localhost:8080/healthz   # redirector 存活
curl -i localhost:8080/readyz    # redirector 就绪
curl -i localhost:8081/healthz   # console 存活
curl -i localhost:8081/readyz    # console 就绪
```

`/healthz` 返回 200 和 `ok`。它是 liveness 探针，故意不访问数据库：依赖抖一下不应该让编排器重启进程。

`/readyz` 就绪时返回 200 和 `ok`，否则返回 503 和 `unavailable`，编排器用它摘流量。两个进程查的东西不一样：

- console 在 1 秒内 Ping PostgreSQL。每个管理请求都要查库鉴权，库不通就一个请求也处理不了。Redis 不探，写缓存失败不影响管理请求成功。
- redirector 不探依赖，启动时连上 PostgreSQL 和 Redis 之后就一直就绪。Redis 故障时它降级查库，数据库故障时缓存命中的链接还能跳；这两个依赖是所有副本共用的，探针失败会把全部副本同时摘掉，降级就变成了全站不可用。依赖故障看 `/metrics`。

关停开始后，两个进程的 `/readyz` 都返回 503，`/healthz` 仍是 200。监听还开着，这段时间进来的请求照常处理，等 `DRAIN_DELAY`（默认 5s）再关。这段等待给主动探测一个周期把副本摘掉。

两个路径都打在进程自己的端口上，公网入口不转发。

## 指标

```bash
curl -i localhost:8080/metrics   # redirector
curl -i localhost:8081/metrics   # console
```

两个进程各自返回 Prometheus 文本格式，指标名相同，用 Prometheus 的 `job` 区分。redirector 暴露 Go 运行时、进程、HTTP、缓存、存储、连接池和点击指标；console 暴露 Go 运行时、进程、HTTP、存储和连接池指标，以及创建、修改、删除时写缓存的计数。目前暴露的指标：

- `shortlink_http_requests_total`、`shortlink_http_request_duration_seconds`（`method` 为 `GET`、`HEAD`、`POST`、`PATCH`、`DELETE`，其余记成 `other`）
- `shortlink_rate_limited_total`（被按租户限流拒绝的管理请求数；没有 label。两个进程都暴露，只有 console 会累加，redirector 恒为 0）
- `shortlink_ip_rate_limited_total`（管理面鉴权前按客户端 IP 限流拒绝的请求数；仅 console 暴露，防伪造凭证穿透数据库）
- `shortlink_cache_gets_total`（`result` 为 `hit`、`miss`、`fallback`。负缓存记成 `hit`）
- `shortlink_cache_writes_total`（`result` 为 `success`、`skipped`、`error`。创建、修改、删除和回源写缓存都会计数；`skipped` 表示回源的 `SET NX` 发现 key 已有更新的值，可能是并发的创建或管理写。封禁和解封的缓存写入发生在 `console link` 命令里，不进这个进程的 `/metrics`）
- `shortlink_cache_operation_duration_seconds`（缓存操作耗时，`operation` 为 `get`、`set`，`result` 取值同上面两个计数器）
- `shortlink_store_operations_total`、`shortlink_store_operation_duration_seconds`（`operation` 为 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`、`ban`、`unban`；`result` 为 `success`、`not_found`、`quota_exceeded`、`banned`、`url_changed`、`canceled`、`timeout`、`error`。租户修改或删除被封禁的链接记成 `banned`，风控封禁前发现 URL 已被改写记成 `url_changed`，都是策略生效，不算存储失败。管理写（含创建和修改）在写完正缓存后会按跳转查询复查一次，所以 console 的存储指标里还会多出 `operation="get"`。`console link` 不经过这个装饰器，`ban`、`unban` 出现在 `console worker` 的 `/metrics` 里）
- `go_sql_*`（连接池，`db_name="shortlink"`）
- `process_*`（进程 CPU、内存、文件描述符）
- `shortlink_clicks_dropped_total`（缓冲满、没能记下的点击。启动后就是 0，没有 label；只有 redirector 有）
- `shortlink_clicks_written_total`（交给存储的点击数，`result` 为 `success` 或 `error`。启动后两个序列都是 0。写失败时 dropped 仍是 0，要看 `error`；只有 redirector 有）
- `shortlink_clicks_recorder_stopped`（`ClickRecorder` 后台写入协程因 panic 停机时置 `1`，触发紧急告警）
- `shortlink_jobs_processed_total`（后台任务完成计数，`kind` 包括 `link.risk_scan`、`cache.reconcile`、`link.purge_stats`、`webhook.deliver`、`invitation.email`，`result` 为 `done`、`skipped`、`retry`、`dead`；仅 `console worker` 配置 `WORKER_ADDR` 时暴露）
- `shortlink_job_duration_seconds`（后台任务执行耗时直方图，`kind`；仅 `console worker`）
- `shortlink_jobs_in_flight`（当前执行中的后台任务数，`kind`；仅 `console worker`）
- `shortlink_jobs_reaped_total`（超时租约回收次数，`kind`；仅 `console worker`）
- `shortlink_jobs_oldest_pending_age_seconds`（最老待执行任务等待秒数，`kind`，反映异步风控暴露窗口；仅 `console worker`）
- `shortlink_risk_cache_lookups_total`（风控扫描结果缓存查找次数，`result` 为 `hit`、`miss`、`fallback`；仅 `console worker`）
- `shortlink_webhook_deliveries_total`（Webhook 出站投递结果计数，`event` 为订阅事件或 `webhook.ping`，`result` 为 `success`、`retry`、`failed`、`skipped`、`deferred`；仅 `console worker`）
- `shortlink_webhook_delivery_duration_seconds`（Webhook 出站 HTTP 投递耗时直方图，`event`；仅 `console worker`）
- `shortlink_webhook_auto_disabled_total`（Webhook 连续终态失败触发自动熔断停用的累计次数；仅 `console worker`）

它是运维端点，不版本化，公网隔离由部署层保证。

## 剖析

redirector 设了 `PPROF_ADDR` 时，在那个地址上另起一个监听提供 `/debug/pprof/`（Go 的 `net/http/pprof`）。
默认不开，只接受回环地址；业务端口上没有这组路径。用法见 [runbook](runbook.md)。

## 请求编号

经过路由的响应均由服务端签发高熵 128 位十六进制编号，写入响应头 `X-Request-ID`、结构化日志 `request_id` 与审计记录 `audit_logs.request_id`（[ADR 0025](adr/0025-reconcile-cache-async-purge-and-audit-hardening.md)）。客户端传入且符合长度不超过 128、仅含字母/数字/`.`/`_`/`-` 字符集的 `X-Request-ID` 只作为排障线索记录到日志字段 `client_request_id`，不会覆盖服务端签发的内部编号。

## 版本策略

- `/api/v1` 出现不兼容改动时新增 `/api/v2`，v1 按计划保留。
- `/r/{code}` 不随 API 版本变化；已经发出去的短链必须一直可用。
- `/healthz`、`/readyz`、`/metrics` 等运维端点不进版本前缀。`/metrics` 只给抓取端，应用不做鉴权，公网隔离由部署层保证。
- 短码大小写敏感。
