# 真实 SaaS 平台闭环演进与全栈 / SQL 进阶规划

本文档记录以当前短链系统为载体，进一步打磨**真实多租户 B2B SaaS 平台**、并同步夯实 **Go 后端、React 现代前端、PostgreSQL 表结构建模与 SQL 调优** 的中长期演进规划。

已有功能状态见 [roadmap.md](roadmap.md)，现行系统结构见 [architecture.md](architecture.md)；本文档聚焦**尚未建设或待补全闭环的后续演进**，每个需求都按“端到端业务闭环”组织，不按纯技术清单割裂堆砌。

---

## 一、文档定位与执行纪律

1. **业务流程端到端闭环优先**：任何一项能力开工前，必须打通完整链路——**UI 交互入口（含权限与能力开关降级） → HTTP/OpenAPI 契约 → 事务编排（RLS、审计、缓存一致性） → 异步任务与到期/过期数据清理 → 异常回滚、并发冲突与运维兜底**。绝不留“建了表没有后台清理”“做了接口没有前端入口”或“只考虑正常路径”的半成品。
2. **垂直切面演进与先立 ADR**：严格遵守仓库协作规范（见 [AGENTS.md](../AGENTS.md)、[internal/postgres/AGENTS.md](../internal/postgres/AGENTS.md)、[web/AGENTS.md](../web/AGENTS.md)）。涉及表结构、持久化状态机或对外 HTTP 契约的改动，动工前先在 [adr/](adr/README.md) 立 ADR（写明「不做什么」与明确接受的边界）；每个独立闭环完成后必须通过 `make selfcheck` 与 `make web-check` 全量验收。
3. **SQL 与 DDL 零停机、先实证后优化**：
   - 不凭感觉加索引或拆表；新增复杂查询、搜索或聚合前，先在合成规模数据上用 `EXPLAIN (ANALYZE, BUFFERS)` 与 `pg_stat_statements` 留存对比基线（沿用 [postmortems/2026-11-24-postgres-schema-and-query-audit.md](postmortems/2026-11-24-postgres-schema-and-query-audit.md) 的做法）。
   - 大表在线变更优先采用 `CREATE INDEX CONCURRENTLY`、`ADD CONSTRAINT ... NOT VALID` + `VALIDATE CONSTRAINT` 及 Expand-Contract 双写双读平滑切换，避免全表长锁拖垮线上流量。

---

## 二、当前基线与待补齐缺口总览

经过 `migrations/001`–`018` 的演进，系统已拥有 **16 张业务表**（7 张开启引擎级 RLS、4 张按月声明式分区）、三进程物理拆分（`redirector` / `console` / `worker`）、基于 `jobs` Outbox + `TxHook` 的可靠异步框架（风控扫描、Webhook 扇出与熔断、邀请邮件、分区与过期清理），以及结构规范的 React 19 + TanStack Query 前端控制台（[web/README.md](../web/README.md)）。

在此稳固底座上，对照真实商业 SaaS 与全栈/数据库工程能力，当前存在以下四类具体缺口：

