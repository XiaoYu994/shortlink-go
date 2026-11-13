# 0008. 冷缓存和长尾短码的读路径保护

- 状态：Proposed
- 日期：2026-11-13

## 背景

跳转的读路径是 `HTTP -> cache -> storemetrics -> postgres`。缓存命中时只访问 Redis；未命中时按 cache key 做 singleflight，合并后的领头请求回源 PostgreSQL。目前只有 Redis 读失败（`fallback`）时限制回源并发：每个进程 8 个许可，超过的直接返回实现了 `Temporary()` 的错误，HTTP 层映射成 503（[ADR 0006](0006-redis-cache-design.md)）。

普通 cache miss 没有这层限制，只有 `DB_MAX_OPEN_CONNS`（默认 10）兜底。命中率取决于访问分布：热门短码命中后压力很小，但大量不同短码、冷启动或随机扫描会让未命中成为主要流量。此时请求会在连接池上排队，最长排到 `REQUEST_TIMEOUT`（默认 5s）再返回 504，吞吐被数据库连接数卡死；并发继续升高时，goroutine、连接缓冲和请求上下文一起占内存，进程有被 OOM kill 的风险。

现有压测没有覆盖这个场景：`docs/runbook.md` 的缓存压测只压同一个热 key，读数注意里写明“不能据此推断多 key、缓存过期或 Redis 故障时的表现”。无缓存基线已经显示连接池峰值 10、`go_sql_wait_count_total` 约 9.9k 次/s，说明连接池是读路径的瓶颈；但“大量不同短码”下的排队、503/504 比例和拐点还没有数字。

这个问题不是缓存算法能解决的：缓存只对重复访问有效，长尾短码的首次访问必然回源。目标是限制回源并发、快速失败，并给数据库扩容留出清晰的升级路径。

## 决策

先做读路径保护，不改变缓存格式和命中逻辑。状态是 Proposed；实施前先完成多 key 压测，用数字确认许可数和超时。

### 正常未命中加独立的 bulkhead

- 在 `CachedStore` 里增加 `missSlots`（名字待定），与 Redis 故障时的 `fallbackSlots` 分开计数。普通 `miss` 回源也要先拿许可，拿不到直接返回临时过载错误，HTTP 层映射成 503。
- 许可必须在 singleflight 回调内部获取，只包住数据库调用：同一短码的等待者共享领头的查询结果，不消耗额外许可；写缓存不占许可。和现有 `readStore` 的 fallback 实现保持一致。
- 许可数先取与连接池同量级的值（例如 8–16），通过 `CACHE_MISS_CONCURRENCY` 配置；默认值在多 key 压测后确定。进程内许可乘以副本数才是数据库的总并发，多副本部署时要和连接池/PgBouncer 一起算。
- 新增指标区分“被限流”和真实 miss/fallback，例如在 `shortlink_cache_gets_total` 增加 `shed`，或单独的 counter。计数口径按请求而不是按回源：singleflight 的等待者也会收到同一个错误，应该算一次 shed；不要只记领头请求。不要复用 `error`，否则看不出是主动拒绝还是数据库故障。

### 回源超时独立于请求时限

- 正常 miss 的回源使用单独的超时（例如 `CACHE_FETCH_TIMEOUT`，默认 1s），不再把 `REQUEST_TIMEOUT`（5s）当成排队上限。数据库正常 p99 在 20ms 量级，1s 足够；超时后快速返回 504/503，避免请求在连接池队列里挂满 5 秒。
- singleflight 的共享回源仍用独立 ctx（`context.WithoutCancel` 加 deadline），但 deadline 取上述回源超时，而不是原来的请求 deadline。领头请求取消不应连累等待者；超时必须对所有等待者一致。
- 需要注意：缩短回源超时会让慢查询更早失败。先看 `shortlink_store_operation_duration_seconds` 的 p99 再定值；如果数据库正常 p99 已接近 1s，先查数据库，而不是调大超时。

### 多 key 压测作为验收

- 准备 N 个不同短码（例如 1 万条），在 Redis 为空时随机访问，分别测缓存命中、全部 miss、大量不存在的短码三种流量。记录 `shortlink_cache_gets_total{result}`、`go_sql_in_use_connections`、`go_sql_wait_count_total`、`go_sql_wait_duration_seconds_total`、HTTP 5xx 比例和 p99。
- 用压测结果确定 `CACHE_MISS_CONCURRENCY` 和 `CACHE_FETCH_TIMEOUT`，把数据和结论补进 `docs/runbook.md`。没有数字之前不把默认值当作结论。
- 验收标准是：数据库连接池不再长时间顶满、503 是快速返回而不是 5 秒后超时、进程在过载时内存和 goroutine 数量有界。命中率本身不是这项的验收指标。

### 暂不做

- L1 进程内缓存：只对热 key 有效，解决不了长尾；先不加一层一致性和内存管理成本。
- 分布式锁/逻辑过期：解决同一个热 key 的多副本击穿，和大量不同 key 的问题不同；仍留在路线图的可选项里。
- 只读副本、PgBouncer、CDN/边缘缓存：属于容量和部署层方案。先完成 bulkhead 和多 key 压测，确认数据库仍是瓶颈后再上；多副本部署前尤其要先解决连接数放大问题。
- Bloom filter：可以挡随机扫描，但维护一致性成本高；负缓存和入口限流先做。

## 后果

- 过载时部分请求会得到 503；这是主动限流，换取数据库不被排队打穿和进程内存有界。503 必须带可重试语义，客户端可以退避重试。
- 正常未命中的回源并发从“由连接池隐式限制”变成“显式可配置”。配置过小会在正常突发时误伤，过大会退化成原来的排队；需要用压测和 `shed` 指标调整。
- 回源超时变短后，数据库抖动会更快暴露为 504/503；这是有意的快速失败，但会增加错误率，需要在监控里区分超时、限流和数据库故障。
- Redis 故障时的 `fallbackSlots` 语义不变；普通 miss 和 fallback 是两条限流路径，排障时先看 `shortlink_cache_gets_total{result}` 判断请求走的是哪条。
- 这项改动不提高缓存命中率，也不减少长尾首次访问的数据库读取；它只保证在数据库成为瓶颈时服务能可控降级。真正的容量提升仍然要靠连接池调优、PgBouncer、只读副本或边缘缓存。
- 多副本下进程内许可的总和可能远大于数据库承受能力；部署新副本时必须重新计算连接预算。

## 参考

- `internal/cache/store.go`：`GetByCode`、`readStore`、`fallbackSlots`
- `internal/cache/client.go`：Redis 超时和重试
- `internal/httpapi/timeout.go`：`REQUEST_TIMEOUT`
- `docs/runbook.md`：无缓存基线、缓存压测和读数注意
- [ADR 0006](0006-redis-cache-design.md)
- [路线图阶段 5](../roadmap.md)
