# internal/postgres/AGENTS.md

本文件约定存储层（`internal/postgres/` 与 `migrations/`）的高危约束；通用 Go 规范见 [docs/conventions.md](../../docs/conventions.md)。

1. **租户隔离与 RLS（禁止绕过）**：
   - 所有租户业务读写必须经过 `withTenantTx` / `withTenantReadTx`（在事务内 `SET LOCAL ROLE shortlink_tenant` 并通过 `set_config('app.tenant_id', ..., true)` 绑定租户上下文），禁止直接使用 `s.db` 绕过。
   - 新增含 `tenant_id` 的租户表时，迁移必须同步授权 `shortlink_tenant`、`ENABLE ROW LEVEL SECURITY`（**不加 `FORCE`**，以免阻塞托管库属主在跨租户白名单路径的查询），并创建零 `OR` 分支的 fail-closed policy（漏设 `app.tenant_id` 时读返回 0 行、写抛错）。

2. **事务编排与外围副作用边界**：
   - 禁止把主业务表与无关业务域硬塞进同一条多表修改 CTE，也禁止通过“跳号插参”往旧 SQL 叠加新需求；多步写在事务闭包内编排单职责 `xxxTx` 小函数（仅在单表并发抢占/分批回收保留 `FOR UPDATE SKIP LOCKED` CTE）。
   - 必选审计写入复用 `insertAuditLogTx`；可选外围副作用（风控扫描入队、Webhook 扇出、缓存对账、邀请邮件、异步统计清理）一律通过 `TxHook`（`txhook.go`）挂载到事务内执行，Hook 内禁止执行外网 I/O。

3. **SQL 结构与查询性能**：
   - 多行 SQL 抽到文件顶部包级 `const`，单条 SQL 控制在 6–8 个位置参数以内且编号严格按出现顺序递增；JSON 载荷在 Go 侧结构体序列化。
   - 新增索引或改动查询/清理 SQL 后，在合成数据量上用 `EXPLAIN (ANALYZE, BUFFERS)` 确认命中预期索引；禁止用 `OR` 混合有索引列与无索引列导致全表扫描；不给高频更新计数列建索引，保留 PostgreSQL HOT Update。

4. **分批清理、租约与分区 DDL**：
   - 后台清理与租约回收 SQL 一律带 `LIMIT` 分批执行；范围分区表分批删改必须依据完整主键元组，禁止跨分区不唯一的 `ctid`。
   - 租约回写必须校验单调递增 Fencing Token（`jobs.lock_version`）。
   - 分区 DDL 必须在独立事务内设 `SET LOCAL lock_timeout = '3s'`，且绝不修改或删除 `*_default` 兜底分区。

5. **迁移可逆性**：
   - 所有迁移必须保证 `up -> down -> up` 可逆；改动后跑 `make test-db` 或 `make selfcheck` 全量重建验证。
