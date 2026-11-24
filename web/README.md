# Shortlink Web Console（React 学习与演进指南）

本目录是 Shortlink 的独立前端控制台，采用 **React 19 + TypeScript + Vite + Tailwind CSS v4 + Radix UI（shadcn/ui 风格）+ React Router v7 + TanStack Query v5 + Vitest + Prettier**。

它与 Go 后端保持完全解耦（Go 侧 `make verify` 不依赖 Node.js），同时沿用了与本仓库 Go 代码相同的**按业务领域分包、边界清晰、护栏齐全**的工程风格。前端详细开发规范与架构边界见 [web/AGENTS.md](AGENTS.md)。

---

## 常用命令

在仓库根目录执行（或在 `web/` 下直接跑 `pnpm ...`）：

```bash
make web-install   # 安装前端依赖
make web-fmt       # Prettier 自动格式化 web/ 源码与配置
make web-fmt-check # 仅校验格式（不修改文件）
make web-check     # 完整检查：prettier 校验 + tsc 类型检查 + eslint + vitest 测试 + vite build
make web-lint      # 仅跑 ESLint（含 react-hooks 与 TypeScript 规则）
make web-test      # 仅跑 Vitest + Testing Library 测试
make web-build     # 构建静态产物到 web/dist
make web-dev       # 启动 Vite 热更新（默认 http://127.0.0.1:5173，同源代理 /api 与 /r）
```

单端口同源运行（推荐快速体验，无需改 OIDC 回调端口）：

```bash
make web-build
WEB_DIST_DIR=web/dist make run-console
```

---

## 目录结构（对照 Go / Java 后端思维）

```txt
web/src/
├── main.tsx                # 启动装配入口（类似 Go cmd/console/serve.go）
├── env.d.ts                # Vite 客户端环境类型声明
├── index.css               # Tailwind v4 入口 + shadcn 语义色变量
├── lib/                    # 零业务路径耦合的基础设施（类似 Go internal/httpx）
│   ├── endpoints.ts        # 后端 /api/v1 端点集中注册表（Single Source of Truth）
│   ├── constants.ts        # 跨模块业务常量（名称字节上限、分页大小等）
│   ├── api-client.ts       # 同源 fetch 封装、X-Org-ID、AbortSignal、skipAuthRedirect
│   ├── error-messages.ts   # 后端英文错误码 -> 面向用户的中文提示映射
│   ├── short-url.ts        # 公开短链 URL 拼接、展示裁剪与短码格式校验
│   ├── query-client.ts     # TanStack Query 全局配置（4xx 不重试、staleTime）
│   ├── use-document-title.ts # 浏览器标签页标题 Hook
│   ├── cn.ts               # clsx + tailwind-merge 样式合并
│   └── format.ts           # 时间/数字格式化与剪贴板辅助
├── components/             # 零业务 UI 原语与通用组件（不 import features/）
│   ├── ui/                 # shadcn/ui 风格原语（Button/Badge/Card/Dialog/SegmentedControl...）
│   ├── ErrorBoundary.tsx   # 渲染期异常兜底（类似 Go recoverer 中间件）
│   ├── ErrorAlert.tsx      # 统一错误横幅与表单错误提示
│   ├── ConfirmDialog.tsx   # 通用二次确认弹窗（替代 window.confirm）
│   ├── CopyButton.tsx      # 一键复制按钮 + 状态反馈
│   ├── QRCodeCard.tsx      # 本地离线二维码渲染与 PNG 下载
│   ├── QRCodeDialog.tsx    # 二维码独立预览弹窗
│   ├── EmptyState.tsx      # 统一空状态
│   └── PageHeader.tsx      # 页面标题与操作区
├── app/                    # 应用壳层：路由表、鉴权守卫、侧边栏布局与 OrgContext
│   ├── routes.ts           # 前端路由模式与类型安全路径构造器（Single Source of Truth）
│   ├── router.tsx          # 声明式路由（/login、/onboarding、/orgs/:orgId/*）
│   ├── AuthGuard.tsx       # 会话守卫、全局 401 回跳与租户路由校验
│   ├── org-context.ts      # 当前组织上下文（类似 Go request context 中的 Principal）
│   └── ConsoleLayout.tsx   # 侧边栏、组织切换下拉、月度配额条、移动端抽屉
└── features/               # 按业务领域分包（对齐 Go internal/{identity,link,console}）
    ├── auth/               # 登录页、/me 与 /auth/providers、角色权限辅助
    ├── orgs/               # 首次无组织引导页、新建组织弹窗
    ├── links/              # 短链列表（游标分页/直达查询）、创建成功态+二维码、统计页
    ├── members/            # 成员列表/改角色/移出、邀请列表/登录入口复制/撤销
    └── api-keys/           # API Key 列表、按 scope 签发（明文一次性展示+cURL）、吊销
```

