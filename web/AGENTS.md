# web/AGENTS.md

本文件只保留 `web/` 前端控制台的硬性边界与反模式红线；目录导读、React 概念对照与新增页面步骤见 [web/README.md](README.md)，全局协作与提交规范见根目录 [AGENTS.md](../AGENTS.md)。

## 验证命令

- 改动 `web/` 任意文件后提交前必跑 `make web-check`（`prettier --check` + `tsc --noEmit` + `eslint` + `vitest` + `vite build`）；格式化用 `make web-fmt`。
- 若改动涉及前后端契约或控制台静态托管，同步跑 Go 侧 `make verify` / `make selfcheck`。

## 架构边界与反模式红线

1. **单向依赖与领域隔离**：
   - `src/lib/`（零业务零 UI）与 `src/components/`（零业务）的反向依赖由 `web/eslint.config.js` 自动拦截。
   - `src/features/<domain>/` 之间默认隔离，仅允许复用 `features/auth/`（角色、权限、`capabilities`）或明确只读的 query hook，禁止领域间循环依赖；各领域类型、Query Keys 与 Hooks 统一放在各自 `api.ts`，不拆全局 `services/`、`types/`、`hooks/`。
   - `src/lib/api-client.ts` 不得硬编码业务路径前缀；跳过全局 401 跳转由调用方显式传 `skipAuthRedirect: true`。

2. **单一事实来源（Single Source of Truth）**：
   - **API 端点**统一走 `src/lib/endpoints.ts`（`ENDPOINTS`）；**前端路由**统一走 `src/app/routes.ts`（`ROUTE_PATTERNS` / `routes.*`）配合 `useMatch` / `useParams`。禁止在业务代码中手写 `"/api/v1/..."` 或用 `location.pathname.split('/')` 硬切路由。
   - **后端登录方式与可选特性**统一以 `GET /api/v1/capabilities`（`features/auth/api.ts` 的 `useCapabilities` / `useWebhooksEnabled` / `useAnalyticsEnabled` / `usePublicBaseUrl`）为准，禁止前端硬编码特性开关或登录入口；新增可选特性页面或功能必须接入能力感知降级。

3. **状态管理、UI 与类型安全**：
   - 服务端数据统一走 TanStack Query（`useQuery` / `useInfiniteQuery` / `useMutation`），query 函数透传 `signal`，写操作后只精准 `invalidateQueries` 受影响缓存键（创建短链同步失效 `authKeys.me`）；**禁止在 `useEffect` 中手写 `fetch` + `setState`**。
   - 复用弹窗表单通过传 `key` 重置状态，**禁止在 `useEffect` 里 `setState` 同步 props**；`OrgContext` 只放当前用户与当前组织成员信息。
   - **禁止 `window.confirm` / `window.alert`**（统一用 `<ConfirmDialog />` 与 `sonner` 的 `toast`）；面向用户文案用清晰中文，禁止裸露后端英文错误码或内部技术术语（翻译集中于 `src/lib/error-messages.ts`）。
   - 禁止无理由使用 `any` 或 `!` 非空断言绕过 `noUncheckedIndexedAccess`；注释格式同根仓库（单行 `//`，多行 `/* ... */` 块内行首不加 `*`）。

## 测试约束

- 使用 Testing Library 按用户可见的 role、label 和文案交互断言，不测组件内部 state；业务页面测试复用 `src/test/test-utils.tsx` 的 `renderWithOrg` 与 `jsonResponse`，每个测试在 `afterEach` 恢复 mocks（`vi.restoreAllMocks()`）。
- 测试涉及能力开关的页面或布局时，需在 mock 中显式覆盖 `/api/v1/capabilities` 响应。
