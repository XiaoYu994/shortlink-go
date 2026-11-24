# web/AGENTS.md

本文件约定 `web/` 前端控制台的目录边界、代码风格、状态管理与测试护栏，面向在本目录工作的 AI 助手与开发者。
全局协作方式、Git 提交格式、禁止推送以及学习路线规则见根目录 [AGENTS.md](../AGENTS.md)，这里不重复定义。

## 定位与技术栈

- 独立前端控制台：React 19 + TypeScript（`strict` + `noUncheckedIndexedAccess`）+ Vite 6 + Tailwind CSS v4 + Radix UI（shadcn/ui 风格）+ React Router v7 + TanStack Query v5 + Vitest + Prettier + ESLint。
- 与 Go 后端彻底解耦：Go 侧 `make verify` / `make selfcheck` 不依赖 Node.js；CI 中的 `web` job 与 Go 检查并行独立运行。

## 目录分层与单向依赖

目录沿用与 Go 侧一致的“底座 / 业务领域 / 装配层”边界，依赖必须保持单向，禁止反向或循环 import：

```txt
src/app/         -> 装配层：路由注册、鉴权守卫、布局壳层、OrgContext（可依赖 features/components/lib）
src/features/    -> 业务领域：auth / orgs / links / members / api-keys（可依赖 components/lib/app 的 routes 与 org-context）
src/components/  -> 零业务 UI 原语（ui/）与通用展示组件（禁止 import features/ 或 app/）
src/lib/         -> 零业务路径耦合的基础设施与纯工具函数（禁止 import features/、components/ 或 app/）
```

跨领域依赖约束：
- `features/<domain>/` 之间默认隔离；仅允许复用 `features/auth/`（角色、权限判断、公开基地址）或明确只读的 query hook（例如 `api-keys` 复用 `members` 翻译成员姓名），禁止形成领域间循环依赖。
- 业务领域内的接口类型、Query Keys 与 Hooks 统一放在该领域的 `api.ts`，不按 Java/旧前端习惯把全项目切碎成全局 `services/`、`types/`、`hooks/` 文件夹。

## 单一事实来源（Single Source of Truth）

- **后端 API 端点**：全部集中在 `src/lib/endpoints.ts` 的 `API_V1_PREFIX` 与 `ENDPOINTS`。页面、组件和 `features/*/api.ts` 一律通过 `ENDPOINTS` 引用，禁止在业务代码中手写 `"/api/v1/..."` 字面量。
- **前端路由**：全部集中在 `src/app/routes.ts` 的 `ROUTE_PATTERNS` 与 `routes.*`。路由注册、`<Link>` / `<NavLink>`、`navigate()` 统一调用它；判断当前所在路由必须用 React Router 的 `useMatch` / `useParams`，禁止用 `location.pathname.split('/')` 按下标硬切。
- **业务常量**：跨多处使用的限额与默认值（如组织名称最大字节数、分页大小）放在 `src/lib/constants.ts`。
- **底层 HTTP 零业务耦合**：`src/lib/api-client.ts` 不得判断任何业务路径前缀。初始探测或登出等需要跳过全局 401 跳转的调用，由调用方显式传入 `skipAuthRedirect: true`。

## 状态管理与 React 约定

- **Server State（服务端数据）**：列表、详情、统计、成员、邀请、API Key 一律交由 TanStack Query（`useQuery` / `useInfiniteQuery` / `useMutation`）管理。
  - 每个领域在 `api.ts` 内集中声明 `<domain>Keys` 工厂，query 函数必须透传 `signal` 以支持切换组织或卸载时自动取消。
  - 写操作完成后只 `invalidateQueries` 受影响的缓存键，不要粗暴清空无关缓存；创建短链这类影响配额的操作同步失效 `authKeys.me`。
  - 禁止在 `useEffect` 中手写 `fetch` + `setState` 拉取接口数据。
- **URL State**：当前组织 `:orgId`、子页面、短链 `:code` 由 URL 承载，保证刷新与前进后退不丢状态。
- **React Context**：`OrgContext` 只放当前用户与当前组织成员信息（类比 Go `context.Context` 中的 `Principal`），不塞业务列表。
- **UI State 与表单重置**：弹窗开关、搜索草稿等纯交互状态就近用 `useState`。编辑/新建复用同一弹窗时，通过给内部表单传 `key` 重置状态，不在 `useEffect` 里 `setState` 同步 props。
- **页面标题**：每个顶层页面调用 `useDocumentTitle(...)` 更新浏览器标签标题。

## 组件抽离、样式与文案

- 相同视觉或交互模式出现 2 次以上再抽成通用组件（例如 `ErrorAlert`、`SegmentedControl<T>`、`CopyButton`、`ConfirmDialog`），避免过早过度封装。
- 样式统一使用 Tailwind CSS v4 工具类与 `src/index.css` 定义的语义令牌（`bg-background`、`text-brand`、`text-destructive` 等）；条件拼接类名一律走 `cn()`。
- 面向用户的文案与错误提示使用清晰中文，不向用户裸露 Go 后端英文错误码或内部技术术语（如 `base62`、`HLL`）。后端错误字符串翻译集中在 `src/lib/error-messages.ts`。
- 禁止使用 `window.confirm` / `window.alert`，破坏性操作统一使用 `<ConfirmDialog />`，瞬时反馈使用 `sonner` 的 `toast`。

## 命名与注释

- React 组件文件使用 `PascalCase.tsx`（与导出的组件名一致）；纯函数、工具模块、Hooks、配置与测试夹具使用 `kebab-case.ts`（如 `api-client.ts`、`use-document-title.ts`）。
- 测试文件与源码就近放置，命名为 `<Target>.test.ts` 或 `<Target>.test.tsx`。
- 注释格式与根 `AGENTS.md` 保持一致：单行 `//`，多行 `/* ... */` 且块内行首不加 `*`；注释只写“为什么”和“注意什么”。
- 优先借助 TypeScript 推导与窄类型，禁止无理由写 `any` 或用 `!` 非空断言绕过 `noUncheckedIndexedAccess`。

## 测试约定

- 组件测试使用 Vitest + React Testing Library + `@testing-library/user-event`，按用户可见的 role、label 和文案交互断言，不测组件内部 state。
- 业务页面测试统一复用 `src/test/test-utils.tsx` 的 `renderWithOrg` 与 `jsonResponse`，每个测试在 `afterEach` 恢复 mocks（`vi.restoreAllMocks()`）。
- 修改鉴权守卫、请求拦截、错误翻译或角色权限边界时，必须补充或更新对应测试。

## 提交前检查

- 改动 `web/` 任意文件后，提交前必须通过：

```bash
make web-check    # prettier --check + tsc --noEmit + eslint + vitest + vite build
```

- 需要自动格式化时执行 `make web-fmt`。
- 若改动涉及前后端契约或控制台静态托管，同时按根 `AGENTS.md` 跑 Go 侧 `make verify` / `make selfcheck`。

## 新增业务模块的标准步骤

扩展新页面（例如阶段 14 审计日志）时按以下顺序落地：

1. 在 `src/lib/endpoints.ts` 注册后端端点，在 `src/app/routes.ts` 注册路由模式与构造函数。
2. 新建 `src/features/<domain>/`，放 `api.ts`（类型、Query Keys、Hooks）、页面组件与 `*.test.tsx`。
3. 在 `src/app/router.tsx` 挂载路由，并在 `src/app/ConsoleLayout.tsx` 补充导航入口与路由匹配。
4. 运行 `make web-check` 确认格式、类型、Lint、测试和构建全绿。
