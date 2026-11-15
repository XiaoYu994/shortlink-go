# 0012. 点击统计：日聚合、客户端维度和统计接口

- 状态：Accepted
- 日期：2026-11-15

## 背景

目前每条链接只有 `links.clicks` 一个累计值。`ClickRecorder` 只把链接 ID 攒成一批，`AddClicks` 直接 `UPDATE links SET clicks = clicks + n`（[ADR 0007](0007-async-click-recording.md)），没有时间、来源、设备和访客信息。

这一课的目标是**运营看板**，不是自助数据分析：

- 按天看每条链接的点击趋势。
- 按 `Referer` 的 host 看 Top N 来源。
- 按 `User-Agent` 看设备形态和客户端类型（浏览器、机器人、CLI、其他）。
- 按天去重统计独立访客，只存哈希，不存原始 IP 和 User-Agent。
- 列表支持按今日访问和历史累计排序。

压测基线显示跳转在缓存命中时可以到 1.9 万 req/s。如果每次点击存一行原始事件，一天约 16 亿行；采样、分区、消息队列或列式存储都会超出这一课的范围。看板需要的四个维度都可以在写入时按天聚合，不需要保留每次点击。代价是只能看聚合结果，不能回查单次点击，历史 `links.clicks` 也无法还原时间分布。自助分析、明细导出、按任意维度下钻需要原始事件或列式存储，留到后续阶段。

## 决策

### 三张日聚合表

新增一个迁移，建三张表，不使用数据库外键（[ADR 0010](0010-app-level-referential-integrity.md)）：

```sql
CREATE TABLE link_clicks_daily (
    tenant_id BIGINT NOT NULL,
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,          -- UTC 日期
    clicks    BIGINT NOT NULL,
    PRIMARY KEY (link_id, day),
    CONSTRAINT ck_link_clicks_daily_positive CHECK (clicks > 0)
);

CREATE INDEX idx_link_clicks_daily_tenant_day_clicks
    ON link_clicks_daily (tenant_id, day, clicks DESC, link_id DESC);

CREATE TABLE link_dimension_daily (
    link_id   BIGINT NOT NULL,
    day       DATE   NOT NULL,
    dimension TEXT   NOT NULL,          -- referrer | device | client
    value     TEXT   NOT NULL,
    clicks    BIGINT NOT NULL,
    PRIMARY KEY (link_id, day, dimension, value),
    CONSTRAINT ck_link_dimension_daily_dimension
        CHECK (dimension IN ('referrer', 'device', 'client')),
    CONSTRAINT ck_link_dimension_daily_value_length
        CHECK (octet_length(value) BETWEEN 1 AND 255),
    CONSTRAINT ck_link_dimension_daily_positive CHECK (clicks > 0)
);

CREATE TABLE link_visitors_daily (
    link_id      BIGINT NOT NULL,
    day          DATE   NOT NULL,
    visitor_hash BYTEA  NOT NULL,
    PRIMARY KEY (link_id, day, visitor_hash),
    CONSTRAINT ck_link_visitors_daily_hash_length
        CHECK (octet_length(visitor_hash) = 16)
);
```

- `links.clicks` 保留，作为终身累计的读模型；时间序列和维度的事实源是这三张日表。两者在同一个事务里维护，见 [ADR 0011](0011-link-crud-api.md)。
- `link_clicks_daily.tenant_id` 是冗余列，写库时由 `JOIN links` 填，用来服务按租户的今日排序；表仍然以 `(link_id, day)` 为主键。
- `link_dimension_daily` 用一张通用维度表承载来源、设备、客户端，避免三张结构相同的表。以后加 OS、浏览器家族、国家时改一次 `CHECK` 加 `dimension` 值，不用加新表。
- 三张表都不建 `link_id` 外键；删除链接时在应用层同一事务清理统计行，孤儿行用对账 SQL 发现。

### 点击数据

`click.Click` 从链接 ID 扩展为：

```go
type Click struct {
    LinkID      int64
    At          time.Time  // 入队时刻，UTC
    Referrer    string     // host 或 direct/other
    Device      string     // desktop/mobile/tablet/bot/other
    Client      string     // browser/bot/cli/other
    VisitorHash []byte     // 16 字节，没有访客信息时为 nil
}
```

