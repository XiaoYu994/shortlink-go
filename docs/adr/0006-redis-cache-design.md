# 0006. Redis 读缓存的设计

- 状态：Accepted
- 日期：2026-11-12

## 背景

第 9 课要给跳转加 Redis 读缓存。没有缓存时的基线是 50 并发、服务端 p50 3.2ms、p99 21.5ms，连接池峰值打满 10 个连接，`go_sql_wait_count_total` 约 9.9k 次/s，瓶颈在 PostgreSQL 连接池。缓存要解决的是读路径的回源压力，同时不能把 Redis 故障变成请求失败。

实现前讨论过四个问题：缓存值存什么格式、`LinkStore` 接口要不要抽公共定义、测试怎么连真实 Redis、要不要加 `APP_ENV` 环境参数。这个 ADR 记录结论和代价。

## 决策

### 缓存完整 Link 的 JSON

- key 是 `shortlink:link:<code>`，前缀给共享 Redis 留命名空间；value 是完整 `link.Link` 的 JSON。
- 只存 URL 也能满足当前跳转，但命中时拼出来的 `Link` 会丢 `ID` 和 `CreatedAt`。下一步 `ClickRecorder` 要按 `ID` 记点击，零值会变成静默的错数据。存完整 Link 让缓存对调用方透明。
- 用 JSON 而不是自定义的 `id|url`：标准库、能自描述、字段扩展方便。代价是固定的字段名和时间戳开销。
- 实测（Redis 7，URL 60 字节，TTL 1 小时）：只存 URL 每个 key 128 字节，完整 JSON 224 字节，差 96 字节（约 75%）。1000 万个缓存短码约差 0.96 GB。key 和 Redis 对象本身还有约 68 字节固定开销，URL 越长 JSON 占比越小。
- 访问量不直接决定内存，决定内存的是 TTL 窗口内不同短码的数量。以后内存真的成为瓶颈，再把缓存值改成只含 `ID` 和 `URL` 的紧凑结构；缓存格式是内部实现，解码失败按未命中处理，或者升 key 前缀版本即可安全切换。

### 正缓存 1 小时，负缓存 1 分钟

- 正缓存 TTL 1 小时；负缓存写 JSON `null`，TTL 1 分钟。
- 负缓存防止用随机短码反复打数据库；TTL 短是为了不让不存在的短码长期占内存。随机短码攻击下的内存上限约等于 QPS × 负缓存 TTL，这个参数比 JSON 格式更值得关注。
- `null` 解出来是 nil `*link.Link`，`GetByCode` 把它映射成 `link.ErrNotFound`。

### 组合顺序和透传

- 组合顺序固定为 `HTTP -> cache -> storemetrics -> postgres`。缓存命中不进入存储指标，`shortlink_store_*` 只统计真正打到数据库的调用；缓存效果和数据库耗时可以分开看。
- `Create` 成功后写穿正缓存，覆盖可能存在的负缓存；查找回源写负缓存用 `SET NX`，避免和创建并发时后到的负缓存覆盖正缓存。
- 并发未命中用 `singleflight` 按 cache key 合并，同一个短码同时只有一个请求回源。
- `singleflight` 的共享回源用 `context.WithoutCancel` 派生独立 ctx，并保留原来的 deadline。领头请求取消时，等待者不再跟着拿到 `context.Canceled`；回源仍受 `REQUEST_TIMEOUT` 约束。
- 正、负缓存 TTL 都向下随机抖动最多 10%，避免同一批写入的 key 在同一时间集中失效。
- Redis 读失败时最多 8 个请求同时回源，取不到许可的直接返回实现了 `Temporary()` 的错误，HTTP 层映射成 503。许可只包住数据库调用，写缓存不占着它。

### Redis 故障 fail-open

- 读 Redis 出错（`redis.Nil` 除外）或写 Redis 出错都只影响命中率，请求照常回源数据库；读失败时回源并发超过上限的请求返回 503，属于限流，不是失败关闭。
- 启动时 `Ping` 失败仍然让进程退出：连不上 Redis 属于部署配置错误，应该在启动阶段暴露，而不是带病运行。
- 客户端超时和重试调短：`DialTimeout` 500ms、`ReadTimeout`/`WriteTimeout` 200ms、`MaxRetries` 1。go-redis 默认读超时 5 秒、重试 3 次，Redis 挂掉时一次 GET 可能超过 `REQUEST_TIMEOUT`，降级还没走到数据库请求就先超时了。
- 连接参数只保留 `REDIS_URL`：`redis://`、`rediss://`（TLS），可带用户名、密码和 DB 编号；没有密码就是 `redis://localhost:6379/0`。不再单独维护 `REDIS_ADDR`，避免两条配置路径和优先级规则。超时和重试在 URL 解析后统一覆盖。
- 缓存值为坏数据（旧格式、写坏）时删除该 key、按未命中回源，让下一次写入自愈。

