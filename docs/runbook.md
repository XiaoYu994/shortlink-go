# 本地运行手册

## 前置条件

- 本机 Docker 中有一个可用的 PostgreSQL 和 Redis，或者用项目自带的 `docker-compose.yml` 启动。
- 项目根目录有 `.env`，至少包含：

```txt
DATABASE_URL=postgres://user:password@127.0.0.1:5432/dbname?sslmode=disable
REDIS_URL=redis://localhost:6379/0
```

`REDIS_URL` 支持密码、TLS（`rediss://`）和 DB 编号；没有密码也可以，直接写 `redis://localhost:6379/0`。

`.env` 已被 gitignore，不会提交。

- 数据库里已经执行过 `migrations/001_create_links.sql` 和 `002_add_links_constraints.sql`，存在 `links` 表。

如果使用项目自带的 `docker-compose.yml`：

```bash
cp .env.example .env
make up
make run
```

如果本机已经有 PostgreSQL，可以把 `DATABASE_URL` 指向它，并执行一次迁移：

```bash
set -a; . ./.env; set +a
psql "$DATABASE_URL" -f migrations/001_create_links.sql
psql "$DATABASE_URL" -f migrations/002_add_links_constraints.sql
make run
```

这种情况下不要再执行 `make up`：它会尝试启动 compose 的 PostgreSQL 并占用 5432 端口。Redis 仍然要在 `REDIS_URL` 上可连，启动时 Ping 失败进程会退出。

## 启动

```bash
make run
```

看到 `connected to postgres`、`connected to redis` 和 `listening addr=:8080` 表示启动成功。`make run` 会自动加载 `.env`。

检查：

```bash
curl -i localhost:8080/healthz
```

日志级别由 `LOG_LEVEL` 控制，默认 `info`；可选 `debug`、`warn`、`error`。

## 指标

```bash
curl -s -o /dev/null localhost:8080/healthz
curl -s localhost:8080/metrics | grep -E '^(go_goroutines|shortlink_http_|shortlink_store_|shortlink_cache_|go_sql_)'
```

计数发生在响应写完之后，没出现过的 label 也不会输出。刚启动后直接抓 `/metrics`，正文里还没有 `shortlink_http_*`，所以上面先访问一次 `/healthz`。

目前能看到：

- `go_goroutines` 等 Go 运行时指标，一启动就有
- `go_sql_*`：连接池使用中连接数、等待时长等（`db_name="shortlink"`），一启动就有
- `shortlink_http_*`：HTTP 请求数和耗时。`method` 只有 `GET`、`HEAD`、`POST`、`other`
- `shortlink_store_*`：存储操作的耗时和结果（`success`、`not_found`、`canceled`、`timeout`、`error`）。`/healthz` 不访问数据库，要创建或打开过短链才看得到
- `shortlink_cache_gets_total`：缓存查找结果（`hit`、`miss`、`fallback`）。`hit` 含负缓存。要发生过跳转才出现
- `shortlink_cache_writes_total`：写缓存结果（`success`、`error`）。创建和回源写缓存都会计数

本地 Prometheus 见下一节。

## 缓存

- 跳转查询先走 Redis：key 是 `shortlink:link:<code>`，value 是完整 `Link` 的 JSON。
- 正缓存 TTL 1 小时；查不到的短码写 JSON `null` 做负缓存，TTL 1 分钟。
- 创建成功后写穿正缓存；负缓存用 `SET NX`，避免和创建并发时后到的负缓存覆盖正缓存。
- 并发未命中用 singleflight 合并回源；Redis 读写失败时直接回源 PostgreSQL，请求不受影响。启动时 Ping 失败进程会退出，属于部署配置错误。
- 查看某个短码的缓存：

```bash
redis-cli -h localhost -p 6379 GET shortlink:link:AbC123xy
redis-cli -h localhost -p 6379 TTL shortlink:link:AbC123xy
```

设计背景见 [ADR 0006](adr/0006-redis-cache-design.md)。

## Prometheus

```bash
make prometheus
```

