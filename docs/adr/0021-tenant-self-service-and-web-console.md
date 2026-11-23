# 0021. 租户自助闭环与最小前端控制台

- 状态：Accepted
- 日期：2026-11-23

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 自助创建组织 | 新增 `POST /api/v1/orgs`，仅接受 `sl_session` 会话 Cookie + `CrossOriginProtection`（不要求 `X-Org-ID`，拒绝 API Key）；事务内建租户并把当前用户设为 `owner`，限制单用户最多作为 `owner` 创建 10 个组织 |
| 组织邀请 API | 新增 `GET/POST/DELETE /api/v1/invitations`，复用 `members:read` / `members:write`；写操作仅允许会话用户，写入 `invitations.invited_by`，`admin` 不可邀请或撤销 `owner` 邀请 |
| 组织 API Key API | 新增 `GET/POST/DELETE /api/v1/api-keys` 与权限 `apikeys:read` / `apikeys:write`（仅授予 `owner`/`admin`，不开放给 API Key）；写入 `api_keys.created_by`，吊销强制按 `tenant_id` 隔离，签发明文只在 201 响应返回一次 |
| 登录回调跳转 | `login` 端点接受可选站内相对路径 `?return_to=/...`，经校验后写入 HMAC 签名的 `authFlowCookie`；回调成功有 `return_to` 时返回 `302`，缺省保持 `200 JSON` 兼容现有调用方与测试 |
| 前端技术栈 | 独立 `web/` 工程：**Vite + React + TypeScript + pnpm**，与 Go 后端解耦；不破坏 `go build` / `make verify` 的零 Node 依赖 |
| 前端挂载与同源策略 | 开发态用 Vite `server.proxy` 将 `/api`、`/r` 代理到后端实现同源；生产/联调态由 `console serve` 通过可选 `WEB_DIST_DIR`（如 `web/dist`）在 `/` 提供 SPA，严格不对 `/api/*`、`/r/*`、运维端点回退 `index.html` |

---

## 背景

阶段 11（[ADR 0018](0018-users-orgs-and-rbac.md)）落地了 OIDC / GitHub OAuth 2.0 登录、服务端会话、组织成员管理与 API Key scope，但产品闭环存在明显断层：

1. **所有租户生命周期入口都卡在运维 CLI**：新用户登录后无法创建自己的组织；组织管理员无法通过 HTTP 发起或撤销邀请；组织内无法自助签发、查看和吊销 API Key（尽管迁移 007 已提前建好 `invitations.invited_by` 与 `api_keys.created_by` 列）。
2. **缺少真实浏览器界面**：登录回调只返回 JSON，浏览器里的 Cookie、`SameSite=Lax`、`CrossOriginProtection`、`X-Org-ID` 与权限分层从未在真实前端交互中端到端走通。

现有约束：
- 保持 `/api/v1` 既有契约兼容（[ADR 0001](0001-api-versioning-and-route-namespace.md)）；
- `sl_session` Cookie 作用范围限定在 `Path=/api/`，不发往公开跳转 `/r/{code}`（[ADR 0018](0018-users-orgs-and-rbac.md)）；
- `internal/console` 按 [ADR 0019](0019-httpapi-package-evolution.md) 继续拆小结构体（`orgHandler`、`invitationHandler`、`apiKeyHandler`），不往 `Server` 堆字段；
- `make verify` 与 Go 单测不能强依赖 Node.js 构建前端产物。

---

## 决策

### 1. 零新增数据库迁移

迁移 007（[007_add_users_and_rbac.up.sql](../../migrations/007_add_users_and_rbac.up.sql)）已经建好 `invitations.invited_by`、`api_keys.created_by`、`api_keys.scopes` 及相关索引；`tenants`、`memberships` 表结构已满足需求。本期无需新增迁移文件。

### 2. 自助管理 HTTP API

#### 2a. 认证提供方发现与登录 `return_to`