### 每个消费方声明自己的 LinkStore

- `httpapi`、`storemetrics`、`cache` 各自定义需要的 `LinkStore` 接口，不抽公共接口。
- Go 接口是结构化满足的，实现方不需要 import 接口包；接口定义在使用方，依赖方向保持 `cache -> link`、`storemetrics -> link`，不互相依赖。
- 接口只有两个方法，重复定义的成本远低于引入公共包造成的耦合。改签名时实现和调用点本来就要一起改，抽公共接口并不会少改几处；等装饰器多到接口开始漂移，再考虑收敛。

### 测试连真实 Redis

- cache 测试读 `TEST_REDIS_URL`；未设置时 `t.Skip`，用于没有 Redis 的 CI。本地在 `.env` 里配置后 `make test` 会真实执行，不是一直跳过。
- 每个测试生成 `test-` 前缀的唯一短码，`t.Cleanup` 只删除自己的 key；绝不 `FLUSHDB`，Redis 可能和别的应用共用。
- 降级测试不需要停 Redis：用已经 `Close()` 的客户端构造 Store，调用会稳定失败并触发回源。
- 覆盖命中、负缓存、singleflight 合并、Redis 故障降级、坏数据自愈、错误不写缓存。

### 暂不引入 APP_ENV

- 当前没有任何行为需要按环境名分叉，加 `APP_ENV=dev/test/prod` 是死配置。
- 单一环境名容易变成分支开关，让测试和生产走不同代码路径，bug 藏在没走到的那条路里。
- 本地用 `.env`，测试用 `TEST_DATABASE_URL`/`TEST_REDIS_URL`，部署环境由平台注入变量；这些已经表达了“在哪运行”。
- 以后需要按环境变化时，加具体行为的开关，而不是万能环境名。现在已经加了 `LOG_LEVEL`；需要 JSON 日志时再加 `LOG_FORMAT`。确实需要环境标识时，只把它当日志和指标的 label，不控制核心逻辑。

## 后果

- 跳转命中缓存时不再占用数据库连接，效果用同一轮压测和基线对比验收（见 `docs/runbook.md`）。
- 缓存值格式和 `link.Link` 绑定。以后给 `Link` 加字段或改序列化格式，需要升 key 前缀版本，或者依赖“解码失败按未命中”自愈。
- 创建成功会写穿正缓存，负缓存用 `SET NX`，避免创建和查找并发时后到的负缓存覆盖正缓存。缓存写失败时仍可能残留负缓存，最多 1 分钟。
- Redis 故障时请求降级回源；回源并发超过上限的请求得到 503，可以稍后重试。TTL 抖动让过期时间分散，代价是实际 TTL 比标称值短最多 10%。
- 测试需要可用的 Redis；没有 `TEST_REDIS_URL` 时 cache 测试跳过，其余测试不受影响。
- 生产上需要给缓存 Redis 配置 `maxmemory` 和淘汰策略，并尽量使用独立实例；默认的 `noeviction` 配置不适合缓存场景。
- 缓存查找记在 `shortlink_cache_gets_total`（`hit` 含负缓存、`miss`、`fallback`），写缓存记在 `shortlink_cache_writes_total`（`success`、`skipped`、`error`；`skipped` 表示负缓存的 `SET NX` 发现 key 已存在），缓存操作耗时记在 `shortlink_cache_operation_duration_seconds`（`operation` 为 `get`、`set`）。压测对比沿用 `docs/runbook.md` 的基线方法，2026-11-12 的结果已记入 runbook（客户端 19,077 req/s，服务端跳转 p50 1.4ms / p99 4.7ms，数据库连接池未被使用）。`ClickRecorder` 的 dropped 计数已接入 `/metrics`，见 [ADR 0007](0007-async-click-recording.md)。

## 参考

- `internal/cache/store.go`、`internal/cache/client.go`
- `docs/architecture.md`、`docs/runbook.md`
- [ADR 0005](0005-prometheus-before-tracing.md)
- [路线图阶段 5](../roadmap.md)
