# 本地运行手册

## 前置条件

- 本机 Docker 中有一个可用的 PostgreSQL 和 Redis，或者用项目自带的 `docker-compose.yml` 启动。
- 项目根目录有 `.env`，至少包含：

```txt
DATABASE_URL=postgres://user:password@127.0.0.1:5432/dbname?sslmode=disable
REDIS_URL=redis://localhost:6379/0
VISITOR_SALT=dev-only-visitor-salt
```

`REDIS_URL` 支持密码、TLS（`rediss://`）和 DB 编号；没有密码也可以，直接写 `redis://localhost:6379/0`。`VISITOR_SALT` 是访客哈希的密钥，redirector 启动时必填；开发用固定占位值即可，生产从密钥管理注入，不要提交真实值。`TRUST_PROXY_HEADERS` 默认 `false`，只有部署在可信反向代理后面才设为 `true`。

`.env` 已被 gitignore，不会提交。

- 在仓库根目录执行过 `make migrate`。它读取 `DATABASE_URL`，把 `migrations/` 里的 up SQL 应用到最新，版本记在 `schema_migrations`。库里会有 `links`、`tenants`、`api_keys`、`link_clicks_daily`、`link_dimension_daily` 和 `link_visitors_daily`。`tenant_id` 没有数据库外键。HTTP 进程启动时不迁移。

如果使用项目自带的 `docker-compose.yml`：

```bash
cp .env.example .env
make up
make migrate
```

然后在两个终端分别启动：

```bash
make run-redirector   # 公开跳转，默认 :8080
```

```bash
make run-console      # 管理 API，默认 :8081
```

`make up` 只启动数据库，不执行 SQL。以前用初始化脚本建过表、但没有 `schema_migrations` 的数据卷，`make migrate` 会在 `CREATE TABLE links` 处失败。学习用的 compose 库可以删掉数据卷再来一次（这会清空该卷里的数据）：

```bash
docker compose down -v
make deps-up
make migrate
```

如果本机已经有 PostgreSQL，把 `DATABASE_URL` 指向它，在仓库根目录执行：

```bash
make migrate
```

然后在两个终端分别启动：

```bash
make run-redirector
```

```bash
make run-console
```

`up`、`down` 和 `version` 都读同一个地址，并且只允许库名 `shortlink` 或以 `shortlink_` 开头的库（部署环境用前缀区分，例如 `shortlink_prod`）。查看当前版本：

```bash
go run ./cmd/migrate version
```

`down` 会把业务表一路降到没有，只允许库名以 `_test` 结尾。测试库由 `make test-db` 自己做 up、down、up，平时不用手跑 down。

连接串用 `postgres://` 或 `postgresql://`，库名写在路径里。这个命令用 lib/pq：不写 `sslmode` 时按 `require` 连接，也不接受 `prefer`。`.env.example` 写的是 `sslmode=disable`。

迁移执行到一半失败时，表结构会回滚，但 `schema_migrations` 会留在 dirty。下一次 `up` 会停住。测试库再跑一次 `make test-db` 即可。学习库先确认失败的那一版没有留下表，再把版本改回上一版且 `dirty = false`；还没有任何成功的版本时删掉那一行。不要把失败的那一版标成已完成。

这种情况下不要再执行 `make up` 或 `make deps-up`：它们会尝试启动 compose 的
PostgreSQL 并占用 5432 端口。Redis 仍然要在 `REDIS_URL` 上可连，启动时 Ping 失败进程会退出。

## 启动

两个进程各开一个终端：

```bash
make run-redirector   # 公开跳转，ADDR，默认 :8080
```

```bash
make run-console      # 管理 API，CONSOLE_ADDR，默认 :8081
```

看到 `connected to postgres`、`connected to redis` 和 `listening addr=...` 表示对应进程启动成功。两个 `make` 目标都会加载 `.env`。跳转进程用 `ADDR`，默认 `:8080`；管理进程只用 `CONSOLE_ADDR`，默认 `:8081`。临时换端口可以写在命令上，例如 `make run-console CONSOLE_ADDR=:18081`。redirector 没有 `VISITOR_SALT` 会直接退出，这是为了避免 UV 被静默记成 0。密钥命令 `make console ...` 不启动 HTTP，也不需要 Redis。

检查：

```bash
curl -i localhost:8080/healthz   # redirector
curl -i localhost:8081/healthz   # console
```