- `At` 在跳转成功、进入 `Record` 时取，不等到批量写出；日期按 `At.UTC()` 计算，flush 延迟不会挪动日期。
- `Record` 仍然只做非阻塞投递；构造元数据是纯 CPU 操作，不做 I/O。
- `HEAD`、客户端已断开和存储找不到链接的请求继续不记点击。

### 来源、设备、客户端和访客口径

- **来源**：解析 `Referer` 的 hostname，转小写并去掉一个前导 `www.`；空值记 `direct`；非法、没有 hostname 或超过 255 字节记 `other`。不存完整 URL、path 和 query；活动归因（UTM）不在这一课。
- **设备**：`desktop` / `mobile` / `tablet` / `bot` / `other`，看屏幕形态。
- **客户端**：`browser` / `bot` / `cli` / `other`，回答“是不是浏览器访问”。
  - 先判机器人，再判 CLI（curl、wget、httpie、Go-http-client、python-requests 等），再判常见浏览器（Chrome、Safari、Firefox、Edge、Opera、Samsung Internet 等），其余归 `other`。
  - 手机 App 的 WebView 经常带 Safari/Chrome 的 UA，无法可靠区分“浏览器”和“App 内网页”；UA 也可以伪造，所以这是启发式，不是安全边界。
  - 需要 OS/浏览器版本细分时再引 UA 解析库，类别仍固定。
- **访客**：`HMAC-SHA256(VISITOR_SALT, "v1\x00" + UTC 日期 + "\x00" + 客户端 IP + "\x00" + User-Agent)`，取前 16 字节存 `BYTEA`。
  - 日期参与 HMAC，所以同一访客跨天得到不同哈希，跨天不可关联。每日 UV 精确；区间 `unique_visitors` 是每日 UV 相加，不是跨天去重。
  - 不存原始 IP 和 User-Agent，也不写进日志。HMAC 是伪匿名，不是匿名化；盐是密钥，不能提交到仓库。
  - 盐变化会让同一天内新旧哈希分裂；需要换盐时在 UTC 零点后做，并接受当天的 UV 可能偏高。
  - IP 或盐拿不到时不记访客，点击趋势和维度照常记录。
- **客户端 IP**：默认取 `RemoteAddr` 的 host。部署在反向代理后，`TRUST_PROXY_HEADERS=true` 时取 `X-Forwarded-For` 的第一个合法 IP；这个开关只能开在可信代理后面，否则客户端可以伪造 IP 刷高 UV。
- **长度上限**：`User-Agent` 参与分类和哈希前截到 512 字节，`Referer` 超过 2048 字节直接归 `other`，避免恶意超长头放大内存和哈希开销。`X-Forwarded-For` 只取第一段并校验成 IP。
- **配置**：`VISITOR_SALT` 由 `config.LoadConfig` 读取；redirector 启动时为空则直接报错，避免静默丢 UV。`TRUST_PROXY_HEADERS` 默认 `false`。console 不用盐。

### 写入路径

- 每个 `ClickRecorder` 批次先在 Go 里按键去重聚合：
  - `(link_id, 日)` → 点击数
  - `(link_id, 日, dimension, value)` → 点击数，dimension 为 `referrer`、`device`、`client`
  - `(link_id, 日, visitor_hash)` → 存在即可
- `click.Store` 接口从 `AddClicks(ctx, map[int64]int64)` 改为 `RecordClicks(ctx, []click.Click)`；`postgres.Store` 实现它，并导入 `click` 包做聚合和写库。`click` 不反向依赖 `postgres`。
- 一个批次在一个事务里执行：
  1. `INSERT INTO link_clicks_daily (tenant_id, link_id, day, clicks) SELECT l.tenant_id, d.link_id, d.day, d.clicks FROM unnest(...) AS d(...) JOIN links l ON l.id = d.link_id ON CONFLICT (link_id, day) DO UPDATE SET clicks = link_clicks_daily.clicks + EXCLUDED.clicks`
  2. 用同样的 `INSERT ... SELECT ... JOIN links ... ON CONFLICT ... DO UPDATE` 写 `link_dimension_daily`，数组里同时带 referrer、device、client 三类行。
  3. `INSERT INTO link_visitors_daily ... SELECT ... JOIN links ... ON CONFLICT DO NOTHING`。
  4. 现有的 `UPDATE links SET clicks = clicks + n`。