| 维度 | 当前真实缺口（对照代码与迁移现状） |
| :--- | :--- |
| **业务闭环** | 1. **协作与身份断点**：[007 迁移](../migrations/007_add_users_and_rbac.up.sql) 中 `invitations.email NOT NULL` 且 [session.go](../internal/postgres/session.go) 仅在 OIDC/GitHub 登录携带 `email_verified=true` 时被动兑现，导致 [017 迁移](../migrations/017_phone_otp_challenges.up.sql) 引入的**手机号 OTP 用户无法受邀加入组织**；已登录老用户点邀请无法直接接受；缺少多登录方式主动绑定（往同一 `user_id` 下挂第二条 `user_identities`）与多设备会话管理页。<br>2. **团队短链资产属性偏薄**：`links` 缺 `title`、创建者 `created_by`（目前仅散落在 180 天保留的 `audit_logs` 中）、团队标签/分组、多人并发编辑冲突保护，以及误删容错（当前 `Delete` 直接物理删行并入队 `link.purge_stats`）。<br>3. **商业化与运营闭环缺失**：缺少自定义品牌域名（`domains`）、组织级宏观统计看板、批量 CSV 导入导出、长任务/风控结果实时推送、订阅套餐/额度包计费，以及 `tenants` 平台级冻结（目前仅有单链封禁）。 |
| **表结构与 SQL** | 1. **RLS 第二道防线不对称**：[016 迁移](../migrations/016_tenant_rls.up.sql) 给 `shortlink_tenant` 授予了 `tenants`（`SELECT, UPDATE`）与 `link_clicks_daily`（`SELECT`）权限，但这两张表未开 RLS，应用漏写条件时仍存在越权隐患。<br>2. **`audit_logs` 分区裁剪失效**：[013 迁移](../migrations/013_partition_timeseries_tables.up.sql) 按 `created_at` 月分区，但 [audit.go](../internal/postgres/audit.go) 仅按 `(tenant_id, id DESC)` 游标查询，无法触发分区裁剪，保留期拉长后扇出增加；且 `metadata` JSONB 只存不查。<br>3. **短链搜索与 HOT Update 张力**：[admin.go](../internal/postgres/admin.go) 的 `q` 搜索采用 `code ILIKE ... OR url ILIKE ...`，无索引在大租户下会全租户扫表；另 `links.clicks` 带着 `idx_links_tenant_clicks` 索引高频更新，在超高并发点击下会阻碍 `links` 的 HOT Update（现阶段保持观察，触及压测瓶颈再垂直拆分 `link_counters`）。<br>4. **待补练的高频 SQL/建模能力**：Online DDL（`CONCURRENTLY` / `NOT VALID` / Expand-Contract）、窗口函数与多维报表 SQL、`pg_trgm` GIN / JSONB 索引、乐观锁 CAS、软删部分唯一索引、多对多集合筛选、时间区间排他约束（`EXCLUDE USING gist`）、Append-Only 账本流水建模、`pg_stat_statements` 与 `pg_locks` 阻塞诊断。 |
| **后端进阶** | 1. 限流目前是单进程内存令牌桶，多副本下总量放大；缺少跨副本分布式的 Redis + Lua 原子限流与降级。<br>2. `POST /api/v1/links` 等写接口缺少 `Idempotency-Key` 幂等保障，超时重试会重复建链并多扣配额。<br>3. 全仓均为短请求，缺少 SSE 长连接推送 + Redis Pub/Sub 跨副本广播及优雅关停衔接。<br>4. [api.md](api.md) 与前端 TS 类型手写同步，缺 OpenAPI 3.1 单一契约源；缺慢依赖故障注入验证。 |
| **前端进阶** | 1. [LinksPage.tsx](../web/src/features/links/LinksPage.tsx) 的筛选、排序、搜索、视图模式停留在 `useState`，未与 URL Query 同步（刷新或详情返回后条件丢失）。<br>2. [router.tsx](../web/src/app/router.tsx) 全部页面同步 `import`，未做路由级 `React.lazy` 拆包与新版发布 chunk 404 恢复。<br>3. [links/api.ts](../web/src/features/links/api.ts) 写操作后粗暴 `invalidateQueries`，未练 TanStack Query `onMutate` 乐观更新回滚与 `prefetchQuery`。<br>4. 复杂弹窗表单缺 Zod Schema 校验与服务端字段级错误映射；缺真实浏览器 Playwright E2E、SSE 实时状态同步、大列表虚拟滚动、批量操作、`Cmd+K` 全局命令面板与深色模式。 |

---

## 三、四个垂直演进阶段（严格按端到端业务闭环设计）

建议按 **阶段 A → B → C → D** 依次推进；每个子闭环都可独立立项、编码并完整上线验收。

### 阶段 A：账号协作闭环、安全兜底与短链协作体验升级

#### 闭环 A1：全登录方式通用的邀请链接直兑闭环

* **解决的断点**：打通手机号 OTP 用户进组织的通道；消除已登录老用户必须退出重登才能兑现邀请的断层。
* **完整业务流程**：
  1. **发起**：管理员在成员页填写可选目标邮箱（或仅生成邀请链接）与角色，支持邮件邀请与复制直兑链接两种形态；仅把随机令牌的 SHA-256 `token_hash` 存入 `invitations`，明文令牌只在创建/发送时使用一次；若配置 SMTP，由 `invitation.email` 异步投递含直兑 URL 的邮件。
  2. **访问与引导**：受邀人打开 `/invitations/accept?token=...` 时，前端先调无鉴权预览接口（只返回脱敏后的组织名、目标角色、是否过期，不泄露成员列表和完整邮箱）。若当前未登录，带上完整 `return_to` 引导至 `/login`，支持使用**任意已启用的登录方式**（OIDC / GitHub / 手机号 OTP）登录并回跳。
  3. **核验与兑现**：用户登录后确认加入，服务端在事务内加行锁校验令牌有效（未兑现、未撤销、未过期、未被取代）；若邀请指定了邮箱，需核对当前账户已验证邮箱（或允许持有效直兑令牌的明确确认策略并记入审计）；原子写入 `memberships`（已是成员则返回明确幂等提示而不报错）、更新 `accepted_at` / `accepted_by`、写 `audit_logs`。
  4. **异常与清理**：覆盖过期、已撤销、已被重邀取代、令牌不存在（统一语义防枚举）及组织达上限等分支；`worker` 继续按过期/撤销/取代部分索引分批清理历史邀请。
* **表结构与 SQL 训练点**：`invitations` 放宽 `email` 可空约束、新增 `token_hash BYTEA UNIQUE`（严格校验长度 `CHECK`），并调整部分唯一索引（有邮箱时同租户同邮箱仅 1 条待兑现；无邮箱时靠令牌隔离）；重温无 `OR` 分支的多条件部分索引设计。
* **后端关键点**：安全令牌生成与单向哈希存储；与已有 `return_to` HMAC 签名机制复用防开放重定向；`Capabilities` 与邮件模板同步切换。
* **前端关键点**：新增 `/invitations/accept` 公开与登录双态落地页；完善跨 `/login` 重定向回跳体验及各类失效态友好提示。