日志级别由 `LOG_LEVEL` 控制，默认 `info`；可选 `debug`、`warn`、`error`。

## 指标

```bash
curl -s -o /dev/null localhost:8080/healthz
curl -s localhost:8080/metrics | grep -E '^(go_goroutines|process_|shortlink_http_|shortlink_store_|shortlink_cache_|shortlink_clicks_|go_sql_)'

curl -s -o /dev/null localhost:8081/healthz
curl -s localhost:8081/metrics | grep -E '^(go_goroutines|process_|shortlink_http_|shortlink_store_|shortlink_cache_|go_sql_)'
```

两个进程各暴露一份。计数发生在响应写完之后，没出现过的 label 也不会输出。刚启动后直接抓 `/metrics`，正文里还没有 `shortlink_http_*`，所以上面先访问一次 `/healthz`。

目前能看到：

- `go_goroutines` 等 Go 运行时指标，一启动就有
- `process_*`：进程 CPU、内存、文件描述符，一启动就有
- `go_sql_*`：连接池使用中连接数、等待时长等（`db_name="shortlink"`），每个进程各一份，一启动就有
- `shortlink_http_*`：HTTP 请求数和耗时。`method` 为 `GET`、`HEAD`、`POST`、`PATCH`、`DELETE`，其余记成 `other`
- `shortlink_store_*`：存储操作的耗时和结果（`operation` 为 `create`、`get`、`list`、`get_tenant`、`update`、`delete`、`stats`；`result` 为 `success`、`not_found`、`canceled`、`timeout`、`error`）。redirector 要回源过才出现，console 调过管理接口后出现对应操作
- `shortlink_cache_gets_total`：缓存查找结果（`hit`、`miss`、`fallback`）。`hit` 含负缓存。只有 redirector 读取，发生过跳转才出现
- `shortlink_cache_shed_total`：回源并发已满被拒绝的请求数，`path` 为 `miss` 或 `fallback`。只有 redirector 有；按请求计数，singleflight 的等待者也各算一次；出现就表示回源上限正在生效
- `shortlink_cache_writes_total`：写缓存结果（`success`、`skipped`、`error`）。两个进程都可能写：console 在创建、修改、删除后维护缓存，redirector 回源时写。`skipped` 表示回源的 `SET NX` 没写进去（key 已有更新的值，可能是并发的创建或管理写）
- `shortlink_cache_operation_duration_seconds`：缓存操作耗时，`operation` 为 `get`、`set`，用来区分慢在 Redis 还是数据库
- `shortlink_clicks_dropped_total`：点击缓冲满了没记下的次数。只有 redirector 有，一启动就是 0
- `shortlink_clicks_written_total`：交给存储的点击数，`result` 为 `success` 或 `error`。只有 redirector 有，一启动两个序列都是 0。写失败时 dropped 仍是 0，要看 `error`

Prometheus 每 5 秒抓取一次 `/metrics`，抓取请求不计入访问日志和 HTTP 指标。

本地 Prometheus 见下一节。

## 缓存

- 跳转查询先走 Redis：key 是 `shortlink:link:<code>`，value 是完整 `Link` 的 JSON。
- 正缓存 TTL 取剩余寿命和 1 小时里更短的那个；没有到期时间时是 1 小时。查不到的短码写 JSON `null` 做负缓存，TTL 1 分钟。
- 回源写缓存（正、负）都用 `SET NX`，只有 key 不存在时才写。console 在创建、修改、删除提交后无条件覆盖缓存；删除写负缓存，修改后的链接已过期也写负缓存。这样在途旧回源不会盖掉管理写的新值。
- 并发未命中用 singleflight 合并回源。普通未命中和 Redis 读失败（fallback）各有一条回源限流：许可数分别是 `CACHE_MISS_CONCURRENCY`（默认 8）和固定 8，拿不到许可立即返回 503。单次回源超时由 `CACHE_FETCH_TIMEOUT`（默认 1s）控制，独立于 `REQUEST_TIMEOUT`。启动时 Ping 失败进程会退出，属于部署配置错误。
- 修改或删除后 Redis 写失败只影响一致性：旧值可能保留到 TTL，下一次写路径会自愈。删除不是强撤销，已经在途的跳转仍可能返回 302。
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

