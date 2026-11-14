# HTTP API

## 路由

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/healthz` | 健康检查，返回 `ok` |
| `GET` | `/metrics` | Prometheus 指标，运维端点 |
| `POST` | `/api/v1/links` | 创建短链，需要 API Key |
| `GET` | `/r/{code}` | 302 跳转到原始 URL |

管理 API 带版本前缀 `/api/v1`，并且要带 API Key。跳转路径 `/r/{code}` 不加版本，也不鉴权，因为短链是已经发出去的公开地址。`/healthz` 是运维端点，不版本化。阶段 5 的 `/metrics` 同样是运维端点：不版本化，不对公网开放（由部署层的反向代理或网络策略限制）。

## 创建短链

```bash
curl -i -X POST localhost:8080/api/v1/links \
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

{"code":"AbC123xy","url":"https://example.com/path?q=1"}
```

规则：

- 请求体最多约 12KB。
- `url` 必填，最多 2048 字节。
- 只允许 `http` / `https` 绝对 URL。
- URL 内部不能有空白；空格需要百分号编码。
- 请求体必须恰好是一个 JSON 值，尾部多余内容返回 400。
- `expires_at` 可选，用 RFC3339 时间，例如 `"2026-12-01T00:00:00Z"`。省略或 `null` 表示永不过期。必须是将来的时间，到期时刻本身算过期。格式不对时错误信息是 `invalid expires_at`。过期后短码仍被占着，跳转返回 404。响应里带上同一个时刻；没有设置时响应不包含这个字段。
- 未知字段返回 400。`expires_at` 拼错时如果被静默忽略，会创建一条永不过期的链接。

错误格式：

```json
{"error":"invalid JSON body"}
```

| 状态码 | 场景 |
| --- | --- |
| 400 | JSON 不合法、有未知字段、URL 或 `expires_at` 不合法 |
| 401 | 没有 `Authorization: Bearer`、密钥格式不对、密钥不存在、已过期或已吊销 |
| 413 | 请求体超过上限（约 12KB） |
| 500 | 存储或内部错误 |
| 504 | 请求处理超过 `REQUEST_TIMEOUT`（默认 5s） |

401 的正文是 `{"error":"unauthorized"}`，并带 `WWW-Authenticate: Bearer`。格式错误、未知密钥、过期和吊销用同一句，不透露密钥是否存在过。

单个请求的处理时限由 `REQUEST_TIMEOUT` 控制。到点后数据库查询被取消，服务端返回 504，不会让请求一直挂着占住连接。

## 跳转

```bash
curl -i localhost:8080/r/AbC123xy
```

成功返回 302，`Location` 是原始 URL。只有 GET 记一次点击。`GET /r/{code}` 也匹配 HEAD（探针、预览、扫描器），HEAD 同样返回 302，但不记点击。查出链接时客户端已经断开的，不记点击。短码不存在或已过期返回 404，存储错误返回 500，处理超时返回 504；回源并发已满返回 503（临时过载，可重试），包括普通缓存未命中超过 `CACHE_MISS_CONCURRENCY` 和 Redis 不可用时的回源。错误体都是 JSON。

## 健康检查

```bash
curl -i localhost:8080/healthz
```

返回 200 和 `ok`。它是 liveness 探针，故意不访问数据库。

## 指标

```bash
curl -i localhost:8080/metrics
```

返回 Prometheus 文本格式。目前暴露 Go 运行时、进程、HTTP、缓存、存储、连接池和点击指标：

- `shortlink_http_requests_total`、`shortlink_http_request_duration_seconds`（`method` 只有 `GET`、`HEAD`、`POST`、`other`）
- `shortlink_cache_gets_total`（`result` 为 `hit`、`miss`、`fallback`。负缓存记成 `hit`）
- `shortlink_cache_writes_total`（`result` 为 `success`、`skipped`、`error`。创建和回源写缓存都会计数；`skipped` 表示负缓存的 `SET NX` 发现 key 已被并发的创建写好）
- `shortlink_cache_operation_duration_seconds`（缓存操作耗时，`operation` 为 `get`、`set`，`result` 取值同上面两个计数器）
- `shortlink_store_operations_total`、`shortlink_store_operation_duration_seconds`（`result` 为 `success`、`not_found`、`canceled`、`timeout`、`error`）
- `go_sql_*`（连接池，`db_name="shortlink"`）
- `process_*`（进程 CPU、内存、文件描述符）
- `shortlink_clicks_dropped_total`（缓冲满、没能记下的点击。启动后就是 0，没有 label）
- `shortlink_clicks_written_total`（交给存储的点击数，`result` 为 `success` 或 `error`。启动后两个序列都是 0。写失败时 dropped 仍是 0，要看 `error`）

它是运维端点，不版本化，公网隔离由部署层保证。

## 请求编号

经过路由的响应都带 `X-Request-ID`。调用方可以沿用自己的编号，但只接受长度不超过 128、由字母、数字、`.`、`_`、`-` 组成的值；其他值由服务端改成 32 位十六进制编号。同一次请求的访问日志和错误日志都有同一个 `request_id`。

## 版本策略

- `/api/v1` 出现不兼容改动时新增 `/api/v2`，v1 按计划保留。
- `/r/{code}` 不随 API 版本变化；已经发出去的短链必须一直可用。
- `/healthz`、`/metrics` 等运维端点不进版本前缀。`/metrics` 只给抓取端，应用不做鉴权，公网隔离由部署层保证。
- 短码大小写敏感。
