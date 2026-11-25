# 0028. PostgreSQL 行级安全（RLS）作为租户隔离第二道防线

- 状态：Accepted
- 日期：2026-11-25
- 相关：`0009`, `0010`, `0018`, `0020`, `0021`, `0022`, `0023`, `0024`, `0025`, `0026`, `0027`

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 防线定位 | 作为“第二道防线”兜底：PostgreSQL 引擎强制按行隔离，同时保留应用 SQL 的 `WHERE tenant_id = $1` 作为第一道防线 |
| 开启 RLS 的表（7 张） | `links`、`api_keys`、`memberships`、`invitations`、`audit_logs`（范围分区父表）、`webhooks`、`webhook_deliveries` |
| 非 RLS 表 | `users`、`user_identities`、`sessions`、`tenants`、`jobs`、`link_clicks_daily`、`link_dimension_daily`、`link_visitors_daily` |
| 受限执行角色 | 迁移 `016` 幂等创建 `NOLOGIN` 角色 `shortlink_tenant`（`NOSUPERUSER NOBYPASSRLS`，非表属主）并授权给当前连接角色；对 7 张表启用 `ENABLE ROW LEVEL SECURITY`（不启用 `FORCE`，避免阻塞托管库非超管表属主在白名单跨租户路径的查询） |
| 策略写法 | `FOR ALL TO shortlink_tenant`，`USING`/`WITH CHECK` 仅比较 `tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint`，零 `OR` 旁路、默认 fail-closed |
| Go 事务装配 | `internal/postgres/tenanttx.go` 统一封装 `withTenantTx` / `withTenantReadTx`，事务开头执行 `SET LOCAL ROLE shortlink_tenant` 与参数化 `set_config('app.tenant_id', $1, true)` |
| 跨租户白名单 | 保持 `internal/pgredirect`（公开跳转/点击）、`worker` 全局调度与清理/分区 DDL、平台 CLI 与 5 条身份认证/建组织查询不走租户 RLS 事务 |

---

## 背景

当前仓库已经在 HTTP 鉴权与 `internal/postgres` 所有查询语句中按 `WHERE tenant_id = $1` 实现多租户隔离（[ADR 0009](0009-process-split-and-credentials.md)、[ADR 0018](0018-users-orgs-and-rbac.md)），但在后续迭代多 CTE 写语句、后台回表逻辑或新增管理查询时，仅靠手工拼条件仍存在“某一条 SQL 漏写 `tenant_id`”的结构性风险：

1. **防御单点化**：应用层哪怕只在一条列表、更新或删除 SQL 漏写 `tenant_id`，就能跨组织读取、篡改或删除他人的短链、Webhook 或成员邀请。
2. **PostgreSQL 角色与连接池陷阱**：
   - 在 PostgreSQL 中，`SUPERUSER`（包括本地 Compose / CI 使用的 `shortlink` 用户）、`BYPASSRLS` 角色以及未受强制约束的表属主默认绕过 RLS 策略，只有当事务显式 `SET LOCAL ROLE` 切换到受限非属主、`NOBYPASSRLS` 角色 `shortlink_tenant` 时，RLS 才会真正生效；
   - Go 使用 `*sql.DB` 共享连接池（类似 Java HikariCP），若使用会话级 `SET` 设置租户上下文，连接归还连接池后会把上一请求的 `tenant_id` 泄漏给下一条无关请求。

对照 [路线图](../roadmap.md) 阶段 16A，需要以最小侵入与零回归性能的方式落地 PostgreSQL 行级安全作为第二道防线。

---

## 决策

### 1. 受保护表范围与显式白名单

#### 1.1 启用 RLS 的 7 张租户表

全部带有强制 `tenant_id NOT NULL` 的租户业务与控制面表启用 RLS：

1. `links`
2. `api_keys`
3. `memberships`
4. `invitations`
5. `audit_logs`（迁移 `013` 转为 `RANGE (created_at)` 分区父表；在父表启用 RLS 与 `POLICY` 会自动覆罩全部既有月分区、`audit_logs_default` 及后续 `EnsureMonthlyPartitions` 动态新创建的月分区）
6. `webhooks`
7. `webhook_deliveries`

#### 1.2 明确不开启 RLS 的表及原因

