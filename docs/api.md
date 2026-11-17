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
| console | `POST` | `/api/v1/links` | 创建短链 |
| console | `GET` | `/api/v1/links` | 当前租户的链接列表，支持排序和游标分页 |
| console | `GET` | `/api/v1/links/{code}` | 单条详情，包含已过期链接 |
| console | `PATCH` | `/api/v1/links/{code}` | 修改 `url` 和/或 `expires_at` |
| console | `DELETE` | `/api/v1/links/{code}` | 硬删除，返回 204 |
| console | `GET` | `/api/v1/links/{code}/stats` | 点击统计：日趋势、来源、设备、客户端、访客 |

本地开发时 redirector 监听 `:8080`，console 监听 `:8081`；部署时由反向代理按路径前缀转发到两个进程。宿主机进程的入口是 `deploy/nginx.conf`，容器栈的入口是 `deploy/nginx.stack.conf`。两份都只转发 `/r/` 和 `/api/`，不转发 `/metrics`、`/healthz` 和 `/readyz`。创建响应的 `Location` 是相对路径 `/r/{code}`，调用方应拼公开域名，不是 console 的监听地址。管理 API 带版本前缀 `/api/v1`，并且要带 API Key。跳转路径 `/r/{code}` 不加版本，也不鉴权，因为短链是已经发出去的公开地址。`/healthz` 和 `/readyz` 是运维端点，不版本化。`/metrics` 同样是运维端点：不版本化，不对公网开放，由这层反向代理不转发。

管理 API 的租户只来自 `Authorization` 头里的密钥。链接的 `tenant_id` 由服务端写入，请求体、查询参数和路径里都不能传。其他租户的链接和不存在的链接一律返回 404，不返回 403，也不透露链接是否存在。每个管理请求先鉴权、再按租户限流；超限返回 429 和 `Retry-After`。链接总数还有每租户配额，满了返回 403。两者都由租户配置决定，查看和调整见 [runbook](runbook.md)。公开跳转、探针、指标和未鉴权的 401 流量不经过限流。

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

{"code":"AbC123xy","url":"https://example.com/path?q=1","created_at":"2026-11-15T10:00:00Z","clicks":0,"clicks_today":0}
```

规则：

- 请求体最多约 12KB。
- `url` 必填，最多 2048 字节。
- 只允许 `http` / `https` 绝对 URL。
- URL 内部不能有空白；空格需要百分号编码。
- 请求体必须恰好是一个 JSON 值，尾部多余内容返回 400。
- `expires_at` 可选，用 RFC3339 时间，例如 `"2026-12-01T00:00:00Z"`。省略或 `null` 表示永不过期。必须是将来的时间，到期时刻本身算过期。格式不对时错误信息是 `invalid expires_at`。过期后短码仍被占着，跳转返回 404。响应里带上同一个时刻；没有设置时响应不包含这个字段。
- 未知字段返回 400。`expires_at` 拼错时如果被静默忽略，会创建一条永不过期的链接。
- 响应新增 `created_at`、`clicks` 和 `clicks_today`；新链接的计数都是 0。这是兼容扩展，老客户端可以忽略。

配额和限流：

- 租户的链接总数有上限，已过期但没删除的链接也占配额，删除链接会释放一个名额。满了返回 403：

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

- 限制是每租户的配置，`rate_limit_per_second` 为 0 表示不限流，`link_quota` 为 0 表示不能再创建。console 多副本时每个副本各自限流，实际总量是配置值乘以副本数。
- 403 和 429 都是租户策略，不是存储故障；重试前先删链接或等 `Retry-After`。

## 管理接口的链接表示

详情、修改和列表项使用同一个对象：

```json
{
  "code": "AbC123xy",
  "url": "https://example.com/path?q=1",
  "expires_at": "2026-12-01T00:00:00Z",
  "created_at": "2026-11-15T10:00:00Z",
  "clicks": 42,
  "clicks_today": 3
}
```

`expires_at` 为空时省略。`clicks` 是终身累计，`clicks_today` 是 UTC 今天的点击数。

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

列表固定降序，包含已过期链接，不提供状态过滤；客户端根据 `expires_at` 判断。列表只有当前租户的行。

```json
{
  "items": [
    {"code":"AbC123xy","url":"https://example.com/path","created_at":"2026-11-15T10:00:00Z","clicks":42,"clicks_today":3}
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

返回 200 和链接对象。过期链接在管理视角仍然可见；不存在、已删除或其他租户的链接返回 404。

## 修改

```bash
curl -i -X PATCH localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...' \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/new","expires_at":null}'
```

只处理 `url` 和 `expires_at`：

- `url` 出现时按创建规则校验。
- `expires_at` 出现且为字符串时必须是将来时间。
- `expires_at` 出现且为 `null` 时清除过期时间，改为永不过期。
- 字段没出现时保持原值。
- 空对象、两个字段都没出现、未知字段、尾部多余内容都返回 400。
- 不能改 `code`、`tenant_id`、`created_at`、`clicks`、`clicks_today`。

成功返回 200 和修改后的链接对象。修改 URL 不改变过期状态；把 `expires_at` 改到将来可以让过期链接重新生效。修改采用最后提交者生效，不做 `If-Match` 或版本号。

修改提交后 console 会覆盖 Redis 缓存，之后开始的跳转看到新 URL。已经发出、正在处理的跳转仍可能按旧 URL 返回一次 302。

## 删除

```bash
curl -i -X DELETE localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...'
```

成功返回 204，无响应体。硬删除会同时删除链接和它的统计行，删除后不能恢复。不存在或其他租户的链接返回 404。

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

## 跳转

```bash
curl -i localhost:8080/r/AbC123xy
```

成功返回 302，`Location` 是原始 URL。只有 GET 记一次点击。`GET /r/{code}` 也匹配 HEAD（探针、预览、扫描器），HEAD 同样返回 302，但不记点击。查出链接时客户端已经断开的，不记点击。短码不存在或已过期返回 404，存储错误返回 500，处理超时返回 504；回源并发已满返回 503（临时过载，可重试），包括普通缓存未命中超过 `CACHE_MISS_CONCURRENCY` 和 Redis 不可用时的回源。错误体都是 JSON。

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
- `shortlink_cache_writes_total`（`result` 为 `success`、`skipped`、`error`。创建、修改、删除和回源写缓存都会计数；`skipped` 表示回源的 `SET NX` 发现 key 已有更新的值，可能是并发的创建或管理写）
- `shortlink_cache_operation_duration_seconds`（缓存操作耗时，`operation` 为 `get`、`set`，`result` 取值同上面两个计数器）
- `shortlink_store_operations_total`、`shortlink_store_operation_duration_seconds`（`operation` 为 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`；`result` 为 `success`、`not_found`、`quota_exceeded`、`canceled`、`timeout`、`error`）
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
