# 0015. 链接配额按月计创建数，删除不返还

- 状态：Accepted
- 日期：2026-11-18

## 背景

[ADR 0014](0014-per-tenant-rate-limit-and-quota.md) 把链接配额定义成「当前拥有的链接数上限」：创建加一，删除减一。这个口径限制的是存量，有三个问题：

- 删除能换回名额。创建、用完、删除、再创建，一个名额可以反复用，配额限制不了租户实际消耗的创建量。配额是给租户的额度，用掉就是用掉了，不该因为事后删除再回来。
- 和封禁冲突。[ADR 0016](0016-link-states-and-takedown.md) 让平台可以封禁违规链接。按存量计的话，违规链接删掉就能拿回名额，违规没有成本。
- 没有时间维度。租户用满之后只能删旧链接腾位置，而短链是已经发出去的地址，删掉就失效。

按月计新建数、到期重置是这类产品常见的额度口径，Bitly 和 Dub 的套餐都按每月新建链接数计。

现有约束：

- `tenants` 和 `links` 之间没有外键，计数和判断要自己保证原子（[ADR 0010](0010-app-level-referential-integrity.md)）。0014 用一条语句同时占配额和插入，这一点保留。
- console 可以多副本，各副本的系统时钟不保证一致。项目里没有定时任务，也没有哪个进程保证在月初活着。
- 没有套餐、账期和租户时区。点击统计的「天」已经统一按 UTC（`link.DayStart`）。
- 删除是硬删除（[ADR 0011](0011-link-crud-api.md)），删掉的链接在库里不留痕迹。

## 决策

### 口径

- `link_quota` 的含义改为：一个 UTC 自然月内最多能创建的链接数。默认值仍是 1000，`0` 仍表示不能创建。
- 计的是「创建成功」这个动作。链接之后过期、停用、被封禁或被删除，都不改变已用量。
- 每月 1 日 00:00 UTC 起已用量重新从 0 计，没用完的额度不结转。
- 拥有的链接总数不再有上限（见「后果」）。
- HTTP 契约不变：超出配额返回 403 和 `{"error":"link quota exceeded"}`。

### 表结构

迁移 005 改 `tenants`：

| 列 | 变化 | 含义 |
| --- | --- | --- |
| `link_usage` | 由 `link_count` 改名，约束 `tenants_link_count_nonnegative` 跟着改名 | `link_usage_month` 那个月已创建的链接数 |
| `link_usage_month` | 新增，`DATE NOT NULL DEFAULT '1970-01-01'` | `link_usage` 属于哪个月，存当月 1 日（UTC） |
| `link_quota` | 列不变，只改注释 | 每月上限 |

- 改名而不是沿用 `link_count`：含义从存量变成了当月用量，留着旧名字，读代码和查库的人会按存量理解。
- `link_usage_month` 的默认值是一个早于任何真实月份的常量，表示「还没用过」。不用可空列，比较时就不用处理 NULL。CHECK 约束 `EXTRACT(DAY FROM link_usage_month) = 1` 只允许每月 1 日。
- 回填：`link_usage` 取该租户本月创建（按 `links.created_at`）且还在的链接数，`link_usage_month` 取当前月。本月创建后又删掉的链接数不回来，这一次少算对租户有利，接受。
- down 迁移删掉 `link_usage_month`，列名和约束名改回去，再按 `links` 重新数一遍，回到 0014 的存量口径。

### 跨月重置放在创建语句里

不起定时任务。先定义「有效已用量」：记录的月份早于当前月，说明这个数是以前某个月留下的，按 0 算。

```sql
CASE WHEN link_usage_month < date_trunc('month', now() AT TIME ZONE 'UTC')::date
     THEN 0 ELSE link_usage END
```

创建语句在 0014 的基础上只改 `quota` 这个 CTE：

