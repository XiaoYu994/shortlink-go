# 0023. 组织操作审计日志

- 状态：Accepted
- 日期：2026-11-24

## 背景

阶段 13 已补齐自助创建组织、成员与邀请管理、组织 API Key 签发/吊销和前端控制台，此前系统也已具备异步风控封禁与 CLI 运维下架。现阶段需要为多租户 SaaS 补齐“谁在什么时候、通过什么入口、带着哪个请求编号对哪项资源做了什么变更”的可追溯轨迹：

1. **事务一致性约束**：如果审计记录在 handler 返回后另开连接异步写入，数据库提交成功但进程崩溃或审计写入超时时，业务变更已经生效却没有审计留痕（产生“幽灵变更”）；如果业务语句失败（如配额不足、风控竞态 URL 已变更、非最后 owner 检查失败或重复调用幂等接口），却写了审计行，就会留下误导性假记录。
2. **端口/接口签名稳定性**：`console.Store`、`cache.Store` 和 `storemetrics.Store` 等层已按透明装饰器模式稳定运行。为了只记录真实发生的持久变更而不破坏各装饰器签名，需要轻量的执行者上下文透传机制。
3. **跨进程统一口径**：短链、成员、邀请与 API Key 的状态变更分散在 `console serve`（会话用户 + API Key）、`console` CLI（运维操作员 `operator`）和 `console worker`（异步风控 `system`）三个入口，查询界面必须能把这四类执行主体统一展示且不泄露敏感凭证。

## 决策

### 1. 表结构与不可变审计日志模型（迁移 `011`）

新增只追加写（append-only）的 `audit_logs` 表：