- 新增公开只读端点 `GET /api/v1/auth/providers`：返回 `{"oidc": bool, "github": bool}`，供前端按实际后端配置展示登录按钮。
- `GET /api/v1/auth/login` 与 `GET /api/v1/auth/github/login` 接受可选查询参数 `return_to`：
  - 校验规则：必须为空或站内相对路径——以 `/` 开头、不能以 `//` 开头、不含反斜杠 `\` 或控制字符、不能指向 `/api/` 或 `/r/` 前缀、长度不超过 256 字节；非法值返回 `400 {"error":"invalid return_to"}`。
  - 合法的 `return_to` 随 `state`、`code_verifier`、`nonce` 一起签入 `authFlowCookie`（`r` 字段，受 HMAC-SHA256 防篡改）。
  - 回调成功签发 `sl_session` 后：若 `flow.ReturnTo != ""` 返回 `302 Found` 并设 `Location: <return_to>`；若为空则保持原有 `200 OK` + JSON 响应体，完全兼容已有 `curl` 流程与测试。

#### 2b. 自助创建组织（`POST /api/v1/orgs`）

- **鉴权方式**：与 `GET /api/v1/me` 一致，只认唯一的 `sl_session` Cookie，并执行 `CrossOriginProtection`（写请求防 CSRF）；**不要求 `X-Org-ID`**（零组织的新用户必须能创建第一个组织），带 `Authorization` 或无有效会话统一返回 `401`。响应带 `Cache-Control: no-store`。
- **请求与校验**：`{"name": "..."}`，`name` 去首尾空白后长度 1–100 字节。
- **事务与配额保护**：在同一事务内对当前用户 `SELECT ... FROM users WHERE id = $1 FOR UPDATE` 串行化建组织操作，统计该用户当前 `role = 'owner'` 的组织数量，达到上限（默认 `10`）返回 `409 {"error":"organization limit exceeded"}`；否则插入 `tenants (name)` 与 `memberships (user_id, tenant_id, 'owner')`，返回 `201 Created` 与 `{"id", "name", "role": "owner", "created_at"}`。

#### 2c. 组织邀请管理（`/api/v1/invitations`）

挂在 `consoleRoute`（`X-Org-ID` + 按租户限流 + 权限检查），复用已有 `members:read` / `members:write`：

- `GET /api/v1/invitations`（需 `members:read`）：按 `id DESC` 返回当前组织的邀请列表，每项包含 `id`、`email`、`role`、`status`（派生：`accepted` / `revoked` / `expired` / `pending`）、`invited_by`、`created_at`、`expires_at`、`accepted_at`、`revoked_at`。
- `POST /api/v1/invitations`（需 `members:write`，且纵深防御仅允许 `ActorUser`）：
  - 请求体 `{"email": "...", "role": "...", "expires_at"?: RFC3339}`；
  - 角色边界复用 `identity.CanAssignMemberRole(p.Role, req.Role)`（`admin` 不能邀请 `owner`，返回 `403`）；
  - 复用 `postgres.Store.CreateInvitation` 并传入 `InvitedBy: &p.ActorID`；已存在未过期未撤销的同邮箱待兑现邀请返回 `409`；成功返回 `201 Created`。
- `DELETE /api/v1/invitations/{id}`（需 `members:write`，仅允许 `ActorUser`）：
  - 在事务内按 `(id, tenant_id)` 查并锁住目标邀请：不存在或属于其他租户返回 `404`；目标角色为 `owner` 且操作人为 `admin` 返回 `403`；已兑现或已撤销返回 `409`；否则设置 `revoked_at = now()` 并返回 `204 No Content`。

#### 2d. 组织内 API Key 管理（`/api/v1/api-keys`）

- **权限模型**：在 `internal/identity` 新增 `PermAPIKeysRead = "apikeys:read"` 和 `PermAPIKeysWrite = "apikeys:write"`，在 `MapAuthorizer` 中**只授予 `owner` 与 `admin`**。
- **绝不开放给 API Key**：不把 `apikeys:*` 加入 `api_keys.scopes` 白名单与数据库 CHECK，同时在 handler 内显式要求 `p.ActorKind == identity.ActorUser`（纵深防御，防止未来更换策略引擎误放行 API Key 导致密钥签发密钥或自我提权）。
- 接口清单（全部带 `Cache-Control: no-store`）：
  - `GET /api/v1/api-keys`（`apikeys:read`）：复用 `ListAPIKeys(ctx, p.Tenant.ID)`（补扫 `created_by` 字段），返回元数据列表（`id`、`scopes`、`status`、`created_by`、`created_at`、`expires_at`、`revoked_at`），不返回明文和哈希。
  - `POST /api/v1/api-keys`（`apikeys:write`）：请求体 `{"scopes": [...], "expires_at"?: RFC3339}`，`scopes` 必须非空且全部合法（`links:read`、`links:write`、`stats:read`）；复用 `IssueAPIKey` 并新增 `CreatedBy *int64` 写入 `api_keys.created_by`；返回 `201 Created`，包含密钥元数据与仅此一次的 `api_key` 明文。
  - `DELETE /api/v1/api-keys/{id}`（`apikeys:write`）：新增 `RevokeAPIKeyForTenant(ctx, tenantID, keyID)` 按 `(id, tenant_id)` 强制租户隔离：跨租户或不存在返回 `404`（不暴露其他租户密钥是否存在），已吊销返回 `409`，成功返回 `204 No Content`。

### 3. 最小前端控制台（`web/`）

- **技术选型**：独立 `web/` 目录，采用 **Vite + React + TypeScript + pnpm**，页面样式保持轻洁现代，不引入重型 UI 框架。
- **功能范围**：
  1. **登录与引导**：调用 `GET /api/v1/auth/providers` 展示 Dex OIDC / GitHub 登录入口（携带 `?return_to=/`）；登录后调用 `GET /api/v1/me` 拉取用户与组织列表；无组织时直接引导创建首个组织。
  2. **组织切换与创建**：顶栏提供组织切换器（请求自动带 `X-Org-ID`）与新建组织弹窗、登出按钮。
  3. **短链管理与统计**：短链创建、列表（支持排序与游标翻页、展示 `active`/`disabled`/`expired`/`banned` 状态与 `ban_reason`）、修改目标 URL、启用/停用、删除，以及单条短链的点击统计视图（总点击、UV、日趋势、来源/设备/客户端明细）。
  4. **成员、邀请与 API Key**：成员列表/改角色/移出、邀请列表/发起/撤销、API Key 列表/签发（一次性复制明文）/吊销；按当前组织角色隐藏无权操作按钮（服务端鉴权保持权威）。
- **同源开发与静态托管**：
  - **本地联调（单进程静态托管）**：执行 `make web-build` 产出 `web/dist`，配置 `WEB_DIST_DIR=web/dist` 启动 `make run-console` 后，访问 `http://127.0.0.1:<console_port>/` 即可与 `/api/v1/*` 天然同源，直接使用现有 `OIDC_REDIRECT_URL` 与 `sl_session`（`Path=/api/`）。
  - **前端热更新开发**：执行 `make web-dev` 启动 Vite（默认 `127.0.0.1:5173`），通过 `server.proxy` 将 `/api` 转到 `console`、`/r` 转到 `redirector`（保持 `Host` 一致，`deploy/dex.yaml` 增补 `http://127.0.0.1:5173/api/v1/auth/callback`）。
  - **SPA 路由边界**：`console` 仅在配置了 `WEB_DIST_DIR` 时挂载静态文件与 SPA fallback；`/api/`、`/r/`、`/healthz`、`/readyz`、`/metrics` 路径绝不回退到 `index.html`，保留原有 404 与路由隔离契约。
  - `deploy/nginx.conf` 与 `deploy/nginx.stack.conf` 同步说明：启用前端时可将 `/` 代理至 `console`（`/r/` 仍归 `redirector`）。