---

## 给 Java / Go 工程师的 6 个 React 核心概念对照

阅读代码时可以按这个顺序理解：

1. **服务端状态 vs 客户端状态（为什么用 TanStack Query 而不是 `useEffect`）**
   - 列表、详情、成员、统计都属于 **Server State**（事实在 PostgreSQL 里，浏览器只是缓存）。
   - `features/*/api.ts` 里的 `useQuery` / `useInfiniteQuery` / `useMutation` 用 `queryKey` 做缓存键（类比 Redis key `shortlink:link:<code>`），透传 `AbortSignal` 自动防竞态，写完后 `invalidateQueries` 精准失效。
   - 弹窗是否打开、输入框草稿属于 **UI State**，就近放在组件 `useState` 里即可。
2. **React Context 的正确边界（`src/app/org-context.ts`）**
   - 就像 Go 里只把 `Principal` 放进 `context.Context`，不要把业务数据塞进 Context；这里只放当前登录用户和当前 `:orgId` 对应的组织成员信息，供子页面通过 `useCurrentOrg()` 读取。
3. **路由即状态（`src/app/router.tsx`）**
   - 把当前组织和子页面放进 URL（`/orgs/:orgId/links/:code`），刷新不丢状态、浏览器前进后退天然可用，并且和 Go 后端 `?return_to=` + `webhandler.go` 的 SPA fallback 完美闭环。
4. **用 `key` 重置表单，而不是 `useEffect` 同步 props（`features/links/LinkFormDialog.tsx`）**
   - React 里最容易踩坑的是“在 `useEffect` 里 `setState`”。通过给子表单传 `key={editingLink ? editingLink.code : 'new'}`，打开不同记录时 React 会自动销毁重建干净状态。
5. **组合模式与 shadcn/ui 原语（`src/components/ui/`）**
   - Radix 解决无障碍焦点管理、ESC 关闭、Portal 遮罩层等底层难题，我们只在 `components/ui/` 里用 Tailwind + `cva` 定义视觉变体；代码完全留在仓库里，随时可看可改。
6. **组件测试思路（`features/*/*.test.tsx`）**
   - Testing Library 提倡像真实用户一样按角色和文案查元素（`getByRole`、`findByText`），不测内部 state 实现细节；`test-utils.tsx` 里的 `renderWithOrg` 类比 Go 的 `httptest` 测试夹具。

---

## 后续扩展新页面（例如阶段 14 审计日志）怎么加

只需四步，不改现有业务模块（更多规范见 [web/AGENTS.md](AGENTS.md)）：

1. 在 `src/lib/endpoints.ts` 注册后端端点，在 `src/app/routes.ts` 注册路由模式与构造函数；
2. 新建 `src/features/audit/`，放 `api.ts`（query keys + hooks）、`AuditLogsPage.tsx` 与对应测试；
3. 在 `src/app/router.tsx` 的 `/orgs/:orgId` 下挂载路由，并在 `src/app/ConsoleLayout.tsx` 的 `navItems` 数组里加导航入口；
4. 运行 `make web-check` 校验格式、类型、Lint、测试与构建。
