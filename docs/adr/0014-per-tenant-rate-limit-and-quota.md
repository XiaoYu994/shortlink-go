# 0014. 按租户限流和链接配额

- 状态：Accepted
- 日期：2026-11-17

## 背景

管理 API 只要持有租户的 API Key 就能一直创建链接、一直发请求。没有配额时，一个租户的脚本可以写满数据库；没有限流时，一个客户端可以占满 console 的连接池和处理时间。这是 SaaS 化绕不开的两类保护。

现有约束：

- 身份就是租户，由 `authenticate` 从 API Key 解析并放进 context（[ADR 0009](0009-process-split-and-credentials.md)）。没有人类用户、登录和套餐。
- `tenants` 和 `links` 之间没有外键，引用完整性在应用层（[ADR 0010](0010-app-level-referential-integrity.md)）。计数和判断必须自己保证原子。
- console 可以多副本部署，进程之间不调用、不共享内存。
- 过期链接仍然存在、仍然可管理，删除才是硬删除（[ADR 0011](0011-link-crud-api.md)）。
- 创建链接已经在一次写库路径上；配额判断不能为了准确再引入多一次往返，也不能让并发创建越过上限。

## 决策

### 配额和限流存在租户行上

迁移 004 给 `tenants` 加三列，全部 `NOT NULL` 带数据库默认值，已有和新建租户都有值：

| 列 | 类型 | 默认 | 含义 |
| --- | --- | --- | --- |
| `link_count` | `BIGINT` | `0` | 当前拥有的链接数，含已过期链接 |
| `link_quota` | `BIGINT` | `1000` | 链接总数上限 |
| `rate_limit_per_second` | `INTEGER` | `10` | 管理 API 每秒请求数，`0` 表示不限制 |

`link_count`、`link_quota`、`rate_limit_per_second` 都有 `>= 0` 的 CHECK 约束。迁移按 `links` 回填 `link_count`，之前创建的链接都计入。没有全局环境变量：所有限制都属于租户。

### 配额在创建语句里原子占用

创建链接用一条语句同时占配额和插入：

```sql
WITH quota AS (
    UPDATE tenants
    SET link_count = link_count + 1
    WHERE id = $4 AND link_count < link_quota
    RETURNING 1
)
INSERT INTO links (code, url, expires_at, tenant_id)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM quota)
   OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = $4)
RETURNING id, created_at
```

- `UPDATE` 的 `WHERE link_count < link_quota` 在行锁上重新判断，并发创建只会排队，不会都读到旧值后一起加一。
- 语句失败（短码唯一约束冲突）时配额更新一起回滚，重试不会重复计数。
- 没有返回行时再查一次 `link_count >= link_quota`，区分「配额满」和「租户不存在」。配额满返回领域错误 `link.ErrQuotaExceeded`，HTTP 层映射成 403 和 `{"error":"link quota exceeded"}`。
- 租户行不存在时照常插入链接，不计数。这是 ADR 0010 允许的孤儿引用，只会在直接调用存储时出现，HTTP 路径的租户一定来自鉴权。
- 删除链接在原有事务里最后一步执行 `UPDATE tenants SET link_count = GREATEST(link_count - 1, 0)`。先锁链接、再更新租户行，和创建的「先更新租户行、再插入新链接行」不会形成环；`GREATEST` 兜住外部把计数改小的情况。
- 过期链接不释放配额：它仍然存在、仍然可管理，只有删除才减一。

### 限流是进程内令牌桶