Prometheus UI：http://localhost:9090。抓取目标是宿主机上两个进程的 `/metrics`，端口来自 `ADDR` 和 `CONSOLE_ADDR`（默认 `8080` 和 `8081`，job 分别是 `redirector` 和 `console`）。两个进程要用 `make run-redirector` / `make run-console` 跑在宿主机。改了端口要重新执行 `make prometheus`，容器才会换上新的抓取配置。两个 job 的指标名相同，PromQL 里用 `job` 区分。如果本机已经有 PostgreSQL 占用 5432，不要用 `make up` 或 `make deps-up` 启动 compose 的 PostgreSQL；用 `make prometheus` 只启动 Prometheus。

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

5. 测量结束后用 Prometheus 的 30 秒窗口查询服务端 p50/p99、RPS、连接池和缓存：

   ```promql
   histogram_quantile(0.50, sum by (le) (rate(shortlink_http_request_duration_seconds_bucket{route="GET /r/{code}",method="GET",status="302"}[30s])))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_http_request_duration_seconds_bucket{route="GET /r/{code}",method="GET",status="302"}[30s])))
   histogram_quantile(0.50, sum by (le) (rate(shortlink_store_operation_duration_seconds_bucket{operation="get",result="success"}[30s])))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_store_operation_duration_seconds_bucket{operation="get",result="success"}[30s])))
   sum(rate(shortlink_http_requests_total{route="GET /r/{code}",method="GET",status="302"}[30s]))
   max_over_time(go_sql_in_use_connections{db_name="shortlink"}[30s])
   rate(go_sql_wait_duration_seconds_total{db_name="shortlink"}[30s])
   sum(rate(go_sql_wait_count_total{db_name="shortlink"}[30s]))
   sum by (result) (rate(shortlink_cache_gets_total[30s]))
   sum by (path) (rate(shortlink_cache_shed_total[30s]))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_cache_operation_duration_seconds_bucket{operation="get",result="hit"}[30s])))
   histogram_quantile(0.99, sum by (le) (rate(shortlink_cache_operation_duration_seconds_bucket{operation="set",result="success"}[30s])))
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

## 缓存压测（2026-11-12）

环境、方法和基线相同：同一台机器、50 并发、10s 预热 + 30s 测量、同一个短码；应用换成带 Redis 缓存的版本。本次短码 `jPsQPUB0`，URL 为 `https://example.com`。

### 结果

hey（客户端，30 秒）：572,365 次请求，全部 302，19,077 req/s，p50 2.5ms，p99 5.7ms。

Prometheus（服务端，30 秒窗口）：

| 指标 | 无缓存基线 | 有缓存 | 变化 |
| --- | --- | --- | --- |
| HTTP 跳转 p50 | 3.2 ms | 1.4 ms | -57% |
| HTTP 跳转 p99 | 21.5 ms | 4.7 ms | -78% |
| 存储 GetByCode p50/p99 | 3.2 / 21.4 ms | 无样本（全部命中缓存） | — |
| 客户端 RPS | 10,105 | 19,077 | +89% |
| 客户端 p50/p99 | 3.9 / 17.1 ms | 2.5 / 5.7 ms | -36% / -67% |
| `go_sql_in_use_connections` 峰值 | 10 | 0 | — |
| `go_sql_wait_count_total` | 约 9.9k 次/s | 0 | — |
| `go_sql_wait_duration_seconds_total` | 约 32.2 s/s | 0 | — |

缓存指标：`shortlink_cache_gets_total{result="hit"}` 约 17.6k 次/s，`miss` 和 `fallback` 为 0；测量窗口内没有写缓存，预热时已经写入。缓存 `get` 命中 p99 4.5ms，和 HTTP p99 接近，说明请求时间基本都花在 Redis 读取上；无缓存时对应的存储 p99 是 21.4ms。

### 读数注意

- 创建短链时写穿就写入了正缓存，预热和测量期间全部命中；累计计数 782,077 次 `hit`、0 次 `miss`、1 次 `success` 写入（来自创建）。
- 只压一个热 key，不能据此推断多 key、缓存过期或 Redis 故障时的表现。
- 缓存命中时 `shortlink_store_*` 没有样本，存储 p50/p99 查出来是空值，这是预期结果。
- 和基线一样，app、Redis、PostgreSQL、Prometheus、hey 共享同一台 WSL2 VM，绝对数字只用于本机前后对比。

## 多 key 压测（2026-11-14）

目标：验证 ADR 0008 的冷缓存保护，用数字确定 `CACHE_MISS_CONCURRENCY` 和
`CACHE_FETCH_TIMEOUT`。之前的压测只压一个热 key，不能说明大量不同短码时的表现。