- `users`、`user_identities`、`sessions`：平台级用户账号与服务端会话，一名用户可加入多个组织，无 `tenant_id` 列；
- `tenants`：组织自身主表，建组织与按 `key_hash` / `X-Org-ID` 鉴权前需要按主键创建或检索；
- `jobs`：`tenant_id` 可空（含全局清理或系统任务），且 `console worker` 需要跨租户抢占（`FOR UPDATE SKIP LOCKED`）、回收过期租约与按期清理；
- `link_clicks_daily`、`link_dimension_daily`、`link_visitors_daily`：没有 `tenant_id` 列。`Stats()` 事务内第一步先查询受 RLS 保护的 `links` 表校验归属（查不到直接返回 `ErrNotFound`），同时避免在 `cmd/redirector` 高频点击批量写出热路径上附加权限上下文开销。

### 2. 受限角色 `shortlink_tenant` 与最小权限集

迁移 [`016_tenant_rls.up.sql`](../../migrations/016_tenant_rls.up.sql) 采用“事务内降权”模型，既无需更改本地 Compose / CI / 测试脚本里的 `DATABASE_URL` 连接账号，也能彻底杜绝超管或表 Owner 绕过：

1. 幂等创建实例级无登录角色：
   ```sql
   CREATE ROLE shortlink_tenant
       NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
   ```
   并执行 `GRANT shortlink_tenant TO CURRENT_USER`，兼容本地超管连接与生产托管库（如 AWS RDS / Cloud SQL 等非超管账号，只要自身对表具备权限并属于该角色即可降权）。
2. 对 7 张租户表全部开启：
   - `ALTER TABLE <table> ENABLE ROW LEVEL SECURITY;`
   - **刻意不启用 `FORCE ROW LEVEL SECURITY`**：`shortlink_tenant` 既不是 `SUPERUSER` 也不是表属主且为 `NOBYPASSRLS`，仅 `ENABLE ROW LEVEL SECURITY` 即对其 100% 强制生效；若叠加 `FORCE`，当托管 PostgreSQL（如 AWS RDS / Cloud SQL / Neon）使用非超管表属主作为连接用户时，白名单跨租户路径（`GetByCode`、`Authenticate`、`GetSessionInfo`、`worker` 与 CLI）因为不设置 `app.tenant_id` 会被误拦导致全站跳转 404、鉴权 401 与后台任务失效。
3. 最小表/列授权（Least Privilege）：
   - `GRANT SELECT, INSERT, UPDATE, DELETE ON links, api_keys, memberships, invitations, webhooks TO shortlink_tenant;`
   - `GRANT SELECT, INSERT ON audit_logs, webhook_deliveries TO shortlink_tenant;`（仅追加，禁止租户事务 `UPDATE`/`DELETE` 篡改历史审计与投递明细）
   - `GRANT SELECT, UPDATE ON tenants TO shortlink_tenant;`（供 `Create` 扣减月度配额、`CreateWebhook` `SELECT ... FOR UPDATE` 锁行及多处租户存在性 `SELECT` 校验）
   - `GRANT SELECT ON users, link_clicks_daily, link_dimension_daily, link_visitors_daily TO shortlink_tenant;`
   - `GRANT INSERT, SELECT (id, kind, dedup_key, status) ON jobs TO shortlink_tenant;`（允许 Outbox 入队、`ON CONFLICT ... WHERE status IN ('pending', 'running')` 局部索引去重及 `RETURNING id`，但不允许读取其他任务的 `payload`）
   - **刻意不授予** `sessions`、`user_identities` 的任何权限，也不授予任何 DDL 权限。
4. **`down` 迁移不执行 `DROP ROLE`**：PostgreSQL 角色属于整个数据库实例集群（cluster-wide），本地开发环境往往在同一实例上同时存在开发库 `shortlink` 与测试库 `shortlink_test`。`016_tenant_rls.down.sql` 负责删策略、关闭 RLS 并 `REVOKE` 当前库的表与 schema 权限，刻意保留角色定义本身，确保 `prepare-test-db.sh` 的 `up -> down -> up` 验证不会因为开发库授权而失败。

### 3. 零旁路后门的 Fail-Closed RLS 策略

7 张表均绑定单条显式限定给 `TO shortlink_tenant` 的策略：

```sql
CREATE POLICY tenant_isolation ON <table>
    FOR ALL
    TO shortlink_tenant
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);
```

设计约束：

- **禁止 `OR current_setting('app.bypass_rls', true) = 'on'` 类旁路开关**：`OR` 表达式会阻止 PostgreSQL 优化器把条件稳定下推到 `(tenant_id, ...)` 联合索引，极易导致大表退化全表扫描（违反本仓库 SQL 约束）；平台后台/CLI 本就不切换 `shortlink_tenant` 角色，不需要策略里的旁路分支。
- **天然 Fail-Closed**：当进入 `shortlink_tenant` 角色但尚未设置 `app.tenant_id` 或为空字符串时，`NULLIF(..., '')::bigint` 求值为 `NULL`：
  - `SELECT` / `UPDATE` / `DELETE` 的 `tenant_id = NULL` 恒为未知/假，固定返回 **0 行**；
  - `INSERT` / 修改 `tenant_id` 的 `WITH CHECK` 不满足条件，直接抛出 **`SQLSTATE 42501`（insufficient_privilege）**。