- `JOIN links` 让已经删除的链接不再产生统计行，同时填 `link_clicks_daily.tenant_id`；`links.clicks` 的更新本来就会匹配不到行。统计写入和累计计数同一个事务提交，要么都成功，要么这一批都不写。
- 每批最多 100 个点击；点击表去重后不超过 100 行，维度表最多三类各不超过 100 行，访客表不超过 100 行。写语句从 1 条 UPDATE 变成 2 条 upsert、1 条访客 insert 加 1 条 UPDATE。上线后要压一轮，观察 `shortlink_clicks_dropped_total` 和连接池，必要时调整 `BatchSize`/`Interval`。
- 这批写入失败时仍按 [ADR 0007](0007-async-click-recording.md) 丢掉整批并记日志，不阻塞跳转；`shortlink_clicks_written_total{result}` 的口径不变。
- 删除和点击并发时，极端情况下可能留下统计孤儿行；统计接口先按 `code + tenant_id` 解析链接，孤儿行查不到，不影响结果。对账 SQL 见下文。

### 统计接口

`GET /api/v1/links/{code}/stats`，在 console，套 `authenticate`。日粒度，不提供小时参数。

查询参数：

- `from`、`to`：RFC3339，转成 UTC 后按天取整。默认 `to` 为当前日期，`from` 为 `to - 7 天`。
- 要求 `from < to`，跨度不超过 90 天；非法参数返回 400。
- 两端所在的整日都计入；聚合表的最小粒度是天，边界所在的整日会整日计入。
- 链接按 `code + tenant_id` 查，过期链接可以看，已删除或其他租户的链接返回 404。

响应：

```json
{
  "code": "AbC123xy",
  "total_clicks": 1234,
  "range_clicks": 120,
  "from": "2026-11-08T00:00:00Z",
  "to": "2026-11-15T00:00:00Z",
  "clicks": [{"day": "2026-11-08", "clicks": 10}],
  "referrers": [{"referrer": "news.ycombinator.com", "clicks": 30}],
  "devices": [{"device": "desktop", "clicks": 40}, {"device": "mobile", "clicks": 50}],
  "clients": [{"client": "browser", "clicks": 80}, {"client": "bot", "clicks": 5}],
  "daily_visitors": [{"day": "2026-11-08", "visitors": 8}],
  "unique_visitors": 42
}
```

- `total_clicks` 取 `links.clicks`，是终身累计；`range_clicks` 是本次时间范围内的日表之和。迁移前的点击只有 `total_clicks`，没有 `clicks` 序列。
- `clicks`、`daily_visitors` 按天补零；`devices` 固定返回五个类别，`clients` 固定返回四个类别，没有数据的补 0。
- `referrers` 取范围内点击数最多的 10 个 host，按点击数降序、host 升序；不返回 `other` 的合并行。
- `unique_visitors` 是 `daily_visitors` 之和，按天去重后相加，不跨天去重。
- 统计查询在一个只读的 `REPEATABLE READ` 事务里执行：先查链接存在性和 `links.clicks`，再查三张日表。这样同一批点击的各维度在响应里是同一个快照。
- 统计接口不经过 Redis 缓存，直接查 PostgreSQL；管理接口流量低，先不做结果缓存。
- 按 `(link_id, ...)` 主键前缀扫描，最多 90 天；`link_clicks_daily` 额外有 `(tenant_id, day, clicks DESC, link_id DESC)` 索引服务今日排序。

### 排序和 `links.clicks` 的角色

- `links.clicks` 是终身累计的读模型：列表展示、按历史访问排序和迁移前历史都需要它。它和日表在同一个事务里更新；上线后的部分可以从日表重算，迁移前的历史除外。
- 历史排序用 `links.clicks`，索引 `(tenant_id, clicks DESC, id DESC)`；今日排序用 `link_clicks_daily` 的 `(tenant_id, day, clicks DESC, link_id DESC)` 索引。具体 API、游标和分页语义见 [ADR 0011](0011-link-crud-api.md)。
- 排序字段会随点击变化，翻页时可能出现重复或漏项；如果只需要 Top N，后续可以加专门的 top 接口，不依赖游标分页。

### 删除、孤儿行和保留