#### 闭环 A2：个人账号设置与多身份 / 多设备会话治理闭环

* **解决的断点**：`users` 1:N `user_identities` 与 `sessions` 已具备多身份/多设备数据基础，但缺少用户自助管理和解绑防锁死闭环。
* **完整业务流程**：
  1. **账号设置入口**：控制台左上角用户菜单新增 `/settings/account`（无需 `X-Org-ID`，与 `/me` 同属用户级界面）。
  2. **多登录方式绑定与解绑**：展示当前账号已绑定的 OIDC、GitHub、手机号身份；支持在已登录态下触发“绑定新身份”（携带绑定态 state 走 OAuth 回调或手机号 OTP 验证后，向当前 `user_id` 插入新 `user_identities` 行；若该 `(issuer, subject)` 已被他人占用则明确拒绝并提示冲突，不自动合并账号）；支持解绑某身份，事务内 `FOR UPDATE` 锁 `users` 行并统计剩余身份数，**强制至少保留 1 种登录方式**，防止账号自我锁死。
  3. **多设备会话查看与踢出**：用户可查看自己的活跃登录设备列表（脱敏显示浏览器/系统摘要、最近活跃时间 `last_seen_at`、最近 IP 及“当前设备”标记）；支持单个踢出（吊销指定 `session.id`，强制校验 `user_id = 当前用户`）或“下线其他所有设备”。被踢会话在下次请求立即返回 401。
* **表结构与 SQL 训练点**：`sessions` 增加 `user_agent`、`last_ip`、`last_seen_at` 列；**`last_seen_at` 沿用 `api_keys.last_used_at` 规范，采取节流更新且严禁建索引以保护 HOT Update**。练习事务内父行加锁（`users FOR UPDATE`）防并发解绑最后一个凭据的竞态。
* **后端关键点**：扩展 `AuthMethod` 回调区分“登录建会话”与“绑定已有用户”；会话鉴权中间件增加带节流的 `TouchSession`；全部敏感操作写用户安全事件或日志。
* **前端关键点**：账号安全中心页面与响应式列表；解绑最后身份的禁用提示与二次确认 `<ConfirmDialog />`。

#### 闭环 A3：RLS 兜底闭环 + 短链团队协作（`title` / `created_by` / 标签 / 乐观锁 / 软删回收站）

* **解决的断点**：补全引擎级 RLS 在 `tenants` 与 `link_clicks_daily` 上的不对称漏洞；解决多人协作编辑互相覆盖、无作者归属、无法分类筛选与误删无法恢复四大痛点。
* **完整业务流程**：
  1. **RLS 全覆盖闭环**：迁移为 `tenants`（`USING/WITH CHECK (id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)`）和 `link_clicks_daily`（`tenant_id = ...`）开启 `ENABLE ROW LEVEL SECURITY`（不加 `FORCE`，保障白名单跨租户路径与建组织流程无阻），并在 [rls_test.go](../internal/postgres/rls_test.go) 补全正反用例。
  2. **协作者与并发编辑防覆盖（乐观锁）**：`links` 增加 `title`、`created_by`、`version INT NOT NULL DEFAULT 1`；详情与列表返回 `version`（可同时映射 HTTP `ETag`）；编辑弹窗提交携带 `expected_version`，服务端用 `UPDATE links SET ..., version = version + 1 WHERE id = $1 AND tenant_id = $2 AND version = $3` 做 CAS 校验，若 0 行受影响则区分“不存在/无权/已被封禁/版本过期”，版本过期返回 `409 Conflict`；前端弹窗捕获 409 后提示“他人已修改”，支持一键拉取最新数据对比覆盖。
  3. **多对多标签管理**：新增 `tags`（同租户标签名唯一、颜色枚举）与 `link_tags`（联合主键 `(link_id, tag_id)` + `tenant_id`，双表开 RLS）；支持在创建/编辑弹窗快速选用或新建标签、列表按多标签筛选（支持“全部匹配交集”和“任一匹配并集”），删除标签只清理关联表，不影响短链本身。
  4. **短链回收站（软删与彻底清理）**：`links` 增加 `deleted_at`（与 `UNIQUE (code)` 协同：`code` 仍保持全库不可复用，避免短码被他人重用造成旧海报串链；或明确约定回收站保留期内禁止重用）；用户执行删除改为软删（写入 `deleted_at = now()`、立即从跳转与默认列表隐身、同步写 Redis 负缓存、**不返还本月配额**、写审计日志、触发 Webhook）。被平台封禁（`banned_at IS NOT NULL`）的短链禁止软删或从回收站恢复。用户可在 30 天内从“回收站”标签页恢复短链（需同时复查风控状态并刷新缓存），或手动“彻底删除”；超 30 天由 `worker` 分批物理删除 `links` 行并入队 `link.purge_stats` 异步清理三张日统计表。