Prometheus UI：http://localhost:9090。抓取目标是 `host.docker.internal:8080/metrics`，所以 app 要用 `make run` 跑在宿主机。如果本机已经有 PostgreSQL 占用 5432，不要用 `make up` 启动 compose 的 PostgreSQL；用 `make prometheus` 只启动 Prometheus。

## 基线压测（无缓存）

日期：2026-11-12。目标：在加 Redis 之前记录跳转延迟，缓存做好后用同样的机器、命令和窗口对比。

### 压测环境

| 项目 | 配置 |
| --- | --- |
| CPU | Intel Core i5-14400；WSL2 内 `nproc=16`（`lscpu`：8 cores/socket，2 threads/core） |
| 内存 | 7.6 GiB（压测前约 5.4 GiB 已用，2.2 GiB available） |
| OS | Ubuntu 26.04.1 LTS，WSL2，内核 6.18.58.1-microsoft-standard-WSL2 |
| Go | go1.27.3 linux/amd64，`GOMAXPROCS=16` |
| Docker | 29.9.3，Compose 5.7.0，overlayfs |
| PostgreSQL | 16.15（Docker），`max_connections=100`，`shared_buffers=128MB`，`synchronous_commit=on`，容器未设 CPU/内存限制 |
| Prometheus | 2.55.1（Docker），`scrape_interval=5s` |
| 压测工具 | hey v0.1.5，`-disable-redirects` |
| 应用配置 | `DB_MAX_OPEN_CONNS=10`、`DB_MAX_IDLE_CONNS=5`、`REQUEST_TIMEOUT=5s`，Info 级别访问日志写文件 |
| 拓扑 | app、PostgreSQL、Prometheus、hey 都在同一台 WSL2 VM 上，共享 16 vCPU / 7.6 GiB |

### 压测方法

如果还没装 hey：`go install github.com/rakyll/hey@latest`；不在 PATH 就 `export PATH="$(go env GOPATH)/bin:$PATH"`。

1. 启动 app 和 Prometheus；Prometheus 每 5s 抓一次 `/metrics`。
2. 通过 `POST /api/v1/links` 创建一条短链，URL 指向 `https://example.com`。压测只读同一个短码，命中 PostgreSQL 页缓存。
3. 预热 10 秒：

   ```bash
   hey -disable-redirects -z 10s -c 50 http://localhost:8080/r/<code>
   ```

4. 测量 30 秒，记录 hey 的延迟、RPS 和状态码：

   ```bash
   hey -disable-redirects -z 30s -c 50 http://localhost:8080/r/<code>
   ```

5. 测量结束后用 Prometheus 的 30 秒窗口查询服务端 p50/p99、RPS 和连接池：

   ```promql
   histogram_quantile(0.50, sum by (le) (rate(shortlink_http_request_duration_seconds_bucket{route="GET /r/{code}",method="GET",status="302"}[30s])))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_http_request_duration_seconds_bucket{route="GET /r/{code}",method="GET",status="302"}[30s])))
   histogram_quantile(0.50, sum by (le) (rate(shortlink_store_operation_duration_seconds_bucket{operation="get",result="success"}[30s])))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_store_operation_duration_seconds_bucket{operation="get",result="success"}[30s])))
   sum(rate(shortlink_http_requests_total{route="GET /r/{code}",method="GET",status="302"}[30s]))
   max_over_time(go_sql_in_use_connections{db_name="shortlink"}[30s])
   rate(go_sql_wait_duration_seconds_total{db_name="shortlink"}[30s])
   sum(rate(go_sql_wait_count_total{db_name="shortlink"}[30s]))
   ```

6. 单轮预热加单轮测量。数字用于同一台机器上的前后对比，不作为容量规划依据。

### 结果

hey（客户端，30 秒）：303,198 次请求，全部 302，10,105 req/s，p50 3.9ms，p99 17.1ms。

Prometheus（服务端，30 秒窗口）：

| 指标 | p50 | p99 |
| --- | --- | --- |
| HTTP 跳转 | 3.2 ms | 21.5 ms |
| 存储 GetByCode（`result="success"`） | 3.2 ms | 21.4 ms |

连接池：`go_sql_in_use_connections` 峰值 10（等于 `DB_MAX_OPEN_CONNS`）；`go_sql_wait_count_total` 约 9.9k 次/s，`go_sql_wait_duration_seconds_total` 约 32.2 s/s。50 并发下几乎每个请求都在等连接，连接池是主要瓶颈。