- 主键：`id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`
- 租户与执行主体：`tenant_id BIGINT NOT NULL`、`actor_kind TEXT NOT NULL`（`user` / `api_key` / `operator` / `system`）、`actor_id BIGINT`（CHECK 约束：`user`/`api_key` 必为正整数，`operator`/`system` 必为 `NULL`）
- 动作与目标：`action TEXT NOT NULL`、`target_type TEXT NOT NULL`、`target_id TEXT NOT NULL`
- 关联与载荷：`request_id TEXT NOT NULL DEFAULT ''`（关联 HTTP `X-Request-ID` 或 worker `job:<id>`）、`metadata JSONB NOT NULL DEFAULT '{}'::jsonb`（强制 `jsonb_typeof(metadata) = 'object'`）、`created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
- 延续 [ADR 0010](0010-app-level-referential-integrity.md) 不建外键：成员被移出、邀请被清理、短链或 API Key 删除/吊销后，历史审计记录继续保留且可按原编号追溯。
- 双索引设计：
  - `idx_audit_logs_tenant_id ON audit_logs (tenant_id, id DESC)` 服务全部动作的游标分页；
  - `idx_audit_logs_tenant_action_id ON audit_logs (tenant_id, action, id DESC)` 服务按 `action` 筛选 + 游标分页；
  - 查询在 PostgreSQL 层显式拆为 4 条无 `OR` 谓词的 sargable 语句，确保查询计划直走对应索引、不触发全租户过滤或内存 `Sort`。

### 2. 审计动作口径（14 类关键状态变更）

| 领域 | `action` | `target_type` | `target_id` | 关键 `metadata` |
| --- | --- | --- | --- | --- |
| 组织/租户 | `org.create` | `org` | 组织 ID | `name`、可选 `external_id` |
| 组织/租户 | `tenant.limits_update` | `org` | 组织 ID | 变更前后的 `link_quota` / `rate_limit_per_second` |
| 短链 | `link.create` | `link` | `code` | `link_id`、`url`、可选 `expires_at` |
| 短链 | `link.update` | `link` | `code` | `fields`（变更字段列表）及对应新值 |
| 短链 | `link.delete` | `link` | `code` | `link_id`、`url` |
| 短链 | `link.ban` | `link` | `code` | `link_id`、`url`、`reason`、`source`（仅从非封禁变为封禁时写一次） |
| 短链 | `link.unban` | `link` | `code` | `link_id`、`url`、`previous_reason`、`previous_source`（仅从已封禁解封时写一次） |
| 成员 | `member.role_update` | `member` | 目标 `user_id` | `email`、`previous_role`、`new_role` |
| 成员 | `member.remove` | `member` | 目标 `user_id` | `previous_role` |
| 邀请 | `invitation.create` | `invitation` | 邀请 ID | `email`、`role`、可选 `expires_at` |
| 邀请 | `invitation.revoke` | `invitation` | 邀请 ID | `email`、`role` |
| 邀请 | `invitation.redeem` | `invitation` | 邀请 ID | `email`、`role` |
| API Key | `apikey.issue` | `api_key` | 密钥 ID | `scopes`、可选 `expires_at`（**严禁写明文或哈希**） |
| API Key | `apikey.revoke` | `api_key` | 密钥 ID | `{}` |

### 3. 事务内原子写入与 `internal/audit` 上下文透传

- 新增零外部依赖的领域包 `internal/audit`，提供 `Meta{ActorKind, ActorID, RequestID}`、`WithMeta` / `MetaFrom` 与约束兜底的 `NormalizeMeta`。
- 上游边界注入元数据：`console` 鉴权中间件写入 `user` 或 `api_key` 与 `httpx.RequestIDFrom(ctx)`；自助建组织与登录回调（兑现邀请）各自注入对应上下文；CLI 命令在 `run` / `runLink` 注入 `operator`；`internal/risk` 在调用 `BanIfURL` 前注入 `system` 与 `request_id = "job:<id>"`。
- `internal/postgres` 所有 14 条写路径在同一数据库事务或单条多 CTE 语句内完成业务写与 `audit_logs` 写入：写成功必留痕，冲突重试/约束失败/业务拒绝/幂等空操作（如重复 `Ban`/`Unban`）不留痕、不重复记。

### 4. 查询权限与 HTTP API

- 在 `internal/identity` 新增 `PermAuditRead = "audit:read"`，**仅授予 `RoleOwner` 与 `RoleAdmin`**；不加入 `AllAPIKeyScopes()`（数据库 `api_keys.scopes` 约束保持不动）。
- 新增 `GET /api/v1/audit-logs?action=&cursor=&limit=`：挂载在 `consoleRoute(identity.PermAuditRead, ...)` 链上，handler 内做 `p.ActorKind == identity.ActorUser` 纵深防御，响应统一附带 `Cache-Control: no-store`。
- 列表 SQL 按 `a.actor_kind = 'user'` `LEFT JOIN users` 回填 `actor_email` 与 `actor_name`，按 `a.id DESC` + `limit`（默认 20、最大 100）做 keyset 游标分页。
- 前端控制台新增只读「审计日志」页面（`web/src/features/audit/`），`owner` / `admin` 可按动作筛选与游标翻页并一键复制 `request_id`，`member` / `viewer` 显示权限提示且不触发后端请求。

## 不做什么

- **不审计只读请求、公开跳转与失败请求**：`GET /r/{code}` 跳转、管理 `GET` 查询、限流 429 与 400/401/403 等未发生状态变更的请求留给结构化访问日志和 Prometheus 指标，不写 `audit_logs`，避免高 QPS 读路径拖垮写库与膨胀表体积。
- **不在 `metadata` 中存放敏感凭证**：绝不记录 API Key 明文/哈希、会话令牌或第三方 OAuth token。
- **第一阶段不做自动清理或按月分区**：审计表在学习/中小规模阶段保持完整历史；未来数据量增长后再结合导出或按月分区（`PARTITION BY RANGE (created_at)`）+ 分批 `LIMIT` 归档，延续不直接建全局热点删改定时任务的原则。

## 怎么验证

1. 迁移 `up -> down -> up` 与 `cmd/migrate/main_test.go` 版本对（`11` 个迁移文件）全部通过。
2. `EXPLAIN (ANALYZE, BUFFERS)`（基于 100 租户、20,000 行合成 `audit_logs` + `ANALYZE audit_logs`）实测结果：
   - 首页查询 `listAuditLogsFirstSQL`：命中 `Index Scan using idx_audit_logs_tenant_id`（`Execution Time: 0.102 ms`，`shared hit=68`，无 `Sort`）；
   - 翻页查询 `listAuditLogsAfterSQL`：命中 `Index Scan using idx_audit_logs_tenant_id`，条件 `(tenant_id = 7) AND (id < 58000)`（`Execution Time: 0.115 ms`，`shared hit=69`）；
   - 动作筛选首页 `listAuditLogsFirstByActionSQL`：命中 `Index Scan using idx_audit_logs_tenant_action_id`（`Execution Time: 0.075 ms`，`shared hit=66`）；
   - 动作筛选翻页 `listAuditLogsAfterByActionSQL`：命中 `Index Scan using idx_audit_logs_tenant_action_id`，条件 `(tenant_id = 7) AND (action = 'link.ban') AND (id < 58000)`（`Execution Time: 0.113 ms`，`shared hit=66`）。
3. `internal/postgres` 集成测试覆盖全部 14 类动作的写入、失败回滚零写入、幂等重复封禁/解封不重复写入、跨租户隔离、action 筛选与游标分页。
4. `internal/console` 与 `web/` Vitest 测试覆盖 RBAC 权限边界（`owner`/`admin` 200，`member`/`viewer`/API Key 403）、参数校验 400 与 `audit.Meta` 透传。
5. `scripts/smoke.sh` 真实多进程验证 API Key 拒绝（403）、自助闭环（`org.create` / `invitation.*` / `apikey.*` / `link.create`）、CLI `link ban/unban`、OIDC `invitation.redeem`，以及异步风控 worker 自动封禁后记录 `actor_kind="system"` 与 `request_id="job:<id>"`。

## 后果

- 多租户变更与安全封禁具备与业务写同等一致性的可追溯记录，用户与排障可通过 `request_id` 把审计日志与进程日志直接串联。
- 每条写事务会多出一次窄行 `INSERT INTO audit_logs`（同语句多 CTE 或同事务追加），写入路径需保持简洁索引（当前仅 2 个复合索引）以避免写放大。
- 未来阶段 16 引入 PostgreSQL RLS 时，`audit_logs` 已具备严格 `tenant_id NOT NULL CHECK (tenant_id > 0)` 列，可直接纳入同一套租户隔离策略。