* **表结构与 SQL 训练点**：
  * **乐观锁 CAS**：用单条原子 `UPDATE ... WHERE version = $N` 取代部分读-改-写悲观锁；
  * **软删索引重构**：`links` 上的活跃查询与状态部分索引增加 `deleted_at IS NULL` 过滤，新增 `idx_links_tenant_deleted` 支撑回收站列表与 `worker` 过期物理清理；
  * **多对多经典 SQL**：用 `GROUP BY link_id HAVING COUNT(DISTINCT tag_id) = $N` 实现精准标签交集筛选；用一次关联查询或 `array_agg` / 两步映射消除列表返回标签的 N+1 查询。
* **前端关键点**（同步补齐基础体验短板）：
  * [LinksPage.tsx](../web/src/features/links/LinksPage.tsx) 使用 `useSearchParams` 把 `sort`、`status`、`q`、`tag`、`view`、`trash` 同步到 URL Query；
  * [router.tsx](../web/src/app/router.tsx) 全量切换 `React.lazy` + `Suspense` 骨架屏，增加 `ErrorBoundary` 识别 chunk 加载失败并引导刷新；
  * [links/api.ts](../web/src/features/links/api.ts) 为停用/启用、软删/恢复实现 TanStack Query 乐观更新（`onMutate` 原地改 `InfiniteData`、失败回滚）与鼠标悬停详情 `prefetchQuery`；
  * 引入 **Playwright** 建立第一批真实浏览器 E2E 回归用例（登录态 Cookie/CSRF 写操作、建链打标签、并发编辑 409 冲突、软删与恢复、邀请直兑）。

---

### 阶段 B：API 契约化、写幂等、检索 / 分区调优与分布式限流

#### 闭环 B1：OpenAPI 3.1 单一契约源 + `Idempotency-Key` 写幂等闭环

* **解决的断点**：前后端接口与类型靠人肉对齐；高并发/弱网重试 `POST /api/v1/links` 或建组织/签发密钥等写请求会重复扣配额或产生重复资源。
* **完整业务流程**：
  1. **契约先行与自动化校验**：建立 `api/openapi.yaml` 作为 HTTP 契约单一事实源；后端单元测试使用响应校验器断言真实 handler 输出符合 OpenAPI Schema；前端通过脚本自动生成 TS 接口类型供 `features/*/api.ts` 消费，逐步淘汰手工重复声明。
  2. **通用写幂等闭环**：客户端在创建类写请求（`POST /api/v1/links`、`POST /api/v1/orgs`、`POST /api/v1/webhooks` 等）携带 `Idempotency-Key` 请求头。
     * **抢占锁态**：在业务事务或前置状态机写入 `idempotency_keys`（`status = 'in_progress'`、`request_hash`、`expires_at = now() + 24h`）利用 `ON CONFLICT DO NOTHING` 抢占唯一键；
     * **冲突分支**：若已存在且 `status = 'in_progress'`，返回 `409 Conflict` + `Retry-After`；若已 `completed` 且 `request_hash` 一致，直接重放持久化响应状态码与 JSON 体（**不二次扣配额、不二次写审计、不二次触发 Webhook**）；若同 Key 但 `request_hash` 不同，返回 `422 Unprocessable Entity` 防误用；
     * **失败回滚与清理**：业务校验 4xx 错误或意外 panic 时释放或标记失败策略清晰可控；`worker` 按 `idx_idempotency_keys_expires_at` 分批清理过期幂等记录。
* **表结构与 SQL 训练点**：`idempotency_keys` 表（开启 RLS，主键 `(tenant_id, key)` 或复合作用域键区分用户级与租户级），实战 **PostgreSQL `INSERT ... ON CONFLICT DO NOTHING` 并发状态机** 与短生命周期滚动表的索引分批回收。
* **后端关键点**：独立封装 HTTP 幂等中间件/事务协同器，严谨处理“业务事务失败时幂等键如何不残留锁死”。
* **前端关键点**：API 客户端自动为关键创建提交生成单次提交级别的 `Idempotency-Key`（重试同一次提交复用 Key，用户修改重填重置 Key）；表单全面升级 **Zod + React Hook Form**，实现声明式校验、脏表单离开/关闭提醒，以及服务端字段错误精准标红到输入框。

#### 闭环 B2：短链模糊搜索优化 + 审计日志分区裁剪与 JSONB 检索闭环