---

## 不做什么

- **不做邮件发送邀请**：邀请仍为库内登记，受邀人通过 OIDC/GitHub 已验证邮箱登录后自动兑现（邮件投递留到后续接 `jobs` 引擎）。
- **不做组织重命名、组织删除与所有权转让之外的复杂治理**，不做配额/限流自助修改（仍由平台运维命令管理）。
- **不做前端自动化浏览器 E2E 框架（Playwright/Cypress）进入 CI**：前端构建和类型检查由 `pnpm build` / `tsc` 验证，HTTP 契约由 Go httptest、集成测试与 `make smoke` 验证。
- **不给 `console` 开启 CORS 放开跨站**：开发与生产均走同源（静态托管或反代/Vite 代理），保持 `http.CrossOriginProtection` 和 `SameSite=Lax` 的严格防线。
- **不把前端构建塞进 `make verify`**：保持 Go 检查链路不依赖 Node.js。

---

## 怎么验证

1. **PostgreSQL 集成测试（`internal/postgres`）**：
   - `CreateOrg` 原子建租户 + `owner` 成员关系；超出单用户 `owner` 组织上限返回 `ErrOrgLimitExceeded`；并发创建不会绕过上限。
   - `ListInvitations` 与 `RevokeInvitation`：跨租户返回 `ErrInvitationNotFound`，`admin` 撤销 `owner` 邀请返回 `ErrMemberForbidden`，已兑现/已撤销返回冲突错误。
   - `IssueAPIKey` 写入 `created_by`、`ListAPIKeys` 读回 `created_by`；`RevokeAPIKeyForTenant` 拒绝跨租户吊销（返回 `ErrAPIKeyNotFound`）。