### 4. Go 事务封装：`withTenantTx` 与 `withTenantReadTx`

对照 Java 生态中常见的 Hibernate `@Filter`（仍在应用层改写 SQL）或 Spring 事务切面 + `ThreadLocal`：Go 无隐式 `ThreadLocal` 与代理切片，且 `*sql.DB` 面向多 goroutine 复用物理连接。因此租户上下文必须在**显式开启的数据库事务边界内**通过 `SET LOCAL` 下发：

1. 在 [`internal/postgres/tenanttx.go`](../../internal/postgres/tenanttx.go) 提供两个唯一入口：
   - `withTenantTx(ctx, tenantID, opts, fn)`：校验 `tenantID > 0`，开启事务后依次执行：
     - `SET LOCAL ROLE shortlink_tenant`
     - `SELECT set_config('app.tenant_id', $1, true)`（第三个参数 `is_local = true` 确保事务结束自动失效，且参数化防拼接）
   - `withTenantReadTx(ctx, tenantID, fn)`：以 `&sql.TxOptions{ReadOnly: true}` 调用 `withTenantTx`（已在 PostgreSQL 16.15 实测确认只读事务完全允许事务级 `SET LOCAL ROLE` 与 `set_config(..., true)`）。
2. **`internal/postgres` 所有租户作用域方法全覆盖**：
   - `Create`（每次短码冲突重试各自独立 `withTenantTx`，避免 `23505` 中止外层事务）、`GetForTenant`、`List` / `listByClicksToday`、`Update`、`Delete`、`Stats`（`REPEATABLE READ` + `ReadOnly`）、`IssueAPIKey`、`ListAPIKeys`、`RevokeAPIKeyForTenant`、`ListMembers`、`UpdateMemberRole`、`RemoveMember`、`CreateInvitation`、`ListInvitations`、`RevokeInvitation`、`ListAuditLogs`、`ListWebhooks`、`CreateWebhook`、`UpdateWebhook`、`DeleteWebhook`、`RotateWebhookSecret`、`EnqueueWebhookTest`、`ListWebhookDeliveries`、`RecordWebhookDelivery`。
   - 原 SQL 中的 **`WHERE tenant_id = $1` 全部保留**，构成“应用显式条件 + PostgreSQL 引擎强制 RLS”双保险，同时确保既有索引统计信息与执行计划清晰稳定。
   - **新租户自举处理**：`IssueAPIKey` 传入 `TenantID == 0`（为新租户建租户并签首把 Key）时，先在主连接角色下插入 `tenants` 行获得新 `tn.ID`，随即在同一事务内调用 `applyTenantRLSContext(ctx, tx, tn.ID)` 降权后写入 `api_keys` 与 `audit_logs`。

### 5. 跨租户白名单（显式不走 `withTenantTx` 的路径）

以下路径按业务语义或进程边界设计为跨租户，保持使用连接原生角色并继续由应用层和进程隔离守护：

1. **`cmd/redirector`（`internal/pgredirect`）**：`GetByCode` 按全球唯一短码跳转，`RecordClicks` 异步批写；不增设事务与 `SET LOCAL` 往返开销（[ADR 0024](0024-process-boundary-and-ops-isolation.md)）。
2. **`console worker` 跨租户后台作业与治理**：`Claim` / `Complete` / `Retry` / `Dead` / `Reap`、`Cleanup*`、`PurgeLinkStatsBatch`、`EnsureMonthlyPartitions` / `DropExpired*Partitions`、`BanIfURL`、`GetWebhookForDelivery`（由 `jobs.payload.webhook_id` 查出归属 `tenant_id`）、`GetInvitationEmailTarget`；而 `RecordWebhookDelivery` 已持有 `tenant_id`，照常走 `withTenantTx`。
3. **平台运维 CLI**：`Ban` / `Unban`、`GetTenantLimits` / `SetTenantLimits`、`RevokeAPIKey`（按主键 `key_id` 吊销）。
4. **`console serve` 身份认证与跨组织入口**：
   - `Authenticate`（根据 `key_hash` 反查未知 `tenant_id`）
   - `AuthenticateSessionForOrg`（按 `sl_session` + `X-Org-ID` 核验成员关系并得出角色）
   - `GetSessionInfo`（`/api/v1/me` 列出当前用户加入的全部组织）
   - `CreateSessionForIdentity`（登录时跨组织兑现该已验证邮箱的全部待兑现邀请）
   - `CreateOrgForUser`（跨组织校验当前用户作为 `owner` 的 10 个组织上限，建组织并写 `memberships` 与 `audit_logs`）