```sql
WITH quota AS (
    UPDATE tenants
    SET link_usage = (CASE WHEN link_usage_month < date_trunc('month', now() AT TIME ZONE 'UTC')::date
                           THEN 0 ELSE link_usage END) + 1,
        link_usage_month = GREATEST(link_usage_month, date_trunc('month', now() AT TIME ZONE 'UTC')::date)
    WHERE id = $4
      AND (CASE WHEN link_usage_month < date_trunc('month', now() AT TIME ZONE 'UTC')::date
                THEN 0 ELSE link_usage END) < link_quota
    RETURNING 1
)
INSERT INTO links (code, url, expires_at, tenant_id)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM quota)
   OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = $4)
RETURNING id, created_at
```

- 月份取数据库时钟，和跳转查询判断过期用的是同一个时钟。console 多副本时，不会因为各自的系统时间不同而对「现在是几月」有分歧。
- `now() AT TIME ZONE 'UTC'` 先把时刻换成 UTC 的墙上时间再截断，结果不依赖会话时区。直接写 `date_trunc('month', now())::date` 会按会话时区取月份。
- `GREATEST` 让月份只前进不后退。数据库时钟被往回拨，或故障切换到时钟落后的实例时，已经记到新月份的用量不会被当成旧月份再清一次。
- 重置和加一在同一个 `UPDATE` 里。月初并发的创建排在租户行锁上：第一个把用量置 1 并写入新月份，后面的拿到锁后按更新后的行重新求值，接着往上加。
- 短码冲突时整条语句回滚，重置和加一都不留痕，重试不会多计。
- 没有返回行时补查一次「有效已用量 >= `link_quota`」，区分配额满和租户不存在。补查结果是没满时重试创建：两条语句之间配额可能被调高，也可能正好跨月。0014 里这个分支的理由是「并发删除刚释放了名额」，这个理由不再成立，分支保留。
- 租户行不存在时照常插入、不计数，和 0014 相同（ADR 0010 允许的孤儿引用）。
- 所有读用量的地方用同一个表达式：`tenant show`、`tenant set` 的回显、配额满的补查。直接读 `link_usage` 列，会在跨月后、第一次创建前看到上个月的数。Go 里把「当前月」和「有效已用量」各定义成一个 SQL 片段常量，拼进用到它们的语句，避免几处各写一份。

### 删除不再更新租户行

`Delete` 去掉事务最后一步的 `UPDATE tenants SET link_count = GREATEST(link_count - 1, 0)`。删除事务只剩链接行和三张统计表，不再和同租户的创建争租户行锁。

### 运维入口

- `console tenant show` 的输出列 `link_count` 改成 `link_usage`，值是有效已用量。`tenant set --link-quota` 的含义改为每月上限。
- 把 `link_quota` 调到本月已用量以下：本月不能再创建，下月恢复。
- 不提供清零本月用量的命令。需要临时放宽时调高 `link_quota`。

## 不做什么

- 不按租户的账期或时区重置。没有套餐、计费和租户时区设置，统一 UTC 自然月。有了账期再把 `link_usage_month` 换成账期起点。
- 不起定时任务清零。懒重置不依赖任何进程在月初活着；代价是读用量必须用同一个表达式。
- 不限制拥有的链接总数。要存量上限时另加一列，和本月用量分开计。
- 不保留历史月份的用量。跨月后旧值被覆盖；要按月出账单时再加用量流水表。
- 不做额度结转、加量包和按接口区分的额度。
- 不在 403 响应里返回重置时间。规则只有一条（每月 1 日 00:00 UTC），写在 API 文档里。
- 不做用量对账。删除是硬删除，本月用量无法再从 `links` 推算（见「后果」）。

## 怎么验证

- `internal/postgres` 集成测试（没有 `TEST_DATABASE_URL` 时跳过）：
  - 月份过期且旧用量等于配额时创建成功，用量变 1，月份变成当前月。
  - 本月用满时返回 `ErrQuotaExceeded`；`link_quota = 0` 时同样返回，并且租户行没有被改写。
  - 记录的月份晚于当前月时不重置，照常加一，月份不变。
  - 删除之后用量不变，用满的租户删除一条链接后仍然不能创建。
  - 20 个并发创建撞一个月份过期、配额为 5 的租户：成功正好 5 个，`links` 行数和 `link_usage` 都是 5。
  - `GetTenantLimits` 在月份过期时返回用量 0。
