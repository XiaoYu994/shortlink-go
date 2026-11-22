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
| console | `GET` | `/api/v1/auth/login` | 发起 OIDC 授权码登录（仅配置完整 `OIDC_*` 时注册） |
| console | `GET` | `/api/v1/auth/callback` | OIDC 回调：校验 state、PKCE 与 ID Token 并建立服务端会话（仅配置完整 `OIDC_*` 时注册） |
| console | `POST` | `/api/v1/auth/logout` | 吊销当前服务端会话并清除会话 Cookie（仅配置完整 `OIDC_*` 时注册） |
| console | `GET` | `/api/v1/me` | 返回当前登录用户信息与所属组织列表（仅配置完整 `OIDC_*` 时注册） |
| console | `GET` | `/api/v1/members` | 当前组织成员列表（仅配置完整 `OIDC_*` 时注册，需要 `members:read`） |
| console | `PATCH` | `/api/v1/members/{user_id}` | 修改当前组织成员角色（仅配置完整 `OIDC_*` 时注册，需要 `members:write`） |
| console | `DELETE` | `/api/v1/members/{user_id}` | 将成员移出当前组织（仅配置完整 `OIDC_*` 时注册，需要 `members:write`） |
| console | `POST` | `/api/v1/links` | 创建短链（需要 `links:write`） |
| console | `GET` | `/api/v1/links` | 当前租户的链接列表，支持排序和游标分页（需要 `links:read`） |
| console | `GET` | `/api/v1/links/{code}` | 单条详情，包含已过期、已停用和已封禁的链接（需要 `links:read`） |
| console | `PATCH` | `/api/v1/links/{code}` | 修改 `url`、`expires_at` 和/或 `disabled`（需要 `links:write`；同时有 `links:read` 才返回链接，否则 204） |
| console | `DELETE` | `/api/v1/links/{code}` | 硬删除，返回 204（需要 `links:write`） |
| console | `GET` | `/api/v1/links/{code}/stats` | 点击统计：日趋势、来源、设备、客户端、访客（需要 `stats:read`） |

本地开发时 redirector 监听 `:8080`，console 监听 `:8081`；部署时由反向代理按路径前缀转发到两个进程。宿主机进程的入口是 `deploy/nginx.conf`，容器栈的入口是 `deploy/nginx.stack.conf`。两份都只转发 `/r/` 和 `/api/`，不转发 `/metrics`、`/healthz` 和 `/readyz`。创建响应的 `Location` 是相对路径 `/r/{code}`，调用方应拼公开域名，不是 console 的监听地址。管理 API 带版本前缀 `/api/v1`，并且需要通过 API Key 或会话鉴权。跳转路径 `/r/{code}` 不加版本，也不鉴权，因为短链是已经发出去的公开地址。`/healthz` 和 `/readyz` 是运维端点，不版本化。`/metrics` 同样是运维端点：不版本化，不对公网开放，由这层反向代理不转发。

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

`expires_at`、`disabled_at`、`banned_at` 和 `ban_reason` 为空时省略。`clicks` 是终身累计，`clicks_today` 是 UTC 今天的点击数。

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
| `sort` | `id`（默认）、`clicks`（历史累计）、`clicks_today`（今日） |
| `cursor` | 上一页响应里的 `next_cursor`，原样传回；不透明 |

列表固定降序，包含已过期、已停用和已封禁的链接，不提供状态过滤；客户端根据 `status` 判断。列表只有当前租户的行。

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

## OIDC 登录

只有在 `OIDC_ISSUER`、`OIDC_CLIENT_ID`、`OIDC_CLIENT_SECRET`、`OIDC_REDIRECT_URL` 四个环境变量全部配置时，console 才注册登录和回调路由；全部为空时这两个路径返回 404，不影响纯 API Key 部署；只配一部分时启动失败。两个端点都不走 API Key 鉴权和按租户限流，响应一律带 `Cache-Control: no-store`，并且只接受 `GET`（`HEAD` 返回 405，避免探针消费授权码）。

发起登录：

```bash
curl -i -c cookies.txt localhost:8081/api/v1/auth/login
```

返回 `302 Found`，`Location` 指向身份提供方的授权端点（带 `response_type=code`、`scope=openid email profile`、随机 `state`、PKCE `S256` 的 `code_challenge` 和随机 `nonce`），同时写入短期流程 Cookie `sl_oidc_flow`：