2. **HTTP 层测试（`internal/console`）**：
   - `GET /api/v1/auth/providers` 按配置返回开关；
   - `return_to` 合法相对路径 302 跳转，拒绝 `https://evil.com`、`//evil.com`、`/api/v1/me`、`/r/abc` 等非法值；
   - `POST /api/v1/orgs` 仅会话可用、拦截跨站（403）、拒绝 API Key（401）、成功后 `GET /api/v1/me` 立即包含新组织；
   - `/api/v1/invitations` 与 `/api/v1/api-keys` 完整权限矩阵（`owner`/`admin`/`member`/`viewer`/API Key）与响应 `Cache-Control: no-store`；
   - `WEB_DIST_DIR` 挂载时：`/` 与前端路由返回 `index.html`，`/api/v1/not-found` 与 `/r/abc` 仍返回 404。
3. **冒烟测试（`make smoke`）**：
   - 沿用 Dex 登录后，新增验证：`POST /api/v1/orgs` 自助建组织 → 在新组织下 `POST/GET/DELETE /api/v1/invitations` → `POST/GET /api/v1/api-keys` 拿新签发明文 Key 创建短链 → `DELETE /api/v1/api-keys/{id}` 后用旧 Key 访问返回 `401`；验证 `?return_to=/` 回调返回 `302 Location: /`。
4. **前端构建与联调验证**：
   - `web/` 通过 `pnpm install && pnpm build`（含 TypeScript 类型校验）；
   - `WEB_DIST_DIR=web/dist` 启动 `console` 后，验证首页静态资源加载及完整浏览器操作路径。

---

## 后果

- **收益**：形成完整的自服务多租户 SaaS 闭环（登录 → 建组织 → 邀成员 → 发 API Key → 管短链看统计），后续阶段 14（审计日志）能够覆盖全部管理写操作。
- **代价**：仓库新增 `web/` 目录及 `pnpm` 依赖；前端与后端接口需要保持字段同步。通过 `WEB_DIST_DIR` 解耦，Go 侧 CI、单测和纯 API 部署零额外依赖。
- **同步文档**：落地时同步更新 `docs/api.md`、`docs/architecture.md`、`docs/runbook.md`、`docs/roadmap.md`、`.env.example` 与 `Makefile`。

---

## 备选方案

- **`go:embed` 原生 HTML/CSS/JS 单页**：零 Node 工具链、单二进制极简，但随着后续增加审计日志、Webhook、自定义域名页面，原生 DOM 维护成本会快速上升；独立 `web/` 工程更贴近真实前后端分离项目。
- **登录后回调一律 302 跳回 `/`**（不保留 200 JSON）：更符合常见 Web 应用直觉，但会破坏已有 `docs/api.md` 契约与 `curl` 冒烟步骤；可选且受签名保护的 `return_to` 兼顾浏览器跳转与 API 兼容。
- **开发期后端开 CORS 允许 `5173 -> 18081` 跨域请求**：需要放宽 `CrossOriginProtection` 和 Cookie `SameSite` 策略，制造开发与生产安全模型差异；用 Vite 同源代理或 `WEB_DIST_DIR` 静态托管可让开发和生产保持同一套同源安全模型。