### 读数注意

- app、PostgreSQL、Prometheus 和 hey 在同一台 WSL2 VM 上，互相争 CPU 和内存；绝对数字不能直接搬到其他机器。
- 访问日志每个请求一条，Prometheus 每 5s 抓一次；这些开销包含在结果里。
- 同一个短码反复读取，数据库页缓存是热的；测的是热 key 读延迟，不是冷启动或随机磁盘 I/O。
- 客户端 p50/p99 来自 hey；服务端 p50/p99 来自直方图插值，桶从 0.1ms 起步。
- 缓存做好后用同样的机器、预热、命令和查询窗口再跑一轮对比。

## 创建一条短链

```bash
curl -i -X POST localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

响应示例：

```http
HTTP/1.1 201 Created
Location: /r/AbC123xy

{"code":"AbC123xy","url":"https://example.com"}
```

访问短链：

```bash
curl -i localhost:8080/r/AbC123xy
```

浏览器打开 `http://localhost:8080/r/AbC123xy` 也可以。

## 查看数据库

如果使用项目自带的 compose：

```bash
make psql
```

如果使用自己的 PostgreSQL：

```bash
set -a; . ./.env; set +a
psql "$DATABASE_URL" -c 'SELECT id, code, url, created_at FROM links ORDER BY id DESC LIMIT 5'
```

## 测试

```bash
make test
make lint
```

PostgreSQL 集成测试只读 `TEST_DATABASE_URL`，未设置时自动跳过；库名必须以 `_test` 结尾，防止误清开发库。应用的 `DATABASE_URL` 不会被测试使用。

首次准备测试库（使用项目自带的 compose）：

```bash
docker compose exec postgres psql -U shortlink -d shortlink -c 'CREATE DATABASE shortlink_test'
docker compose exec -i postgres psql -U shortlink -d shortlink_test < migrations/001_create_links.sql
docker compose exec -i postgres psql -U shortlink -d shortlink_test < migrations/002_add_links_constraints.sql
```

如果使用自己的 PostgreSQL，建一个以 `_test` 结尾的库，并用你的 psql 客户端执行同样两个迁移文件。

然后在 `.env` 里加：

```txt
TEST_DATABASE_URL=postgres://shortlink:shortlink@localhost:5432/shortlink_test?sslmode=disable
```

使用自己的 PostgreSQL 时，把用户名、密码和库名换成实际的。

Redis 缓存测试只读 `TEST_REDIS_URL`，未设置时自动跳过。测试用 `test-` 前缀的唯一短码，结束后只删除自己写入的 key，不会清空 Redis。在 `.env` 里加：

```txt
TEST_REDIS_URL=redis://localhost:6379/0
```

## 停止

前台 `make run` 用 `Ctrl+C` 停止：进程不再接受新请求，并等待正在处理的请求结束，最多 `SHUTDOWN_TIMEOUT`（默认 10s）。再按一次 `Ctrl+C` 会立刻退出。`make down` 只停止项目自带的 compose 服务。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| `DATABASE_URL is required` | `.env` 缺失，或没有通过 `make run` 启动 |
| `connect postgres` 失败 | 检查容器是否运行、端口、用户名密码 |
| `connect redis` 失败 | 检查 Redis 是否运行、`REDIS_URL` 是否正确；启动时 Ping 失败进程会退出 |
| `REDIS_URL is required` | `.env` 里没有配置 Redis 连接 |
| cache 测试被跳过 | `.env` 里没有 `TEST_REDIS_URL`；`make test` 会自动加载 `.env` |
| `relation "links" does not exist` | 执行 `migrations/001_create_links.sql` 和 `002_add_links_constraints.sql` |
| `make up` 报端口占用 | 本机已有 PostgreSQL 占用 5432；不要启动 compose 的 PostgreSQL，把 `DATABASE_URL` 指向已有实例 |
| 测试清空了链接 | 确认 `TEST_DATABASE_URL` 指向 `_test` 库；应用的 `DATABASE_URL` 不会被测试使用 |
