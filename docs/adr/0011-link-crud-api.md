# 0011. 短链管理 API 的增删改查和排序

- 状态：Accepted
- 日期：2026-11-15

## 背景

阶段 6 的管理 API 只有 `POST /api/v1/links`。管理页面和用户平台需要列表、详情、修改、删除，列表还要支持按今日访问和历史累计排序；没有这些接口，只能直接改数据库，租户隔离和缓存一致性都没有统一入口。

边界已经由前面的 ADR 定好：

- [ADR 0009](0009-process-split-and-credentials.md)：管理 API 在 console，redirector 只做公开跳转；租户身份由 `authenticate` 放进 context。
- [ADR 0010](0010-app-level-referential-integrity.md)：`links` 用 `(tenant_id, id)` 索引；列表游标用 id，不用 `created_at`；不使用数据库外键。
- 缓存 key 是 `shortlink:link:<code>`；创建时写穿，redirector 读取。新增更新和删除后，console 必须同步维护同一份缓存。

需要同时解决四类问题：跨租户隔离、过期链接在管理视角和跳转视角的不同语义、PATCH 的可空字段，以及管理写路径对缓存的影响。

## 决策

### 路由和鉴权

新增以下 console 路由，全部套现有 `authenticate`：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/links` | 当前租户的链接列表，支持按今日/历史访问排序 |
| `GET` | `/api/v1/links/{code}` | 单条详情，包含已过期链接 |
| `PATCH` | `/api/v1/links/{code}` | 修改 `url` 和/或 `expires_at` |
| `DELETE` | `/api/v1/links/{code}` | 硬删除，返回 204 |

- redirector 不注册管理路由；`/r/{code}`、`/healthz`、`/metrics` 行为不变。`/readyz` 另做就绪检查，失败返回 503。
- 每个管理路由单独包 `authenticate`。Go 的 `ServeMux` 没有路由组，不能把 `/api/v1/` 包一层再指望更具体的路由继承中间件。
- 所有路由都在 `/api/v1` 下；新增接口是兼容扩展，不升 v2。

### 租户隔离

- 所有查询都带 `tenant_id = $n`，`tenant_id` 只来自 context，请求体不能传。
- 其他租户的链接和不存在的链接都返回 404，不返回 403，也不透露链接是否存在。
- 列表只返回当前租户的行；详情、修改、删除按 `code + tenant_id` 查。

### 管理视角和跳转视角的过期语义

- 管理视角的详情、列表、修改、删除**不按 `expires_at` 过滤**；管理员能看到、修改、删除过期链接。
- redirector 的 `GetByCode` 继续过滤 `expires_at IS NULL OR expires_at > now()`；过期链接跳转仍返回 404。
- 修改 URL 不会改变过期状态；把 `expires_at` 改到将来可以让过期链接重新生效。
- 新增 `GetForTenant` 给管理接口，它不经过 Redis 缓存；缓存里只放跳转可见的链接。`GetByCode` 的缓存语义不变。
- 管理响应用 `link.AdminLink`（`Link` + `Clicks` + `ClicksToday`），不把计数加进 `link.Link`。缓存继续只存 `link.Link`，避免把会过期的计数写进缓存。

### 列表、排序和分页

- `GET /api/v1/links?limit=20&sort=id&cursor=...`，固定降序；`sort` 取值 `id`（默认）、`clicks`（历史累计）、`clicks_today`（今日访问）。
- 响应 `{"items":[...],"next_cursor":"..."}`；没有下一页时省略 `next_cursor`。列表项带 `clicks` 和 `clicks_today`。
- `limit` 默认 20，最大 100；非法值和非法游标返回 400。
- 不返回总数。`COUNT(*)` 每次列表都要扫租户的行，当前没有需求，先不加。
- 列表包含已过期链接，不提供状态过滤；客户端根据 `status` 判断。

三种排序的查询和游标：

- `sort=id`：`WHERE tenant_id = $1 AND ($2 = 0 OR id < $2) ORDER BY id DESC`，走已有的 `(tenant_id, id)` 索引。游标是上一页最后一条的 id，稳定，作为默认排序。
- `sort=clicks`：`WHERE tenant_id = $1 AND (clicks, id) < ($2, $3) ORDER BY clicks DESC, id DESC`，索引 `(tenant_id, clicks DESC, id DESC)`。游标编码 `(clicks, id)`；`clicks` 会随点击变化，翻页时可能出现重复或漏项，看板场景接受。
- `sort=clicks_today`：左连接 `link_clicks_daily` 上今天的行，按 `COALESCE(d.clicks, 0) DESC, id DESC` 排序；`link_clicks_daily` 上的 `(tenant_id, day, clicks DESC, link_id DESC)` 索引服务今天的 Top N。游标编码 `(clicks_today, id)`；`clicks_today` 同样会变，翻页不保证稳定。
- 游标对客户端不透明；服务端按 `sort` 解码成对应的字段组合。`sort=id` 的游标可以就是 id，其他两种排序用 `<key>:<id>` 形式的编码。
- 按今日排序要对租户内的链接集合排序；租户链接很多时 `LEFT JOIN` 加排序会变重，`link_clicks_daily` 的索引主要服务 Top N。当前先接受，真成为瓶颈时加专门的 `GET /api/v1/links/top?period=today|all`，只返回 Top N。

### PATCH 语义

- 请求体上限和创建接口一样（约 12KB）；必须恰好是一个 JSON 值，未知字段和尾部多余内容都返回 400。
- 只处理 `url` 和 `expires_at`：
  - `url` 出现时按现有规则校验：http/https 绝对 URL、无空白、最多 2048 字节。
  - `expires_at` 出现且为字符串时必须是将来时间；出现且为 `null` 时清除过期时间，改为永不过期；没出现时保持不变。
- 空对象、`url` 和 `expires_at` 都没出现、未知字段都返回 400。
- 不能改 `code`、`tenant_id`、`created_at`、`clicks`、`clicks_today`。
- Go 的 `*time.Time` 区分不了「没传」和「传了 null」；实现用 `json.RawMessage` 或自定义可选类型。语义由本 ADR 定，写法在 handler 里选。
- 修改用一条原子 `UPDATE`：没传的字段保持原值，两个字段可以并发修改，同一字段最后提交者生效。不做 `If-Match` 或版本号；管理写频率低，先接受同一字段的并发覆盖，需要时再加乐观锁。
- URL 和 `expires_at` 的校验从 `NewLink` 里抽成可复用的函数，创建和修改共用，避免两套规则漂移。
- 成功返回 200 和修改后的链接表示。

### 删除

- `DELETE /api/v1/links/{code}` 返回 204，无响应体。
- 硬删除：删除 `links` 行和该链接的统计行（[ADR 0012](0012-click-analytics.md)）放在同一事务；链接不存在或属于其他租户返回 404。
- 删除后管理列表不再出现，短码从唯一约束里消失。短码是 8 位随机码，理论上可能被以后的新链接随机生成（概率极低）；不建墓碑或保留表。如果以后要求短码永不复用，再改成软删除或独立墓碑表。
- 删除不是强撤销：已经在途的跳转仍可能在删除提交前读到链接并返回 302；缓存负缓存写失败时旧值也可能保留到 TTL。管理员应把这理解为最终一致。

### 缓存一致性

这是新增写路径最重要的部分。

- 缓存 key 不变：`shortlink:link:<code>`。创建成功后无条件 `SET` 正缓存（现有行为）。
- 回源写缓存（正、负）都用 `SET NX`；`SET NX` 只在 key 不存在时写入。
- 修改成功后，在数据库提交之后无条件 `SET` 新值。如果修改后的链接已经过期（只改了 URL，原 `expires_at` 已过），写负缓存而不是正缓存。
- 删除成功后，在数据库提交之后无条件 `SET` 负缓存（JSON `null`，1 分钟 TTL）。
- 这样，修改/删除之前已经读到旧值、之后才写缓存的在途请求，`SET NX` 会失败，不会把旧值盖回去；管理写用无条件 `SET`，覆盖回源或旧缓存。不需要额外的 `DEL`。
- `writeCache` 要显式区分回源（`SET NX`）和管理写（无条件 `SET`）两种模式，不能继续只按 value 是否为 nil 选择 NX：删除写的是负缓存，但必须无条件覆盖旧正缓存。
- 修改/删除的 Redis 写失败只影响一致性：旧值可能保留到正缓存 TTL（最多 1 小时），删除的负缓存最多 1 分钟；记现有 `shortlink_cache_writes_total{result="error"}` 和日志，下一次写路径会自愈。
- 管理写的缓存更新发生在数据库提交之后，不使用请求的 ctx。客户端断开或请求超时不能跳过这次 `SET`，否则删除的链接会继续跳转到正缓存 TTL 结束。写入使用 `context.WithoutCancel` 再加 1 秒独立超时；失败同样记日志和 `error`。回源填充仍是 `SET NX`，调用方取消不记成写失败。
- `shortlink_cache_writes_total{result="skipped"}` 的含义扩展为：`SET NX` 没写进去，说明已经有更新的值。指标 help 和 `docs/api.md` 要同步。
- 并发修改同一条链接时，数据库按提交顺序，缓存 `SET` 的顺序可能与提交顺序不同，缓存可能短暂落后；TTL 兜底。管理写低频，先接受；需要更强一致性时用版本号加 Lua 比较。
- 先提交数据库，再维护 Redis；不要先删缓存再改数据库，否则中间窗口的请求会回源读到旧值并重新写回缓存。

### 修改/删除和访问并发

- 数据库提交是分界点。跳转是普通 `SELECT`，修改是 `UPDATE`；PostgreSQL MVCC 下两者不互相阻塞，读到的要么是旧行，要么是新行，不会读到改了一半的行。
- 缓存命中会放大这个窗口：修改提交前已经命中旧缓存的请求按旧 URL 跳一次；提交后、缓存 `SET` 之前的请求也可能命中旧缓存。
- 修改前已经回源读到旧行、修改后才写缓存的在途请求，`SET NX` 会失败，不会把旧值盖回去。
- `PATCH` 返回 200 时，如果 Redis 写成功，之后开始的请求看到新值；已经在途的请求仍可能返回旧值。Redis 写失败时，旧值保留到 TTL，数据库已经是新值，下一次写路径自愈。
- 删除同理，并且不是强撤销：已经在途的跳转仍可能返回 302，删除后的新请求才会 404。
- 点击统计按链接 ID 记录，和 URL/过期时间是否刚改过无关；一次跳转只算一次。
- 两个管理员同时 `PATCH` 同一条链接时，同一字段按提交顺序最后写入者生效；不同字段都会保留，因为 UPDATE 只改请求里出现的字段。当前不做 `If-Match`/版本号，同一字段的并发覆盖先接受，需要时再加乐观锁。
- 测试要覆盖回源 `SET NX` 和管理写 `SET` 的交错：旧回源不能盖掉更新的新值或删除的负缓存。

### 接口拆分

- `httpapi.LinkStore` 保持 `Create` + `GetByCode`，给 redirector 和 console 的创建接口用。
- 新增 `httpapi.LinkAdminStore`：`List`、`GetForTenant`、`Update`、`Delete`，给 console 的管理 handler 用。
- 新增 `httpapi.LinkStatsStore`：`Stats`，给统计接口用（[ADR 0012](0012-click-analytics.md)）。
- 新增组合接口 `httpapi.ConsoleStore`（嵌入上面三个），`NewConsole` 接收它；`Server` 里分别保存 `store`、`admin`、`stats` 三个字段。`NewRedirector` 只接收 `LinkStore`。
- `cache.Store`、`storemetrics.Store` 是透明装饰器，需要透传完整能力，各自定义一个包含这七个方法的 `LinkStore` 接口；`postgres.Store` 隐式满足。`memorystore.Store` 只实现 `LinkStore`。
- `cache.Store` 的管理方法：
  - `List`、`GetForTenant`、`Stats` 直接透传，不读也不写缓存。
  - `Update`、`Delete` 调用下游后在 Redis 维护缓存。
- `storemetrics` 增加操作 label：`list`、`get_tenant`、`update`、`delete`、`stats`；结果分类沿用 `success`、`not_found`、`canceled`、`timeout`、`error`。

### 响应表示

管理接口统一返回：

```json
{"code":"AbC123xy","url":"https://example.com","expires_at":"2026-12-01T00:00:00Z","created_at":"2026-11-15T10:00:00Z","clicks":42,"clicks_today":3}
```

- `expires_at` 为空时省略，和创建接口一致。
- 创建接口增加 `created_at`、`clicks` 和 `clicks_today`（新增字段，不破坏兼容）。
- 列表项使用同样的对象，外层是 `items`。
- 修改返回 200 加对象；删除 204；详情 200。
- 错误格式沿用 `{"error":"..."}`。

## 后果

- 管理 API 从 1 个接口扩到 5 个，并支持三种排序；列表和详情能看到过期链接，跳转仍然只看有效链接。
- 缓存多了一条管理写入路径。回源 `SET NX` 加管理写无条件 `SET` 保证在途旧值盖不回去；但 Redis 写失败或并发修改时，缓存可能短暂落后到 TTL，这是本决策接受的最终一致。
- 硬删除会真正删掉链接和统计；删除后不能恢复，短码理论上可被随机重新生成。需要审计或短码永不复用时改软删除或墓碑表。回收站和分组不在这期；回收站需要软删除，分组需要新的表和 API，分别单独决策。
- `(tenant_id, id)` 索引服务默认排序；历史排序新增 `(tenant_id, clicks DESC, id DESC)`，今日排序用 ADR 0012 定义的 `link_clicks_daily (tenant_id, day, clicks DESC, link_id DESC)`。详情、修改、删除按 `code` 查，`code` 唯一索引已经存在。
- 排序字段会随点击变化，`sort=clicks` 和 `sort=clicks_today` 的翻页可能重复或漏项；默认 `sort=id` 保持稳定。看板只需要 Top N 时优先用专门的 top 接口，不依赖游标。
- 管理接口的低频写会经过 storemetrics 和 HTTP 指标；缓存指标里的 `skipped` 不再只表示负缓存冲突，排障时要看新的含义。
- 测试要覆盖：跨租户 404、过期链接管理可见、三种排序和游标、PATCH 的四种组合、删除和统计级联、缓存 `SET NX` 防回写、修改/删除与在途回源的交错。
- 2026-11-18：管理写乱序窗口里「不可跳转的链接留在正缓存」这一半由 ADR 0016 的复查解决：写完正缓存后再读一次库，读不到就补写负缓存。两个可跳转值之间（比如两次改 URL）的乱序仍然是本 ADR 接受的窗口，要靠版本号解决。
- 文档：`docs/api.md`、`docs/architecture.md`、`docs/roadmap.md`、`docs/runbook.md` 都要更新；`make smoke` 增加修改、删除和排序路径。

## 备选方案

- **PUT 全量替换**：语义简单，但只改 URL 也要带全量字段，漏传会把过期时间清掉；选择 PATCH。
- **软删除**：保留审计和统计，但所有查询都要过滤 `deleted_at`，短码继续占用，唯一约束和清理更复杂；当前不需要审计，选择硬删除。回收站需要这套语义，留到后续课。
- **OFFSET 分页**：实现简单，但深页扫描和翻页重复/漏项；ADR 0010 已经选了 id 游标。
- **今日排序查询时聚合小时/日表**：不增加表，但跨租户排序要扫所有链接当天的行；选择维护 `link_clicks_daily` 和租户/日期索引。
- **`links.clicks_today` 计数器**：今日排序看起来最简单，但跨天清零要额外任务，清零失败会污染排序；不选。
- **修改后只 DEL 缓存**：在途回源仍可能在 DEL 之后写回旧值；选择无条件 `SET` 加回源 `SET NX`。
- **先删缓存再写数据库**：中间窗口会回源旧值并写回；选择先提交数据库，再维护缓存。

## 参考

- `internal/httpapi/routes.go`、`internal/httpapi/createlink.go`、`internal/httpapi/auth.go`
- `internal/cache/store.go`：`Create`、`GetByCode`、`writeCache`
- `internal/postgres/store.go`：`Create`、`GetByCode`
- [ADR 0009](0009-process-split-and-credentials.md)、[ADR 0010](0010-app-level-referential-integrity.md)
- [ADR 0012](0012-click-analytics.md)：删除链接时一并删除统计
- [路线图阶段 7](../roadmap.md)
