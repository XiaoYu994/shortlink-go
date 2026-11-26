# AGENTS.md

本文件只保留给在本仓库工作的 AI 助手必须遵守的高杠杆边界、全局反模式、验证命令与文档指针；完整 Go 工程规范（含 Java 对照与从 0 起新项目模板）见 [docs/conventions.md](docs/conventions.md)。

## 协作边界

- **项目背景**：用短链接服务学 Go，用户有 Java 背景。解释概念与设计取舍时对照 Java；不确定的标准库或第三方行为先查证代码或文档再讲，不凭印象下结论。
- **未收到明确修改指令时（纯提问、讨论、评审），只给结论与方案，不擅自改代码。**
- **对外动作**：推送、开 PR 除非用户明确要求，不要自己执行。
- **设计沉淀**：关键设计先立 ADR（`docs/adr/`，在「不做什么」里写清明确接受的风险边界，供后续 Review 直接拒收同类加码），完成后同步更新受影响的 `docs/`、本文件与包注释。

## 全局反模式红线（工具拦不住，实现与 Review 必查）

1. **单一事实来源（防双存配置与重复计算）**：同一份配置或请求期衍生值（如可信代理链、客户端 IP、请求 ID）只在归属底座持有并解析一次，下游经 `context` 或显式接口消费；不在多个结构体复制配置或重复计算。
2. **禁止留“伪兼容”壳与“半死分支”**：区分**真兼容**（外部 `/r/{code}` 短链、存量库表数据、滚动发布窗口）与**伪兼容**（同仓可控的 `web/`、`smoke.sh` 与旧测试）。升级接口、路由与字段时同步迁移全部仓内调用方并删除旧入口与双存字段，不用假默认值掩盖语义变化，非 `_test.go` 文件中不留仅供测试或旧调用方使用的兼容壳。
3. **改共享语义时回溯整条调用链，禁注释合理化异常分支**：改动共享辅助逻辑或取消/超时/限流语义时，从请求入口重新走查边界（尤其调用方 `ctx` 已取消/超时）。遇到“按理不该走到这里”的状态分支，追根在入口拦截，禁止补一句注释把反常路径说圆。
4. **局部改动对齐邻近实现**：修改或新增函数前先通读当前文件的注释习惯、错误包装前缀、日志埋点（`logx` / `RequestEvent`）与既有 helper 用法，不在同一文件混用等价风格。
5. **测试替身与断言必须校验“不该发生的副作用”**：`fake`/stub 测取消、超时、限流、降级等边界时，不仅断言返回值或指标，还要断言不该发生的副作用确实未发生（如取消后下游 `calls == 0`、未占并发槽位、未写缓存）。
6. **不向核心写事务或核心 Server 硬塞外围特性**：新增外围能力或登录方式复用既有模块化扩展点（`postgres.TxHook`、`console.AuthMethod`、`console.Module`、`WorkerModule` + `/api/v1/capabilities`）；控制面多步写在事务闭包内编排单职责 `xxxTx` 小函数，禁止多业务域巨型写 CTE 与“跳号插参”。
7. **用事实说话，禁止为纸面“假设 P1”过度防御**：安全/越权、永久脏数据、死锁与必现崩溃等确定性 P0 直接修；涉及性能瓶颈、并发退化、极低概率时序竞态或大规模积压等风险，**改动代码或新增防御机制前必须先用实测举证**（压测、benchmark、`pprof`/`trace`、`EXPLAIN (ANALYZE, BUFFERS)` 或稳定复现用例），确认真实存在风险再动手。优先用调参数、放宽契约或单测守门等“减法解”；若补丁复杂度高于风险本身，直接记入 ADR「不做什么」或注释接受该边界，不为理论边界叠状态机、自研校验框架或冗余兜底链路。

## 验证、AI Review 与提交

- **验证命令**：
  - `make verify`：Go 侧快速检查（`fmt-check`、`go vet`、`go test -race`、`arch-check`、`golangci-lint`、`build`，不依赖 Docker）。
  - `make web-check`：改动 `web/` 时必跑（Prettier、`tsc`、ESLint 含依赖边界、Vitest、`vite build`）。
  - `make selfcheck`：重建 `TEST_DATABASE_URL` schema 并跑完整 `verify` + `test-integration` + 真实进程 `smoke`（**本机已有 Postgres/Redis/Dex 时不要执行 `make deps-up`**）。改动 Go 行为报告完成前必跑；改了 HTTP 契约同步更新 `docs/api.md` 与 handler 测试。
- **AI Review（提交前自查 & 独立审查）**：通读完整 `git diff --staged`（或目标提交），**写法检查**（对照全局红线与相应文档）与**行为检查**（对照 ADR 与 roadmap）分开报告；行为检查挑 1 条核心异常/边界路径顺着完整调用链走查即可。所有意见须明确分级（**P0** 安全/越权/永久脏数据/死锁；**真 P1** 高频主路径明显缺陷或已复现竞态；**待实证/低收益建议**），非 P0 项必须写清**触发前提、最坏后果与预估改动成本**，不得把需多重罕见条件才触发的假设标成 P1 倒逼加补丁；是否修改或记为已知取舍由用户拍板，**Review 原则上不超过 2 轮**（第 2 轮仅复核上轮改动是否引入新问题，禁止发散加码）。
- **提交格式**：`<type>: <摘要>`（中文动词开头、不加句号、50 字以内；`type` 取 `init` / `feat` / `fix` / `test` / `refactor` / `docs` / `chore`；一提交只做一件事）。

## 按需读取地图

- [docs/conventions.md](docs/conventions.md)：完整 Go 工程规范、日志（`logx`）、测试（`synctest`）与新项目参考。
- [internal/postgres/AGENTS.md](internal/postgres/AGENTS.md)：存储层、SQL、RLS 与分区红线（改动 `internal/postgres/` 或 `migrations/` 时必读）。
- [web/AGENTS.md](web/AGENTS.md) & [web/README.md](web/README.md)：前端控制台约束与导读（改动 `web/` 时必读）。
- [docs/README.md](docs/README.md)：架构（`architecture.md`）、API（`api.md`）、运维（`runbook.md`）、路线图（`roadmap.md`）、ADR 索引（`adr/README.md`）与复盘（`postmortems/`）。