### 压测环境

| 项目 | 配置 |
| --- | --- |
| CPU | AMD Ryzen 7 5700U，8 核 16 线程 |
| 内存 | 14 GiB（压测前约 6.1 GiB 已用，8.8 GiB available） |
| OS | Linux 6.18.63-2-lts |
| Go | go1.27.4 linux/amd64 |
| Docker | 29.9.3 / Compose 5.7.1 |
| PostgreSQL | 17.12（Docker），`max_connections=100` |
| Redis | 7.4.12（Docker） |
| 压测工具 | vegeta v12.13.0，`-redirects=-1` |
| 应用配置 | `DB_MAX_OPEN_CONNS=10`、`DB_MAX_IDLE_CONNS=5`、`REQUEST_TIMEOUT=5s`、`CACHE_FETCH_TIMEOUT=1s`、`LOG_LEVEL=warn` |
| 隔离 | 独立库 `shortlink_bench` 和 Redis DB 15，压测后删除；主库未写入 |
| 拓扑 | app、PostgreSQL、Redis、vegeta 在同一台机器上，共享 16 vCPU |

### 方法

1. 在独立库造 50 万条短链，短码是 `md5(g)` 的前 8 位；另造 50 万个不与现有短码冲突的随机码，用来测不存在的链接。
2. 用 vegeta 按目标文件发 `HEAD /r/{code}`，`-redirects=-1`。用 HEAD 是为了不触发 `ClickRecorder`，只看读路径；GET 会额外产生点击写入。
3. 三种流量，每轮 20 秒：
   - 全部命中：1 万条热短码，先以 2,000 req/s 预热 1 万次，再以 20,000 req/s 压 20 秒；
   - 全部 miss：清空 Redis，50 万条不同短码按顺序使用，12,000 req/s 压 20 秒，共 24 万次，窗口内不重复；
   - 大量不存在：清空 Redis，50 万个不同的不存在短码，同样 12,000 req/s、20 秒。
4. 每 0.5 秒抓一次 `/metrics`；压测前后各抓一次完整快照，计算计数器增量、直方图分位数和连接池峰值。

### 结果

客户端 vegeta：

| 场景 | 请求数 | 2xx/3xx | 503 | 404 | 客户端成功吞吐 | 客户端 p50 / p99 |
| --- | --- | --- | --- | --- | --- | --- |
| 全部命中 | 400,000 | 400,000（302） | 0 | 0 | 19,999/s | 0.53 / 7.55 ms |
| 全部 miss，并发 8 | 239,999 | 97,221（302） | 142,778 | 0 | 4,859/s | 2.09 / 14.35 ms |
| 大量不存在，并发 8 | 240,001 | 0 | 141,793 | 98,208 | —（404 不计成功） | 2.10 / 14.60 ms |
| 全部 miss，并发 16 | 240,001 | 117,895（302） | 122,106 | 0 | 5,894/s | 3.29 / 19.04 ms |

服务端（`/metrics` 快照，20 秒窗口）：

| 场景 | HTTP p50 / p99 | 存储 p50 / p99 | 连接池使用峰值 | 等待计数 | 等待累计 | goroutine 峰值 |
| --- | --- | --- | --- | --- | --- | --- |
| 全部命中 | 0.33 / 5.15 ms | 无样本 | 0 | 0 | 0 | 305 |
| 全部 miss，并发 8 | 1.46 / 15.21 ms | 0.83 / 18.86 ms | 8 | 0 | 0 | 211 |
| 大量不存在，并发 8 | 1.48 / 15.64 ms | 0.80 / 19.14 ms | 8 | 0 | 0 | 364 |
| 全部 miss，并发 16 | 2.48 / 18.78 ms | 1.57 / 20.73 ms | 10 | 94,007（4,700/s） | 80.39 s | 480 |

缓存指标：miss 场景 `shortlink_cache_gets_total{result="miss"}` 等于请求数，
`result="fallback"` 为 0；`shortlink_cache_shed_total{path="miss"}` 分别为
142,778（并发 8）和 122,106（并发 16）。命中场景 400,000 次全部 `hit`，没有
`miss`、`fallback` 或 `shed`。四轮都没有 504，说明 `CACHE_FETCH_TIMEOUT=1s`
没有误伤正常查询。

### 结论