---

## 不做什么

- **不在业务 `DATABASE_URL` 上拆分独立双连接池或强制切换无权限登录账号**：优先采用单一连接池 + 事务内 `SET LOCAL ROLE shortlink_tenant`；既避免连接池翻倍与迁移兼容成本，又随时允许在生产通过给连接用户降权+授予 `shortlink_tenant` 进一步收敛静态权限。
- **不对 `link_clicks_daily` / `link_dimension_daily` / `link_visitors_daily` 反范式冗余 `tenant_id` 列**：避免给点击聚合写入增加存储和热路径索引成本。
- **不将跨租户后台 `jobs`、风控 `BanIfURL` 或 `pgredirect` 强塞进单租户 RLS 上下文**。

---

## 怎么验证

1. **迁移可逆性（迁移 `016`）**：`./scripts/prepare-test-db.sh` 验证 `16 -> none -> 16` 无错；`cmd/migrate/main_test.go` 同步更新版本数到 16。
2. **渗透越权测试（故意漏写 `WHERE tenant_id`）**：在 `withTenantTx(tenantA)` 内对 7 张开启 RLS 的表执行无租户条件的裸 `SELECT` / `UPDATE` / `DELETE` / `INSERT`（含试图写入或篡改归属为 `tenantB`），验证读不可见、改删影响 0 行、跨租户写与篡改不可变表均返回 `SQLSTATE 42501`。
3. **Fail-Closed 测试**：切换 `shortlink_tenant` 但未设或设空 `app.tenant_id`，确认 7 张表读取恒为 0 行、写入恒报 `42501`。
4. **分区表继承测试**：在测试库动态通过 `EnsureMonthlyPartitions` 创建未来月分区并写入当前月、未来月与 `audit_logs_default` 后，确认 `withTenantReadTx(tenantA)` 跨分区只返回 `tenantA` 的行。
5. **连接池隔离与无泄漏测试**：使用 `MaxOpenConns=1, MaxIdleConns=1` 单物理连接池交替执行 `withTenantReadTx(A)`、`withTenantReadTx(B)`、回滚 `withTenantTx(A)` 及普通 `db.QueryRow`，断言每次事务结束或回滚后 `current_user` 与 `app.tenant_id` 立即还原，绝无跨请求污染。
6. **`EXPLAIN (ANALYZE, BUFFERS)` 索引不退化验证**：在受限角色 `withTenantReadTx` 下实测 `links`、`audit_logs`、`webhooks`、`webhook_deliveries` 与 `memberships` 核心查询仍命中原有索引，无 `Seq Scan` 退化。
7. **全量自检**：`make selfcheck`（含全量单元、集成、冒烟）与 `arch-check` 全绿。

---

## 后果

- **收益**：
  - 所有管理面与租户写入路径获得 PostgreSQL 引擎级行隔离兜底，彻底防止未来新增 SQL 漏加 `WHERE tenant_id` 导致的跨租户泄露或越权写；
  - `audit_logs` 与 `webhook_deliveries` 在数据库权限层面达到租户事务不可篡改/不可删除（append-only），同时禁止租户路径读取 `sessions` 与 `user_identities`；
  - `audit_logs` 按月分区继承 RLS 自动生效，无需改写 `partition.go` 的自动建表流程。
- **代价与运维注意**：
  - 所有接入 RLS 的查询都在事务内执行，相比原先单条 `QueryRow` 多 2 条本地轻语句（`BEGIN` + `SET LOCAL` 与 `COMMIT`），仅作用于管理面低 QPS 路径，不影响 `redirector` 零事务热路径；
  - 后续若新增含 `tenant_id` 的业务表或新 `Store` 方法，必须遵循同样规范纳入迁移策略与 `withTenantTx` / `withTenantReadTx`；
  - 若生产环境需要进一步收敛静态连接权限、去掉表属主和 DDL 能力，建议运维侧另建非超管、非表属主的应用连接账号（`NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT BYPASSRLS`）并 `GRANT shortlink_tenant`，使白名单跨租户路径正常工作，同时所有 `withTenantTx` 进入事务后通过 `SET LOCAL ROLE shortlink_tenant` 自动剥夺 `BYPASSRLS` 并强制执行 RLS（详见 [runbook](../runbook.md)）。