* **解决的断点**：消除大租户短链关键词搜索退化全表扫描；解决 `audit_logs` 月分区裁剪失效与 `metadata` 无法检索的问题；让前端能按短链或操作对象反向追溯审计轨迹。
* **完整业务流程**：
  1. **短链精准与模糊双路检索**：当用户输入符合 8 位 Base62 短码格式时，服务端优先精确匹配短码命中 `links_code_key`；任意关键词模糊搜索 URL/Title/Code 时命中 `pg_trgm` GIN 索引，保证大租户下响应毫秒级稳定。
  2. **审计时间窗与按对象追溯闭环**：审计日志页新增“时间范围选择器（默认近 30 天）”与“按目标筛选（如 `target_type=link, target_id=AbC123xy`）”；短链详情页新增 **“操作历史” Tab**，可直接查看谁在何时创建、改过什么 URL、谁停用或被风控自动封禁。支持按关键元数据字段（例如按原始变更前 URL 或请求维度）检索。
* **表结构与 SQL 训练点**：
  * **Online DDL 实战**：使用 `CREATE EXTENSION IF NOT EXISTS pg_trgm`，并用 **`CREATE INDEX CONCURRENTLY`** 在线创建 `idx_links_search_trgm`（按 `(url || ' ' || title || ' ' || code)` 或分列建 GIN `gin_trgm_ops`），写一个 10 万行合成数据脚本用 `EXPLAIN (ANALYZE, BUFFERS)` 量化优化前后对比并记入 `docs/postmortems/`。
  * **分区裁剪对齐**：将 `audit_logs` 列表分页游标从单一 `id` 升级为 **`(created_at, id)` 复合游标**，查询默认带 `created_at BETWEEN ...`，让 PG 真正跳过无关历史月分区；增加 `(tenant_id, target_type, target_id, created_at DESC, id DESC)` 局部/复合索引与按需表达式/JSONB `jsonb_path_ops` 索引对比测试。
* **后端关键点**：游标编解码与向后兼容（前端同仓同步迁移，不留兼容壳）；确保复合游标严格单调且零漏行。
* **前端关键点**：短链详情页新增“操作历史”抽屉/Tab；审计页增加时间范围快捷筛选并同步进 URL SearchParams。

#### 闭环 B3：分布式 Redis Lua 限流与慢依赖故障注入闭环