- **`CACHE_MISS_CONCURRENCY=8` 作为默认值**：连接池使用峰值 8、等待为 0，给创建等写请求留了 2 个连接；过载时 503 是快速拒绝，没有请求排到 5 秒超时。
- `16` 把成功吞吐从 4,859 提到 5,894 req/s，但连接池被顶满 10，20 秒内出现 94,007 次等待、累计 80.39 秒，p99 升到 19ms。这是用数据库排队换吞吐，不符合“数据库不被打穿”的目标；多副本时连接数还会按副本数放大，所以不选 16。
- **`CACHE_FETCH_TIMEOUT=1s` 保持不变**：正常存储 p99 约 20ms，1s 留了两个数量级的余量；四轮都没有 504。等数据库 p99 明显上升时再重新评估。
- 命中场景 20k req/s、服务端 p99 5.15ms、连接池零使用，说明缓存命中路径没有受这次保护影响。

### 读数注意

- app、PostgreSQL、Redis、vegeta 共享同一台机器，绝对数字只用于本机前后对比，不是容量规划依据。
- 使用 HEAD 避免点击写入；GET 会多一条异步点击写路径，数字会更差。
- miss 和不存在场景每轮使用不同的短码，窗口内每个短码只访问一次；同一短码重复访问会命中刚写入的缓存。
- 503 是 bulkhead 主动拒绝，不是故障；客户端应退避重试。当前 503 响应没有 `Retry-After`，后续可以补。

## 点击写入压测（2026-11-15）

目标：日聚合把点击写入从 1 条 UPDATE 变成 2 条 upsert、1 条访客 insert 加 1 条 UPDATE，
确认跳转路径和连接池还能承受，并观察 dropped。

### 环境和方法

| 项目 | 配置 |
| --- | --- |
| 机器 | 本机 Linux，16 vCPU，PostgreSQL/Redis 用项目 compose |
| 应用 | `bin/redirector`、`bin/console`，测试库，Redis DB 15，`LOG_LEVEL=warn` |
| 压测工具 | vegeta，`-redirects=0`，单热 key，`GET /r/{code}` |
| 请求头 | 固定 `Referer: https://news.ycombinator.com/item?id=1` 和 Chrome User-Agent |
| 流量 | 预热 5s x 1000/s，测量 20s x 3000/s |
| 连接池 | `DB_MAX_OPEN_CONNS=10`、`DB_MAX_IDLE_CONNS=5`（默认） |

所有请求都命中 console 创建时写穿的正缓存，所以存储读路径没有流量；点击走
`ClickRecorder` 每 1 秒或满 100 条的异步批量写。

### 结果

vegeta（客户端）：

| 指标 | 数值 |
| --- | --- |
| 请求数 | 60,000（预热 5,000） |
| 客户端 p50 / p99 / max | 0.35 / 0.83 / 2.73 ms |
| 状态码 | 全部 302（`-redirects=0` 时 vegeta 把 302 当作停止点，`Success` 统计为 0 是工具口径） |

redirector `/metrics`（20 秒窗口）：

| 指标 | 数值 |
| --- | --- |
| `shortlink_cache_gets_total{result="hit"}` | 65,000（含预热） |
| `shortlink_clicks_written_total{result="success"}` | 65,000 |
| `shortlink_clicks_written_total{result="error"}` | 0 |
| `shortlink_clicks_dropped_total` | 0 |
| `go_sql_in_use_connections` 峰值 | 1 |
| `go_sql_wait_count_total` | 0 |

### 结论

- 单热 key、3000 req/s 下点击写入没有打满 10 个连接：异步批次只占用约 1 个连接，
  没有等待；dropped 为 0。
- 客户端 p99 在 1ms 内，说明点击元数据构造和入队没有明显拖慢跳转。
- 这是单机、单 redirector 和学习用测试库的数字，只用于本机前后对比，不是容量规划依据。
  真实流量还要看多 key、冷缓存和数据库负载。

## 签发 API Key

`make` 会加载 `.env`，所以下面的命令能读到 `DATABASE_URL`。直接 `go run` 时要先导出环境变量。命令不连接 Redis。

```bash
make console ARGS="key issue --name acme"
make console ARGS="key issue --name acme --expires 2026-12-01T00:00:00Z"
make console ARGS="key issue --tenant 1"
make console ARGS="key list --tenant 1"
make console ARGS="key revoke --id 1"
```