- 新增 `internal/httpapi` 里的 `tenantRateLimiter`，用 `golang.org/x/time/rate`，按租户 ID 各一个桶。
- 速率和突发容量都取 `rate_limit_per_second`：长期每秒 N 个请求，允许一次突发 N 个。`0` 表示不限制，也不建桶。
- 中间件只包管理路由，必须放在 `authenticate` 里面：先有租户，才有桶。公开跳转、`/healthz`、`/readyz`、`/metrics` 和不带密钥的 401 流量都不经过它。
- 超限返回 429，`Retry-After` 是整数秒（`ceil(需要等待的时间)`，至少 1），错误体沿用 JSON 格式：`{"error":"rate limit exceeded"}`。拒绝的请求不会进入 handler，不查库、不写缓存。
- 租户的速率随 `Authenticate` 的查询一起读出，不额外查库。桶上记录的速率和租户当前值不一致时，换一个新桶从满桶开始：改完库下一次请求就生效，不用重启。只调 `SetLimit` / `SetBurst` 的话，桶里不足一个令牌时新请求仍会被拒，看起来像没生效。
- 桶的 map 不会一直增长：每个请求顺带检查一次，最多每分钟遍历一次，清掉超过 10 分钟没有请求的租户。清理不单开 goroutine，也就不需要退出点。被清掉的租户下次请求从满桶开始，限流状态丢一点可以接受。
- 新增 `shortlink_rate_limited_total`（无 label）记录被拒绝的请求数。租户 ID 不做 label，租户数量没有上界。存储指标新增 `result="quota_exceeded"`，把配额不足和真正的创建失败分开。

### 运维入口

不改管理 HTTP 契约，只加 `cmd/console` 命令：

```bash
console tenant show --tenant ID
console tenant set --tenant ID [--link-quota N] [--rate-limit N]
```

`set` 至少给一个选项，用 `flag.Visit` 区分「没传」和「显式传 0」；负数在 CLI 和存储层都拒绝。把 `link_quota` 调到当前 `link_count` 以下不会删除已有链接，只是在删除腾出配额前不能再创建。

## 不做什么

- 不做 Redis/Lua 分布式限流。进程内限流的上限会乘以 console 副本数：3 个副本、每租户 10/s 时，总量可能到 30/s。这个误差可以接受，因为限流保护的是单个进程的数据库连接和处理时间；要全局限额时再换 Redis。这一点必须记住，不能把 `rate_limit_per_second` 当成跨副本的精确承诺。
- 不限流公开的 `/r/` 跳转，也不限流未鉴权的 401 流量。前者属于高 QPS 的读路径，要限也应该在 nginx 或网关按 IP 做；后者在鉴权之前，还没有租户可做键，同样交给反向代理。
- 不做套餐、计费、按接口区分的限额（创建、列表、统计共用同一个速率）。现在只有「每租户一个数」的需求。
- 不提供修改配额的公开 HTTP 接口。运维改库，避免多一个鉴权和审计面。
- 不做计数自动对账。`link_count` 由应用维护；绕过应用直接改 `links` 时会漂移，runbook 里给了一条重算 SQL，需要时手工执行。
- 不把已删除的链接、点击统计数据算进配额。配额只数当前拥有的链接。

## 怎么验证

- `internal/httpapi` 的 httptest：第二个请求返回 429、`Retry-After` 和 JSON 错误体；不同租户互不影响；改掉租户的速率后下一次请求就按新值；`0` 不限流；假时钟下空闲租户被清理；`link.ErrQuotaExceeded` 映射成 403，包了一层的错误也能认出来。
- `internal/postgres` 集成测试（没有 `TEST_DATABASE_URL` 时跳过）：配额满后创建返回 `ErrQuotaExceeded`，过期链接仍占配额，删除释放配额；20 个并发创建撞 5 个配额，成功的正好 5 个，`links` 行数和 `link_count` 都是 5；删除不会把计数减成负数；`Authenticate` 带回速率；`tenant show/set` 背后的读写和默认值。
- `make smoke`：把冒烟租户的配额压到当前计数，创建返回 403；删除一条过期链接后创建成功；把速率改成 1/s 后紧接的第二次创建返回 429 并带 `Retry-After`，改回后立即恢复 201。
- `make selfcheck` 会先跑迁移的 up、down、up，验证 004 的回滚 SQL。

## 后果

- 创建链接多了一次 `tenants` 行更新的锁竞争：同一个租户的并发创建会串行在租户行上。这是配额准确性的代价；不同租户互不影响。
- 计数器是派生数据，绕过应用改 `links` 会漂移。删除路径用 `GREATEST` 避免负数，重算方法写进 runbook。
- 多副本下实际允许的速率是配置值乘以副本数；扩容 console 会让限流变宽，需要时按副本数调整配置。
- 改限制会重置该租户的桶，改完的瞬间多出一个突发容量。运维操作低频，可以接受。
- 限流 map 最多每分钟被遍历一次；map 大小是活跃租户数，不是历史租户总数。
