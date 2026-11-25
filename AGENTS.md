# AGENTS.md

本文件写给在本仓库工作的 AI 助手（Claude Code、Cursor 等），同时也是团队成员共用的代码约定。
它是活文档：学习过程中约定有变化，就回来改这里，不要另起一份。

## 项目是什么

- 学习项目：用短链接服务学 Go。用户有 Java 背景，配套学习笔记在 Notion。
- `docs/roadmap.md` 的路线图就是学习进度，完成一项勾一项。
- 学到新东西、改了约定、发现更好的写法时，同步更新 README、`docs/`、本文件和各包的包注释。

## AI 的协作方式

- 用户自己写业务代码。AI 负责讲解、给提示、做 code review；除非用户明确要求，
  不要直接替用户实现路线图里的功能。
- 解释概念时对照 Java：Java 里是什么样、Go 里为什么不同。
- 发现代码问题时先说问题再说建议；给出示例代码时解释每一行，避免大段直接生成。
- 不确定的 API 行为先查证再讲，不凭印象下结论。

## 代码约定

### 注释

- 单行注释用 `//`。
- 多行注释用 `/* ... */`，块内每行开头不再加 `*`。
- 包注释写在包的主文件顶部（通常是 `<pkg>.go`、`store.go`、`server.go` 或 `main.go`），以 `Package 包名` 开头（main 包以 `Command 命令名` 开头）。
- TODO 标注对应课程，例如 `// TODO(第 7 课): ...`。
- 注释写"为什么"和"注意什么"，不重复代码本身已经说清楚的内容。
- SQL 组织与数据库纪律：
  - 多行 SQL 语句抽到文件顶部的包级 `const`，避免长 SQL 打断方法主流程；单行短 SQL 可就近内联。
  - 新增索引或改动查询/清理 SQL 时，在合成数据量上跑 `EXPLAIN (ANALYZE, BUFFERS)` 确认真正命中预期索引；禁止用 `OR` 混合有索引列与无索引列导致退化全表扫描。
  - 后台清理与租约回收 SQL 一律带 `LIMIT` 分批执行；在范围分区表（三张日统计表、`audit_logs`）上分批删改必须依据完整主键元组，禁止使用跨分区不唯一的 `ctid`；租约回写必须校验单调递增 Fencing Token（如 `jobs.lock_version`），不只依赖进程级 `WorkerID`；分区 DDL 必须在事务内设定 `SET LOCAL lock_timeout = '3s'` 且不得触碰 `*_default` 兜底分区。
  - 避免给高频更新计数列（如 `tenants.link_usage`、`link_usage_month`）建索引，保留 PostgreSQL HOT Update 优化；仅在不涉及索引列更新时才可通过调整 `fillfactor` 配合 HOT 优化（如 `links.clicks` 被 `idx_links_tenant_clicks` 索引，未拆出独立计数表前设 `fillfactor` 无法触发 HOT）。

### 命名与文件

- 文件名全小写，多单词直接连写（`customdomain.go`），不用 `doc.go`、`utils.go`、
  `common.go` 这类看不出内容的通用名。
- 文件名要能说明内容：放 Base62 的文件叫 `base62.go`，放存储实现的叫 `store.go`。
- 包名短小全小写，按业务领域分包（`link`、`tenant`、`analytics`），
  不用 `util`、`common`、`base` 这类万能包，也避开和标准库重名（`http`、`context`）。
- 不按 Java 习惯做技术分层拆包（如 `handler/`、`middleware/`、`dto/`）；先在包内拆小结构体解耦依赖，出现明确依赖分叉时再按底座/业务边界拆包（见 `docs/adr/0019-httpapi-package-evolution.md`）。

### 路由

- 管理 API 带版本前缀，当前是 `/api/v1`；不兼容改动时新增 `/api/v2`，旧版本按计划保留。
- 公开短链固定在 `/r/{code}`，不加版本：短链是已经发出去的永久地址，不能随 API 版本失效。
- `/healthz`、`/readyz` 等运维端点不版本化。
- 短码和路由分属不同路径命名空间，天然隔离。

### 错误处理

- 哨兵错误用 `errors.New`，包装用 `fmt.Errorf("...: %w", err)`；调用方用 `errors.Is` / `errors.As` 判断（Go 1.26 起也可以用 `errors.AsType`）。
- 错误只处理一次：要么返回，要么在这里处理，不既打日志又返回。
- 不用 panic/recover 模拟 try-catch。recover 只放在中间件和 goroutine 入口。

### 并发

- 每个 goroutine 都要能说清楚它什么时候退出。
- 数据库、Redis、HTTP 调用都传 ctx，并设置超时。
- 共享状态加锁或用 atomic；带锁的结构体一律用指针接收者。
- 测试一律带 `-race`（Makefile 已配置）。