- `make smoke`：把冒烟租户的配额压到本月用量，创建返回 403；删除一条链接后创建仍然是 403（这一步原来期望 201）；把 `link_usage_month` 改成上个月后创建返回 201。
- `make selfcheck` 的迁移 up、down、up 覆盖 005 的回滚。
- 2026-11-18：草稿 SQL 在一次性的 PostgreSQL 17 实例上手工跑过，结果和上面的预期一致。另外确认了：回填按 UTC 月界计数（月初零点整创建的算本月，早 1 微秒的不算）；会话时区设成 `America/New_York` 时月份仍按 UTC 取；短码冲突时用量和月份一起回滚；20 个会话各自拿到行锁后保持 0.15 秒再提交，成功的仍然正好 5 个；down 之后计数回到存量。

## 后果

- [ADR 0014](0014-per-tenant-rate-limit-and-quota.md) 里配额的口径被本 ADR 取代：`link_count` 的定义、删除释放配额、删除事务最后更新租户行、按 `links` 重算计数。限流部分和「在创建语句里原子占配额」的做法不变。本 ADR 采纳时在 0014 的「后果」里加一行指向这里，0014 的状态保持 Accepted。
- `link_usage` 从派生数据变成了唯一记录。删除是硬删除，本月创建过多少条不能再从 `links` 数出来；runbook 里按 `links` 重算的 SQL 要删掉。绕过应用直接往 `links` 插行不会计入用量。
- 存储增长不再有硬上限。0014 用存量上限防「一个租户的脚本写满数据库」，现在变成速度上限：每个租户每月最多 `link_quota` 行。
- 迁移 005 改了列名，和旧版本 console 不兼容。迁移之后、新版本上线之前，旧 console 的创建、删除和 `tenant show/set` 会因为找不到 `link_count` 失败；鉴权、列表、详情、修改和跳转不受影响。发布顺序是迁移后立即换 console，接受这段窗口。要零停机时拆成加新列、双写、删旧列三次发布，现在没有这个需求。
- 回填少算了本月已删除的链接；之前按存量被卡住的租户，迁移后本月立刻有额度。
- 同租户的创建仍然串行在租户行上，删除不再参与这把锁。
- 需要同步的文档和注释：`docs/api.md`（配额说明、403 之后怎么办）、`docs/architecture.md`、`docs/runbook.md`（「查看和调整租户配额与限流」）、`link.ErrQuotaExceeded` 和 `postgres.TenantLimits` 的注释、`console` 的用法说明。
- 2026-11-24（补充）：上文中沿用 0014 的“租户行不存在时照常插入、不计数（`OR NOT EXISTS (SELECT 1 FROM tenants ...)`）”已被 [ADR 0022](0022-postgres-query-and-lease-hardening.md) 取代：`Create` 遇到不存在的租户一律拒绝并返回 `ErrTenantNotFound`。

## 备选方案

- **存量上限，删除返还**（0014 的做法）：见「背景」。
- **累计创建数，永不重置**：只要去掉删除时的减一，不用加列。但额度用完后只能靠运维手工调高，没有周期。
- **滚动 30 天窗口**：没有月初集中重置，更平滑。但要按每条链接的创建时间现算，被硬删除的链接不在 `links` 里，得另建创建流水表。
- **定时任务月初清零**：读用量可以直接读列。但要保证任务一定执行、只执行一次，多副本下还要选主或加锁。
- **月份由应用算好传进语句**：测试可以注入假时间。但多副本各自的时钟会对月界有分歧，库里其他按时间的判断（过期）用的也是数据库时钟。
- **2026-11-24 演进补充**：后续为保留 `tenants` 表 HOT Update 优化并减少高频更新锁竞争，迁移除保持不给 `link_usage`/`link_usage_month` 建索引外，管理面修改与密钥吊销也全部收敛为单条多 CTE 原子语句；`Delete` 已将三张日统计表清理拆出主事务改为 `link.purge_stats` 异步批删（见 [ADR 0025](0025-reconcile-cache-async-purge-and-audit-hardening.md)）。