`key issue` 把明文打印到标准输出，只此一次。终端滚动记录里会留下这把密钥。`--expires` 省略表示不过期；到期时刻本身算过期。`key list` 只显示编号和时间，不显示明文或哈希。吊销使用签发时打印的 `key_id`。

已有租户再发一把密钥时用 `--tenant`。`--name` 每次都新建租户，名称不必唯一。`--external-id` 只在新建时写入，并且不能重复。

## 创建一条短链

```bash
curl -i -X POST localhost:8081/api/v1/links \
  -H 'Authorization: Bearer slk_...' \
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

## 管理短链

```bash
# 列表：默认按 id 降序，limit 最大 100
curl -s 'localhost:8081/api/v1/links?limit=20&sort=clicks_today' \
  -H 'Authorization: Bearer slk_...'

# 详情、修改、删除
curl -s localhost:8081/api/v1/links/AbC123xy -H 'Authorization: Bearer slk_...'
curl -i -X PATCH localhost:8081/api/v1/links/AbC123xy \
  -H 'Authorization: Bearer slk_...' -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/new","expires_at":null}'
curl -i -X DELETE localhost:8081/api/v1/links/AbC123xy -H 'Authorization: Bearer slk_...'
```

修改和删除之后 console 会维护 Redis 缓存；跳转进程不需要重启。列表和详情包含已过期链接，跳转仍然只认有效链接。游标是响应里的 `next_cursor`，原样传回即可。

## 点击统计

访问几次短链（带 `Referer` 和 `User-Agent`），等 1 秒让 `ClickRecorder` 落库，再查统计：

```bash
curl -s -H 'Referer: https://news.ycombinator.com/' \
  -A 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0' \
  localhost:8080/r/AbC123xy

sleep 2
curl -s localhost:8081/api/v1/links/AbC123xy/stats \
  -H 'Authorization: Bearer slk_...'
```

默认查最近 8 个 UTC 日（`to` 为今天，`from` 为 `to - 7 天`）。可以用 `?from=...&to=...` 指定 RFC3339 时间，跨度不超过 90 天。`total_clicks` 是终身累计，`clicks` 是日趋势；`devices` 和 `clients` 固定返回全部类别，没有数据的为 0。`unique_visitors` 是每天 UV 相加，不是跨天去重。

访客哈希依赖 `VISITOR_SALT`。换盐会让当天的新旧哈希分裂，UV 可能偏高，安排在 UTC 零点后换。`TRUST_PROXY_HEADERS=false` 时用连接地址，代理后面看到的 IP 会都一样，UV 会偏低；确认代理可信后再打开。

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
make test             # 全部测试；使用 .env 里的 TEST_*，库没准备好时会失败
make test-unit        # 只跑不依赖 PostgreSQL/Redis 的测试
make verify           # 格式校验、vet、单测、lint、build，不启动 Docker
make test-integration # 重建测试库，再跑全部测试（含集成）
make smoke            # 启动真实进程跑冒烟测试
make selfcheck        # test-db + verify + test-integration + smoke
```

`make selfcheck` 不负责启动依赖；用 compose 时先 `make deps-up`，用自己的实例时
确认 `.env` 指向的服务可连。

`make test-integration` 连接 `.env` 里的 `TEST_DATABASE_URL`，删掉 public schema，
再用迁移工具升到最新、降到空、再升到最新，确认 down SQL 可用，然后带 `TEST_*` 跑 `go test -race`。
测试库每次都会被清空，所以库名必须以 `_test` 结尾，防止误清开发库；应用的
`DATABASE_URL` 不会被测试使用。准备测试库不依赖 compose：本机没有 psql 时会用
`postgres:17-alpine` 镜像里的客户端。使用项目自带的 compose 时先跑 `make deps-up`；
使用自己的 PostgreSQL/Redis 时跳过它，确保 `.env` 指向已有实例即可。

`make smoke` 会启动两个真实进程：redirector 默认 `127.0.0.1:18080`，console 默认 `127.0.0.1:18081`，
可分别用 `SMOKE_REDIRECTOR_ADDR`、`SMOKE_CONSOLE_ADDR` 覆盖。它使用测试库和测试 Redis，
验证健康检查、创建、写穿缓存、跳转、HEAD、404、非法参数、过期链接、列表和三种排序、
修改后缓存写穿、删除、统计、租户隔离、路由隔离和指标，最后用 SIGTERM 检查两个进程的优雅退出。
脚本会自己设置一个 `VISITOR_SALT`（如果环境里没有）；同样会重建测试库，不碰开发库。