* **解决的断点**：修复多副本下按租户/按 IP 限流放大 N 倍及单机 `singleflight` 局限；补全系统对“依赖变慢而非断连”的韧性验证。
* **完整业务流程**：
  1. **多副本一致性限流**：当配置了 `REDIS_URL` 时，租户与 IP 限流器切换为 Redis Lua 脚本实现的原子令牌桶/滑动窗口；当 Redis 故障或超时时，自动记录降级指标并平滑切回进程内本地限流器（保证管理面可用性优先，不因 Redis 抖动拒绝全部管理请求）。
  2. **多副本缓存回源对比与取舍**：依据 [roadmap.md](roadmap.md#L59) 预留课题，实测对比 `SET NX PX` 分布式互斥回源锁与现有短 TTL + `singleflight`，留下压测对比结论（按实证决定是否合入主路径）。
  3. **慢依赖混沌演练与数据库救火演练**：在本地/测试环境搭建可控延迟代理（模拟 PG 慢查询、Redis 500ms 卡顿、Webhook 慢滴流），验证超时级联、连接池排队、熔断器与 Prometheus 告警全部按预期生效。
* **表结构与 SQL 训练点**：启用并实操 **`pg_stat_statements`**（抓取高耗时/高 I/O Top SQL）、**`pg_locks` + `pg_stat_activity`**（人为复现一次 `idle in transaction` 阻塞 `DDL` 或互锁，练习定位 Blocking PID），并实测复现与分析 `pgx` Prepared Statement 的 Generic Plan vs Custom Plan 计划切换行为，沉淀到 [runbook.md](runbook.md)。
* **后端关键点**：Redis Lua 脚本原子性与时钟偏差控制（使用 Redis `TIME` 或固定时间片）；限流器接口抽象无缝切换本地/分布式模式。
* **前端关键点**：收到 `429` + `Retry-After` 时在 UI 呈现明确倒计时与温和提示，避免用户狂点触发雪崩重试。

---

### 阶段 C：组织宏观分析、SSE 实时推送与批量 CSV 导入导出

#### 闭环 C1：组织级宏观数据看板

* **解决的断点**：目前只能看单条短链统计，团队负责人无法一站式查看“整个组织近 7/30 天总点击、UV 变化、环比增长、核心来源渠道与热门短链排行”。
* **完整业务流程**：
  1. 导航新增“概览（Overview）”首页（`/orgs/:orgId/overview`，替代原来一进来直接进列表，或作为侧边栏首项），受 `FEATURE_ANALYTICS` 开关控制。
  2. 一屏呈现：核心 KPI 卡片（总点击、环比涨跌幅、活跃短链数、月度配额水位）、组织级日趋势对比图（当期 vs 上期）、设备/客户端占比、Top 热门短链及 Top 来源域名。
  3. **数据一致性与性能兜底**：默认仅读可控窗口（最多 90 天）；当通过基准测试实测“大租户下直接从 `link_clicks_daily` / `link_dimension_daily` 实时聚合触发慢查询门槛”时，立 ADR 引入 `tenant_clicks_daily` 增量汇总表（在 `RecordClicks` 同事务更新）及离线重算对账 SQL。
* **表结构与 SQL 训练点**：实战中高级分析型 SQL——**`generate_series()` 生成连续日期骨架左连补零**、**窗口函数 `LAG()` 算日/周环比、`SUM() OVER (ROWS BETWEEN 6 PRECEDING AND CURRENT ROW)` 算 7 日移动平均、`ROW_NUMBER()` 组内排名**、**`GROUPING SETS` + `FILTER (WHERE ...)` 单次扫描出多维小计**。
* **后端关键点**：定义组织概览只读 Store 接口与 `REPEATABLE READ` 快照读取；`FEATURE_ANALYTICS=false` 时自动隐藏或返回明确未启用状态。
* **前端关键点**：封装可复用对比趋势图组件，支持切换 7/30/90 天与时间窗口同步到 URL。

#### 闭环 C2：SSE 跨副本实时事件推送闭环

* **解决的断点**：用户创建完短链或触发 Webhook 测试后，必须手动点刷新才能看到风控是否命中封禁、Webhook 是否投递成功或批量导入进度。
* **完整业务流程**：
  1. `worker` 完成 `link.risk_scan`（封禁或放行）、`webhook.deliver` 终态/熔断、或 `link.bulk_import` 进度推进后，通过 Redis Pub/Sub 向频道 `tenant:<id>:events` 发布轻量事件通知（含类型、资源 ID、摘要；Redis 不可用时优雅跳过实时推送，不影响持久化主流程）。
  2. `console serve` 提供 `GET /api/v1/events`（校验会话/API Key 与 `X-Org-ID` 权限），订阅当前租户频道并以 `text/event-stream` 下发心跳与事件帧，支持 `Last-Event-ID` 或断线重连后提示前端补拉。
  3. **连接保护**：单独剥离短请求的 5s `REQUEST_TIMEOUT` 限制，改为监听 `req.Context().Done()` + 进程优雅关停广播 + 最长单连接存活周期（防代理僵尸连接）；限制单用户并发 SSE 连接上限；慢消费者写入阻塞超时直接断开而非无限堆积内存。
* **表结构与 SQL 训练点**：保持状态事实落在 `links` / `jobs` / `webhook_deliveries` / `import_jobs` 表中，Pub/Sub 只作**纯通知信号（Notify-then-Fetch 或携带轻量增量）**，不发明另一套状态存储，防止推送丢失导致前端永久状态错误。
* **后端关键点**：Go `http.ResponseController.Flush()` 规范使用、跨副本 Pub/Sub 扇出、连接数与推送耗时 Prometheus 指标、集成测试覆盖并发关闭不卡住 `Shutdown`。
* **前端关键点**：实现 `useEventSource` Hook（指数退避重连、`visibilitychange` 后台静默/切回补刷、收包后直接调用 TanStack Query `setQueryData` 原地更新缓存并弹出非侵入 `toast`）。

#### 闭环 C3：CSV 批量导入短链与流式导出报表闭环

* **解决的断点**：B 端真实上线或迁移时手工一条条建链不可用；运营与审计合规导出需要支持大数据集不 OOM。
* **完整业务流程**：
  1. **批量导入闭环**：用户在控制台上传 CSV（前端做预检：列头校验、行数上限、大小提示）→ 服务端校验配额余量后在事务内写入 `import_jobs`（开 RLS，记录总行数、成功/失败数、状态）与 `jobs`（`link.bulk_import`），立即返回 `202 Accepted` + 任务 ID；`worker` 分块解析、校验 URL 安全、生成短码、批量扣配额并写 `links` 与审计日志，将每条失败行号与原因批量写入 `import_job_errors`（开 RLS）；用户通过 **SSE 实时进度条**看到进度，完成后可直接在页面下载“失败行修正 CSV”重传；超保留期的导入任务与明细由 `worker` 定期清理。
  2. **流式导出闭环**：支持导出筛选后的短链列表与指定时间段的审计日志 CSV；服务端用 `io.Pipe` 边游标分批读（`Keyset` 分页，禁止 `OFFSET`）边流式写 HTTP 响应，实现恒定低内存开销；**严格防御 CSV 公式注入**（对 `=`、`+`、`-`、`@` 等开头单元格做前置转义）。
* **表结构与 SQL 训练点**：对比 PostgreSQL 批量写入方式（`unnest` 数组批插 vs `pgx.CopyFrom` 协议吞吐对比）；部分成功场景下配额计数精准扣减与事务边界切分；导入任务与错误明细表的 `ON DELETE` 等价应用层分批清理。
* **后端关键点**：上传文件体积/行数/单字段字节严格限流防内存爆；流式导出时正确处理客户端中途断开取消 DB 查询。
* **前端关键点**：拖拽上传 CSV + 预览前 5 行 + SSE 实时进度条 + 失败结果下载；同时在 `LinksPage` 接入 `@tanstack/react-virtual` **大列表虚拟滚动**与**多选批量操作栏**（批量停用/软删，并妥善展示“10 条成功、2 条因已封禁跳过”的部分成功反馈）。

---

### 阶段 D：商业化 SaaS 核心壁垒模块（二选一或按序深入）

> 说明：前三个阶段已经覆盖绝大多数日常研发与中高级全栈考核点；阶段 D 用于专攻大型商业 SaaS 的高难度领域建模与零停机数据库演进。

#### 闭环 D1：自定义品牌域名与自定义短码（Expand-Contract 零停机演进）

* **解决的断点**：打破全平台共用单一默认域名与仅限 8 位随机短码的限制，支撑品牌短链（如 `go.acme.com/launch`）。
* **完整业务流程**：
  1. **域名接入与 DNS 校验**：组织管理员在控制台提交自定义域名（校验合法性、禁绑定平台保留域名与内网域名），写入 `domains` 表（开 RLS，`UNIQUE (host) WHERE deleted_at IS NULL`）并生成唯一 TXT 验证令牌；入队 `domain.verify_dns` 任务（或手动触发“立即验证”），`worker` 查询公网 DNS TXT 记录校验所有权通过后激活，失败自动指数退避并提示具体错误原因。
  2. **自定义后缀创建与冲突处理**：创建短链时可选已验证的品牌域名（默认走平台域）和自定义 `slug`（正则白名单 + 保留路径黑名单如 `healthz`/`api`/`metrics`），唯一冲突返回清晰 `409`。
  3. **在线零停机数据结构迁移（Expand-Contract 三步实战）**：
     * **Expand**：`links` 新增可空 `domain_id`（`NULL` 表示平台默认域），使用 **`CREATE UNIQUE INDEX CONCURRENTLY`** 新建 `(COALESCE(domain_id, 0), code) WHERE deleted_at IS NULL`（或分两条部分唯一索引），`ADD CONSTRAINT ... NOT VALID` 后 `VALIDATE CONSTRAINT`；
     * **Dual-Read/Write**：应用与 `redirector` 支持按 `(Host, code)` 查库与构造缓存键 `shortlink:v2:link:<host>:<code>`；分批回填历史数据；
     * **Contract**：确认无旧路径流量与数据对账无误后，再移除旧全局 `links_code_key`。
  4. **域名下线闭环**：停用或删除自定义域名时，检查是否仍有活跃短链（或要求显式确认批量失效并同步驱逐缓存），防止悬空跳转。
* **表结构与 SQL 训练点**：真实生产级 **Expand-Contract 零停机迁移全流程**、部分唯一索引隔离平台默认域与租户品牌域、跨域名路由索引设计。
* **后端关键点**：`redirector` 安全解析与白名单校验 `Host` 头防 Host Header 注入；多域名缓存键隔离与批量驱逐策略。
* **前端关键点**：分步式域名绑定向导（填域名 → 复制 DNS 记录 → 轮询/SSE 看校验结果 → 绑定成功建链）。

#### 闭环 D2：套餐订阅、额度包账本与平台级租户治理

* **解决的断点**：让配额、限流、保留天数、Webhook 上限、自定义域名数真正受订阅套餐与按需额度包动态驱动，并补齐平台级租户风控封停与动态风控规则库。
* **完整业务流程**：
  1. **套餐与订阅闭环**：建立 `plans`（套餐目录与 JSONB/结构化权益配置）、`subscriptions`（租户订阅周期与状态机：`trialing`/`active`/`past_due`/`canceled`）、`billing_events`（支付网关回调事件幂等表）；实现一个内存/本地 `fakebilling` 模拟支付网关（类似现有 `fakeoidc`），演练**支付回调重复投递、乱序到达（先到取消后到续费）、升级立即生效、降级在计费期末生效、逾期宽限期**完整状态机。
  2. **不可变额度包账本（Ledger）**：除套餐内月度基础额度外，支持购买/赠送“额外短链额度包”；使用只追加不可修改的 `quota_ledger` 流水表记录每一笔发放、消费、过期作废与退款冲红，杜绝直接覆写历史余额；附带每日流水汇总与当前余量快照对账 SQL。
  3. **平台级租户治理与动态风控**：`tenants` 增加 `suspended_at` / `suspend_reason`（平台冻结后，该组织下所有短链跳转返回特定拦截/404、管理面与 API Key 拦截，且冻结原因对组织可见、解冻可恢复）；新增 `risk_rules` 动态黑名单/敏感词表，平台运维增删规则后通过缓存/短周期刷新热生效，无需重启 `worker`。
* **表结构与 SQL 训练点**：
  * **PostgreSQL `EXCLUDE USING gist` 区间排他约束**：在 `subscriptions` 上用 `tstzrange(starts_at, ends_at)` 加上排他约束，**从数据库底层绝对杜绝同一租户同时存在两条时间重叠的有效订阅**；
  * **Append-Only 金融级账本建模**与余额对账 SQL；
  * 幂等事件去重与乱序版本号比较（`event_seq` / `occurred_at`）。
* **后端关键点**：动态权益解析器（`Entitlements`）替换代码内写死常量（如最大 Webhook 数、统计/审计保留期）；平台运维 CLI 与管理端治理接口对齐。
* **前端关键点**：套餐对比与当前用量权益页、超限引导升级交互；借此阶段顺手打磨两项高阶 SaaS UI 基础设施——**`Cmd+K` 全局命令面板**（快速跳组织、搜短链、切功能页）与**无首屏闪烁深色模式（Dark Mode）**。

---

## 四、全阶段“业务闭环”上线前核对清单（6 问 Checklist）

每个功能合并或进入下一阶段前，必须逐项核对以下 6 个闭环是否全部闭合：

1. **入口与降级闭环**：用户能否从控制台自然找到并走通流程（含空状态、移动端布局与快捷键）？功能开关关闭或角色权限不足时，导航、按钮、API 与 `worker` 是否一致降级（参见 [ADR 0029](adr/0029-modular-feature-plugins.md)）？
2. **数据与安全闭环**：含 `tenant_id` 的新表是否全部在迁移中完成 `shortlink_tenant` 授权、开启 `ENABLE ROW LEVEL SECURITY`（不加 `FORCE`）并配好零 `OR` fail-closed 策略？写操作是否全部经 `withTenantTx` 写了必选 `audit_logs` 且不阻塞公开跳转路径？涉及缓存的新增/修改/删停路径是否处理了一致性？
3. **并发闭环**：是否存在双人并发编辑覆盖（乐观锁 CAS）、并发重复点击/网络重试重复创建（`Idempotency-Key`）、或跨多表更新锁序不一致引发死锁的隐患？
4. **异常与幂等闭环**：第三方调用或异步任务超时、部分失败、回调乱序、Redis 抖动或慢延迟时，是否会留下半截脏数据（如扣了配额没建链、短信发失败却占着冷却额度）？
5. **生命周期闭环**：**每一张新增的数据表**，都必须回答“一年后它会有多少行？谁来按什么索引分批清理或整月 `DROP TABLE`？”绝不允许只增不减的日志/流水/状态表上线裸奔。
6. **实证与验证闭环**：新增 SQL 是否跑过合成数据 `EXPLAIN (ANALYZE, BUFFERS)`？高并发逻辑是否补齐 `-race` 与 `testing/synctest` 测试？前端是否跑通 Vitest 与核心路径 Playwright E2E，并同步更新 [api.md](api.md)、[runbook.md](runbook.md) 与 [roadmap.md](roadmap.md)？

---

## 五、SQL & 表结构专项能力映射速查表

如果某段时间想专门练透某一项 SQL 或数据库建模技能，可直接按此表挑对应的业务闭环入手：

| 想夯实的 SQL / 表结构专项技能 | 对应演进闭环 |
| :--- | :--- |
| PostgreSQL 引擎级 RLS 闭环与 Fail-Closed 策略验证 | 闭环 A3（补齐 `tenants` 与 `link_clicks_daily` RLS） |
| 乐观锁 CAS（`version`）与父行加锁防并发违约 | 闭环 A2（防解绑最后一个身份）、闭环 A3（短链协同编辑） |
| 软删除与部分唯一索引协同 | 闭环 A3（回收站）、闭环 D1（域名 `host` 软删重建） |
| 多对多建模、集合交集/并集筛选与消除 N+1 | 闭环 A3（`tags` + `link_tags`） |
| 并发幂等状态机（`INSERT ... ON CONFLICT DO NOTHING`） | 闭环 B1（`idempotency_keys`）、闭环 D2（`billing_events`） |
| `pg_trgm` GIN 模糊搜索与 `CREATE INDEX CONCURRENTLY` | 闭环 B2（短链全文/模糊检索） |
| 分区键对齐复合游标触发分区裁剪 + JSONB 检索 | 闭环 B2（`audit_logs` `(created_at, id)` 与操作历史检索） |
| `pg_stat_statements`、`pg_locks` 阻塞排查与执行计划分析 | 闭环 B3（慢依赖混沌与数据库诊断实战） |
| 窗口函数（`LAG`/移动平均）、`GROUPING SETS`/`FILTER`、`generate_series` | 闭环 C1（组织宏观看板） |
| 批量写入吞吐优化（`unnest` vs `COPY`）与 Keyset 流式分页 | 闭环 C3（CSV 批量导入导出） |
| Expand-Contract 零停机迁移、`NOT VALID` / `VALIDATE CONSTRAINT` | 闭环 D1（全局短码演进为 `(domain_id, code)`） |
| `EXCLUDE USING gist` 时间区间排他约束与 Append-Only 账本表 | 闭环 D2（`subscriptions` 与 `quota_ledger`） |