### 接口与泛型

- 接口由使用方定义，尽量小（1 到 3 个方法）。
- 透明装饰器（`cache`、`storemetrics`）要透传被装饰对象的完整能力，接口会超过 3 个方法；
  按基础、管理、统计拆成几组，由组合接口汇总；不要为凑方法数再加一层转发适配器。
- 先用具体类型写；出现第二个实现、或测试需要替换外部依赖时，再抽接口。
- 泛型同理：先用具体类型写两遍，重复明显了再泛型化。

### 测试

- 表驱动测试；HTTP handler 用 httptest；环境变量用 `t.Setenv`。
- 依赖外部服务的测试，没有对应环境变量时 `t.Skip`。
- 集成测试用 `make test-integration` 跑：目标会重建测试库、执行迁移，再带 `TEST_*`
  跑 `go test -race`。`make test` 直接跑，则依赖 `.env` 里的 `TEST_*` 和已准备好的库。
- `make smoke` 启动真实进程验证创建、跳转、过期和错误路径；行为改动至少跑一次。
- CI 配置在 `.github/workflows/ci.yml`，push 到 main 和 PR 会跑 `make verify`、
  `make test-integration` 和 `make smoke`。

## 提交前检查

- `make verify`：gofmt 校验（只检查、不改文件）、go vet、go test -race（单测）、
  `make arch-check`（静态依赖边界检查）、golangci-lint、build，必须全部通过。不依赖 Docker。
- `make web-check`：改动 `web/` 前端代码时必须通过（Prettier 校验、`tsc --noEmit`、
  ESLint、Vitest、`vite build`）；前端代码约定详见 `web/AGENTS.md`。
- `make selfcheck`：重建 `TEST_DATABASE_URL` 的 schema（迁移升到最新、降到空、再升到最新），
  跑完整检查、集成测试和真实进程冒烟测试。需要 PostgreSQL、Redis 和 Dex（OIDC）可连；用项目自带的
  compose 时先跑 `make deps-up`。agent 报告完成前必须跑这一条；只跑 `make verify`
  没有验证真实 HTTP 行为。
- 改了 HTTP 行为，同步更新 `docs/api.md` 和对应的 handler 测试。
- 推送、开 PR 是对外动作，除非用户明确要求，不要自己推。
- AI review：提交前把 `git diff --staged` 从头读一遍。AI 写代码容易顺着当前思路走到局部最优，
  所以这一遍要跳出实现过程来看：有没有更简单的做法，和仓库已有写法、本文件约定是否一致，
  是不是只给眼前这一处打了补丁。发现问题先告诉用户，再决定改不改。
  写法和行为分开看：写法对照本文件；行为对照对应的 ADR（决策、不做什么、怎么验证）
  和 roadmap 条目逐条核对。代码写得规整不代表行为做对了，两边的结论分开报告。

## 提交规范

- 格式：`<type>: <摘要>`。摘要用中文，动词开头，不加句号，50 字以内。
- type 取值：

| type | 用途 | 例子 |
| --- | --- | --- |
| init | 项目或模块的初始化 | `init: 初始化短链接项目骨架` |
| feat | 新功能 | `feat: 实现 Base62 编解码` |
| fix | 修 bug | `fix: 短码解码未校验非法字符` |
| test | 只改测试 | `test: 补 Base62 表驱动测试` |
| refactor | 重构，行为不变 | `refactor: 抽出 LinkStore 接口` |
| docs | 文档、注释 | `docs: 补充 AGENTS.md 提交规范` |
| chore | 依赖、Makefile、配置 | `chore: 升级 go-redis` |

- 一次提交只做一件事。学习阶段每完成一个小练习就提交一次，方便回看每一步。
- 需要说明背景时，空一行后写正文：为什么改、有什么影响。例如：

```txt
feat: 实现 Base62 编解码

解码要校验非法字符和 int64 溢出，短码来自用户输入，
按不可信数据对待。
```

- 提交前 `make verify` 必须通过；改过 HTTP 行为再跑 `make selfcheck`。
- 个人学习阶段直接在 main 上提交；多人协作时用 `feat/xxx` 之类的分支。

## 文档地图

- `README.md`：项目概览、快速开始、文档入口。
- `docs/`：架构、API、运行手册、路线图、ADR 与问题复盘（`docs/postmortems/`）。
- `AGENTS.md`（本文件）：全局协作方式、Go 代码约定与提交规范。
- `web/AGENTS.md`：前端控制台的分层边界、React/TypeScript 约定与检查要求。
- `web/README.md`：前端启动命令、目录对照与 React 学习导读。
- 各包的包注释：这个包做什么、对应哪一课。