测试配置在 `.env` 里：

```txt
TEST_DATABASE_URL=postgres://shortlink:shortlink@localhost:5432/shortlink_test?sslmode=disable
TEST_REDIS_URL=redis://localhost:6379/0
```

Redis 缓存测试只读 `TEST_REDIS_URL`，未设置时自动跳过。测试用 `test-` 前缀的唯一短码，
结束后只删除自己写入的 key，不会清空 Redis。

使用自己的 PostgreSQL 时，把 `TEST_DATABASE_URL` 指过去即可；准备脚本会自动创建
不存在的 `_test` 库（数据库用户需要 CREATEDB 权限），然后重建 schema。Redis
同样用 `TEST_REDIS_URL` 指向自己的实例。

## CI

`.github/workflows/ci.yml` 在 push 到 main 和 PR 时运行：

- `verify`：安装固定版本的 golangci-lint，跑 `make verify`。
- `integration`：从 `.env.example` 生成 `.env`，启动 compose 的 PostgreSQL/Redis，
  跑 `make test-integration smoke`。

## 停止

前台运行的 redirector / console 各自用 `Ctrl+C` 停止：进程不再接受新请求，并等待正在处理的请求结束，最多 `SHUTDOWN_TIMEOUT`（默认 10s）。redirector 请求结束后再把点击缓冲写完，最多再等一份 `SHUTDOWN_TIMEOUT`；若当时有一批正在写，那一批还会用完自己的写入超时（默认 2s）。然后关闭 Redis 和数据库。再按一次 `Ctrl+C` 会立刻退出。进程崩溃或被 SIGKILL 时，缓冲区里还没写完的点击会丢掉。`make down` 只停止项目自带的 compose 服务。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| `DATABASE_URL is required` | `.env` 缺失，或没有通过 `make run-*` / `make console` 启动 |
| console 端口被占用 | 换 `CONSOLE_ADDR`，例如 `make run-console CONSOLE_ADDR=:18081`；改端口后重新 `make prometheus` |
| 新短码立刻跳转 404 | 确认创建走 console、跳转走 redirector；console 创建后会写穿 Redis，绕过 console 直接写库不会清掉负缓存 |
| `connect postgres` 失败 | 检查容器是否运行、端口、用户名密码 |
| `connect redis` 失败 | 检查 Redis 是否运行、`REDIS_URL` 是否正确；启动时 Ping 失败进程会退出 |
| `REDIS_URL is required` | `.env` 里没有配置 Redis 连接 |
| `VISITOR_SALT is required` | `.env` 里没有配置访客哈希的盐；从 `.env.example` 补一行开发占位值 |
| 统计里 `unique_visitors` 一直是 0 | 检查 redirector 是否配置了 `VISITOR_SALT`；代理后面还需要 `TRUST_PROXY_HEADERS=true`，否则所有请求的 IP 都一样 |
| `PATCH` 成功后仍跳旧地址 | 看 console 的 `shortlink_cache_writes_total{result="error"}`；Redis 写失败时旧值最多保留到 TTL（1 小时） |
| 跳转大量 503 | 回源并发已满。先看 `shortlink_cache_shed_total{path}` 和 `shortlink_cache_gets_total{result}`：`path="fallback"` 查 Redis，`path="miss"` 说明长尾短码或随机扫描打满了 `CACHE_MISS_CONCURRENCY` |
| cache 测试被跳过 | `.env` 里没有 `TEST_REDIS_URL`；`make test` 会自动加载 `.env` |
| `relation "links" does not exist` | 在仓库根目录执行 `make migrate` |
| `relation "links" already exists` | 库是旧初始化脚本建的，没有 `schema_migrations`。学习用的 compose 库可以 `docker compose down -v`，再 `make deps-up` 和 `make migrate` |
| `refusing to migrate "<库名>": not a shortlink database` | 库名不是 `shortlink` 也不是 `shortlink_` 开头；确认 `DATABASE_URL` 指向本项目的库 |
| `make up` / `make deps-up` 报端口占用 | 本机已有 PostgreSQL 占用 5432；不要启动 compose 的 PostgreSQL，把 `.env` 指向已有实例 |
| 测试清空了链接 | 确认 `TEST_DATABASE_URL` 指向 `_test` 库；应用的 `DATABASE_URL` 不会被测试使用 |