- `HttpOnly`、`SameSite=Lax`、`Path=/api/v1/auth/callback`、`Max-Age=600`（10 分钟）。
- `OIDC_REDIRECT_URL` 为 `https://` 时带 `Secure`；本地 HTTP 开发和冒烟（`http://127.0.0.1:...`）不带 `Secure`。
- 值是 HMAC 签过的，客户端不能改 `state`、PKCE verifier、nonce 或过期时间。

回调建会话：

```http
GET /api/v1/auth/callback?code=...&state=...
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

## 当前用户与登出

配置完整 `OIDC_*` 后，console 同时注册 `GET /api/v1/me` 和 `POST /api/v1/auth/logout`（未配置时返回 404）。两个端点只认唯一的 `sl_session` Cookie，不走 `X-Org-ID` 校验和按租户限流，响应一律带 `Cache-Control: no-store`。同名 Cookie 多于一份时返回 401，不查询、不吊销。

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
      "role": "owner"
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

配置完整 `OIDC_*` 后，console 同时注册以下成员管理接口（未配置时返回 404）。三个接口都走管理 API 鉴权链（要求 `sl_session` + `X-Org-ID`、受按租户限流和 `CrossOriginProtection` 保护，响应带 `Cache-Control: no-store`）。API Key 永远不包含 `members:read` / `members:write` scope，携带有效 API Key 访问这三个接口固定返回 403 `{"error":"forbidden"}`。

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

客户端 IP 默认取连接地址。只有 `TRUST_PROXY_HEADERS=true` 时才看 `X-Forwarded-For` 的最右一段。代理如果追加而不是覆盖，最左一段是客户端带来的，可以伪造；最右一段不是合法 IP 时改回连接地址，不往左找。`deploy/nginx.conf` 把这个头覆盖成代理看到的对端地址。这个开关只能开在可信反向代理后面。`VISITOR_SALT` 由 redirector 启动时读取，为空则拒绝启动。

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
- `shortlink_cache_gets_total`（`result` 为 `hit`、`miss`、`fallback`。负缓存记成 `hit`）
- `shortlink_cache_writes_total`（`result` 为 `success`、`skipped`、`error`。创建、修改、删除和回源写缓存都会计数；`skipped` 表示回源的 `SET NX` 发现 key 已有更新的值，可能是并发的创建或管理写。封禁和解封的缓存写入发生在 `console link` 命令里，不进这个进程的 `/metrics`）
- `shortlink_cache_operation_duration_seconds`（缓存操作耗时，`operation` 为 `get`、`set`，`result` 取值同上面两个计数器）
- `shortlink_store_operations_total`、`shortlink_store_operation_duration_seconds`（`operation` 为 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`、`ban`、`unban`；`result` 为 `success`、`not_found`、`quota_exceeded`、`banned`、`canceled`、`timeout`、`error`。租户修改或删除被封禁的链接是策略生效，记成 `banned`，不算存储失败。管理写在写完正缓存后会按跳转查询复查一次，所以 console 的存储指标里还会多出 `operation="get"`。`console link` 不经过这个装饰器，`ban`、`unban` 不会出现在进程的 `/metrics` 里）
- `go_sql_*`（连接池，`db_name="shortlink"`）
- `process_*`（进程 CPU、内存、文件描述符）
- `shortlink_clicks_dropped_total`（缓冲满、没能记下的点击。启动后就是 0，没有 label；只有 redirector 有）
- `shortlink_clicks_written_total`（交给存储的点击数，`result` 为 `success` 或 `error`。启动后两个序列都是 0。写失败时 dropped 仍是 0，要看 `error`；只有 redirector 有）

它是运维端点，不版本化，公网隔离由部署层保证。

## 剖析

redirector 设了 `PPROF_ADDR` 时，在那个地址上另起一个监听提供 `/debug/pprof/`（Go 的 `net/http/pprof`）。
默认不开，只接受回环地址；业务端口上没有这组路径。用法见 [runbook](runbook.md)。

## 请求编号

经过路由的响应都带 `X-Request-ID`。调用方可以沿用自己的编号，但只接受长度不超过 128、由字母、数字、`.`、`_`、`-` 组成的值；其他值由服务端改成 32 位十六进制编号。同一次请求的访问日志和错误日志都有同一个 `request_id`。

## 版本策略

- `/api/v1` 出现不兼容改动时新增 `/api/v2`，v1 按计划保留。
- `/r/{code}` 不随 API 版本变化；已经发出去的短链必须一直可用。
- `/healthz`、`/readyz`、`/metrics` 等运维端点不进版本前缀。`/metrics` 只给抓取端，应用不做鉴权，公网隔离由部署层保证。
- 短码大小写敏感。