- 删除链接时在同一事务里删除 `link_clicks_daily`、`link_dimension_daily`、`link_visitors_daily` 中该链接的行，再删 `links` 行。
- 与删除并发的点击批次可能看到删除前的快照，留下孤儿统计行；按链接查询不可见。对账 SQL 沿用 ADR 0010 的做法，正常结果应为 0：

  ```sql
  SELECT 'link_clicks_daily' AS tbl, count(*) AS orphans
  FROM link_clicks_daily s LEFT JOIN links l ON l.id = s.link_id
  WHERE l.id IS NULL
  UNION ALL
  SELECT 'link_dimension_daily', count(*)
  FROM link_dimension_daily s LEFT JOIN links l ON l.id = s.link_id
  WHERE l.id IS NULL
  UNION ALL
  SELECT 'link_visitors_daily', count(*)
  FROM link_visitors_daily s LEFT JOIN links l ON l.id = s.link_id
  WHERE l.id IS NULL;
  ```

- 这一课不做自动清理。日表每个链接每天最多一行；维度表每个链接每天按维度值各一行；访客表每个链接每天每个访客一行。数据量增长后再加 `console stats prune --before` 或按月分区。
- 来源是唯一能被客户端任意撑大的维度：伪造 `Referer` 可以制造大量 host。当前用 255 字节上限、Top N 查询和后续清理兜底；如果被滥用，改成来源类别或采样。设备和客户端类别固定，不受影响。
- 国家/地区需要 GeoIP 数据，这一课不做；以后作为新维度加值。

## 后果

- 点击写入从 1 条 UPDATE 变成 2 条 upsert、1 条访客 insert 加 1 条 UPDATE，事务更大。跳转路径仍然异步，不增加响应延迟；吞吐影响要看 dropped 和连接池指标。
- 统计只有日聚合结果：没有小时趋势，不能回查单次点击，不能事后补来源/设备/客户端/访客维度，迁移前的历史也只有累计总数。这是换取可控写入量和看板查询速度的代价。
- `unique_visitors` 是按天去重后相加，不是跨天唯一人数；同一访客换 IP、换 UA 或跨天都会被算成多个。
- 访客表只存 16 字节哈希，但盐泄露或配置错误会影响伪匿名性；盐不能进仓库，换盐会影响 UV 统计。
- 来源、设备、客户端数据从上线后开始；`total_clicks` 和 `range_clicks` 在迁移初期会差很多，API 文档要写清楚。
- 新增 `VISITOR_SALT` 和 `TRUST_PROXY_HEADERS` 配置；`.env.example`、runbook、architecture、CI/smoke 都要更新。redirector 没有盐时启动失败。
- 统计查询使用只读事务，管理接口多几次往返；HTTP 指标会记录耗时。数据量变大后先看查询耗时，再决定是否加索引或缓存。
- 测试要覆盖：来源规范化、设备/客户端分类、访客哈希按天变化、批量事务、删除级联、统计范围校验、空桶补零、Top N 排序、今日/历史排序。
- `make smoke` 要带 `Referer`、`User-Agent` 和 `VISITOR_SALT`，验证点击后统计接口能读到趋势、来源、设备、客户端和访客。

## 备选方案

- **小时聚合**：能看当天内的峰值，但表多 24 倍、写入更多，而且前端需求是日趋势和今日排序；这一课选日粒度。以后需要小时趋势时新增小时表，历史小时数据不能回填。
- **原始点击事件表**：查询最灵活，但按 1.9 万 req/s 一天约 16 亿行，必须采样、分区或引入列式存储；不适合这一课。
- **Redis HyperLogLog 估算 UV**：内存省、可跨天合并，但 Redis 是缓存不是统计的持久来源，丢实例会丢 UV；选择 PostgreSQL 精确的每日访客表。
- **固定盐跨天去重**：区间 UV 更严格，但哈希长期可关联，隐私更弱；选择按天盐。
- **显式维度表**（referrer、device、client 各一张）：约束更清楚，但结构重复、写入语句更多；选择通用维度表。
- **来源只存类别**：基数固定、抗伪造，但看不到具体站点；选择 host，接受基数风险并保留改类别的退路。

## 参考

- `internal/click/recorder.go`：`Record`、`flush`、`Close`
- `internal/postgres/store.go`：`AddClicks`
- `internal/httpapi/redirect.go`：跳转后记录点击
- `internal/cache/store.go`：点击和缓存无关，统计不经过缓存
- [ADR 0007](0007-async-click-recording.md)、[ADR 0010](0010-app-level-referential-integrity.md)、[ADR 0011](0011-link-crud-api.md)
- [路线图阶段 7](../roadmap.md)
