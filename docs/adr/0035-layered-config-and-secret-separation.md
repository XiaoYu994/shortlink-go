# 0035. 分层配置架构（YAML + 密钥走 Env）与配置精简

- 状态：Accepted
- 日期：2026-11-27
- 相关：`0013`, `0018`, `0020`, `0024`, `0026`, `0027`, `0029`, `0030`, `0032`, `0034`

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 分层与优先级 | **代码默认值 < 可选共享 YAML（`--config` 优先于 `SHORTLINK_CONFIG`） < 环境变量 < CLI Flag**；未指定配置文件时可仅靠默认值与环境变量零配置启动 |
| 解析器选型 | 仅引入零传递依赖的 `go.yaml.in/yaml/v3`，**不引入 `spf13/viper`**；在 `internal/config` 显式分层解码与严格校验 |
| 跨进程隔离 | 顶层 `config.File` 使用 `yaml.Node` 延迟解码各进程专属节，校验顶层 key 与单文档约束；`cmd/redirector` 只解公共节与 `redirector` 节，继续守住 `make arch-check`（[ADR 0024](0024-process-boundary-and-ops-isolation.md)） |
| 密钥安全边界 | 10 个连接串与密钥 **仅允许从环境变量读取**，YAML DTO 结构体不声明对应字段，且各节开启 `KnownFields(true)`——误将密钥写入 YAML 启动即失败 |
| 同步做减法 | 精简重复与过细变量：统一 `console.public_url`（移除 `OIDC_REDIRECT_URL`、`GITHUB_REDIRECT_URL`、`SMS_SECURE_COOKIES`）；统一 `trusted_proxies` / `TRUSTED_PROXIES`（移除 `TRUST_PROXY_HEADERS`、`TRUSTED_PROXY_CIDRS`）；统一 `outbound.allowed_ports`（移除 3 份 `*_ALLOWED_PORTS`）；Worker 保留期由 8 档收敛为 4 档；风控黑白名单改用原生 YAML 列表/映射；运行时环境变量由 95 个收口至 **23 个**（另加 2 个 `TEST_*`） |

---

## 背景

随着服务从单一跳转进程演进到多租户 RBAC、Phone OTP、Webhook、异步风控 Funnel 与 OTel 链路追踪，单纯依赖扁平 `.env` 环境变量出现了四类明显摩擦：

1. **变量数量膨胀且主次不分**：`.env.example` 累积到 97 个环境变量（其中约 66 个属于连接池、超时、调度、保留期、风控规则等低频调优旋钮），不仅新接入难分清哪些必填，还会因为 `.env.example` 取消注释抄入默认值而覆盖代码演进（如 `Makefile` 为热更新被迫用 `DEV_DRAIN_DELAY` 覆盖 `.env` 里的 `DRAIN_DELAY=5s`），且本地 `.env` 极易与示例漂移。
2. **扁平环境变量硬塞结构化数据**：`RISK_BLOCKED_HOSTS`（`[2001:db8::1]:malware`）、`RISK_BLOCKED_KEYWORDS`、`RISK_ALLOWED_HOSTS`、`TRUSTED_PROXY_CIDRS` 被迫在环境变量里手写逗号/冒号解析 DSL。
3. **跨进程与多特性间的重复配置**：
   - 控制台公网入口被拆成 `CONSOLE_PUBLIC_URL`、`OIDC_REDIRECT_URL`、`GITHUB_REDIRECT_URL` 与 `SMS_SECURE_COOKIES` 4 份，但回调路径按 [ADR 0029](0029-modular-feature-plugins.md) 已固化为 `/api/v1/auth/{id}/callback`，且 `Secure` 属性本质只取决于入口是否 `https://`；
   - 出站防 SSRF 端口白名单重复配置了 `RISK_ALLOWED_PORTS`、`WEBHOOK_ALLOWED_PORTS` 与 `SMS_ALLOWED_PORTS` 3 份；
   - `TRUST_PROXY_HEADERS` + `TRUSTED_PROXY_CIDRS` 在 `cmd/redirector` 与 `cmd/console` 各自重复解析与校验；
   - Worker 清理保留期被拆成 8 个单位不统一的变量（6 个小时串 + 2 个天数）。
4. **Java 背景迁移时的选型约束**：在 Spring Boot 中通常使用 `application.yaml` 承载分层结构化配置，再用环境变量覆盖连接串与密钥。在 Go 生态如果盲目引入 `spf13/viper`，会带来大量传递依赖、全局单例污染并发单测、以及静默把 map key 转小写（破坏规则精度）等暗坑；需要一套贴合 Go 显式风格且保持依赖干净的方案。

---

## 决策

### 1. 分层配置与优先级

三进程与运维 CLI 共享同一套分层模型（优先级从低到高）：

1. **代码内置默认值**：作为唯一默认来源；`.env.example` 不再把默认值取消注释写死，仓库新增可提交的 `config.example.yaml` 作为参考模板（由单测断言其与代码默认值严格一致）。
2. **可选共享 YAML 配置文件**：通过 CLI `--config <path>`（最高）或环境变量 `SHORTLINK_CONFIG` 指定路径。两者均为空时不读取任何文件，纯按代码默认 + 环境变量即可启动（兼顾极简单 PG 部署与冒烟测试）。
3. **环境变量（仅 23 个运行时变量 + 2 个测试变量）**：
   - **连接串与密钥（10 个，Env-only，禁止写入 YAML）**：
     `DATABASE_URL`、`REDIS_URL`、`VISITOR_SALT`、`OIDC_CLIENT_SECRET`、`GITHUB_CLIENT_SECRET`、`SMS_WEBHOOK_TOKEN`、`WEBHOOK_SECRET_KEY`、`SMTP_PASSWORD`、`RISK_GSB_API_KEY`、`RISK_LLM_API_KEY`。
   - **配置入口与高频运行期覆盖（13 个，非空时覆盖 YAML/默认值）**：
     `SHORTLINK_CONFIG`、`ADDR`、`OPS_ADDR`、`CONSOLE_ADDR`、`CONSOLE_OPS_ADDR`、`WORKER_ADDR`、`LOG_LEVEL`、`LOG_FORMAT`、`DRAIN_DELAY`、`TRUSTED_PROXIES`、`OTEL_EXPORTER_OTLP_ENDPOINT`、`FEATURE_ANALYTICS`、`FEATURE_RISK_SCAN`。
   - **集成测试专用（2 个）**：`TEST_DATABASE_URL`、`TEST_REDIS_URL`。
4. **CLI Flags**：`--config`、`--allow-test-host` 等进程级运行期参数。

### 2. YAML 架构、`yaml.Node` 跨进程隔离与严格校验

1. **不引入 Viper，直接用 `go.yaml.in/yaml/v3`**：零传递依赖、无全局状态、并发单测友好；自定义 `config.Duration` 仅接受字符串时长（如 `"5s"`、`"168h"`），拒绝无单位裸整数，避免 YAML `5` 被静默解析成 5 纳秒。
2. **顶层分节与跨进程隔离**：`internal/config.File` 定义 12 个顶层 `yaml.Node` 节：
   `features`、`database`、`http`、`cache`、`trusted_proxies`、`outbound`、`log`、`tracing`、`redirector`、`console`、`worker`、`smtp`。
   - `LoadFile` 使用 `yaml.NewDecoder` 开启 `KnownFields(true)` 校验顶层 key 并拒绝多文档 YAML；
   - `cmd/redirector` 只解码公共节与 `redirector` 节，不引入也不解码 `console` / `worker` / `smtp` / `outbound`，从而继续满足 `make arch-check`（`internal/config` 仅依赖标准库、`logx`、`pgdb` 与 `yaml/v3`）；
   - `DecodeSection` 在节点为空或 `!!null` 时直接返回（不覆写调用方预填默认值），非空时通过重编组后 `dec.KnownFields(true)` 严格拒绝任何拼写错误或误放进 YAML 的密钥字段。
3. **运维预检命令**：新增 `console config dump [--config FILE]`，加载并同时校验 `ConsoleConfig` 与 `WorkerConfig`，通过既有 `LogValue()` 脱敏输出最终生效配置，便于发布前自查。

### 3. 同步精简与单一事实来源

1. **统一 `console.public_url`**：
   - OIDC 与 GitHub 登录启用时要求已配置 `console.public_url`，回调 URL 自动推导为 `{public_url}/api/v1/auth/{id}/callback`，`SecureCookies` 由 `https://` 自动推导；删除 `OIDC_REDIRECT_URL` 与 `GITHUB_REDIRECT_URL`；
   - Phone OTP 的 `SecureCookies` 同样由 `console.public_url` 是否 `https://` 自动推导；删除 `SMS_SECURE_COOKIES`；
   - `worker` 通过 `config.LookupMappingString(file.Console, "public_url")` 读取同一基址用于邀请邮件登录链接，删除 `CONSOLE_PUBLIC_URL`。
2. **统一 `trusted_proxies` / `TRUSTED_PROXIES`**：
   - 收口到 `internal/config.ProxyConfig` 与 `ParseTrustedProxies`：空切片 = 不信任转发头；`["*"]`（或 Env `TRUSTED_PROXIES=*`）= 单跳代理模式；CIDR/IP 列表 = 多跳右向剥离模式；混写 `*` 与 CIDR 启动报错；
   - 删除 `TRUST_PROXY_HEADERS` 与 `TRUSTED_PROXY_CIDRS`，消除 `redirector` 与 `console` 的重复解析代码。
3. **统一 `outbound.allowed_ports`**：
   - 合并供 `risk`、`webhook`、`sms` 三个出站模块共用（留空由 `safehttp` 默认仅放行 `80, 443`）；删除 `RISK_ALLOWED_PORTS`、`WEBHOOK_ALLOWED_PORTS` 与 `SMS_ALLOWED_PORTS`。
4. **收敛 Worker 保留期（8 → 4）**：
   - 收口为 `worker.retention.{short, long, stats_days, audit_days}`：
     - `short`（默认 `168h`）：`sessions`、`done_jobs`、`webhook_deliveries`；
     - `long`（默认 `720h`）：`dead_jobs`、`invitations`；
     - `stats_days`（默认 `90`）、`audit_days`（默认 `180`）；
     - 短信验证码清理周期固定使用 `identity.DefaultOTPRetention`（`24h`）。
5. **风控黑白名单原生 YAML 化**：
   - 将 `internal/risk` 的字符串解析函数替换为 `NormalizeHostList([]string)`、`NormalizeHostRules(map[string]string)` 与 `NormalizeKeywordRules(map[string]string)`，在 YAML 中直接以列表和键值映射书写。

---

## 不做什么

1. **不做运行期配置文件热重载（`fsnotify` / `SIGHUP`）**：进程遵循不可变启动配置模型，配合编排器滚动重启变更配置，避免动态改连接池/认证插件/定时周期引发的并发状态复杂度（[AGENTS.md](../../AGENTS.md) 红线 #7）。
2. **不在数据库建动态配置中心**：现阶段没有线上实时修改系统配置的强诉求，不引入配置表与管理后台（风控提示词未来扩展缝已在 [ADR 0034](0034-layered-risk-scan-and-ai-judge.md) 预留）。
3. **不为被撤除的 60+ 低频调优环境变量与旧命名保留兼容壳**：仓库尚在演进期，全部调用方（`docker-compose.yml`、`scripts/smoke.sh`、`docs/` 与单测）同仓可控，按 `AGENTS.md` 红线 #2 一步到位迁移并删除旧变量解析分支。

---

## 怎么验证

1. **单元测试**：
   - `internal/config` 覆盖 YAML 解码、`KnownFields(true)` 拒绝未知顶层/嵌套字段与误写密钥、`null` 节不覆盖默认值、`Duration` 拒绝裸整数、`ProxyConfig`（空/`*`/CIDR/混写报错）以及 Env 覆盖 YAML 优先级；
   - `cmd/redirector` 与 `cmd/console` 覆盖零配置默认、YAML+Env 分层覆盖、密钥禁止写进 YAML、基于 `console.public_url` 自动推导回调与 Cookie 安全标记、直接加载仓库根目录 `config.example.yaml` 断言与代码默认值一致、`LogValue()` 脱敏与 `console config dump`。
2. **架构边界与全量自检**：
   - `make verify`（含 `make arch-check`、`golangci-lint`、`-race` 单测）全绿；
   - `make selfcheck`（含 `scripts/smoke.sh` 全功能 YAML 模式、`console config dump` 验证及无 YAML 极简单 PG 模式）全绿。

---

## 后果

- **收益**：`.env.example` 从 97 项压缩到十余行必需连接串与按需密钥，消除本地默认值漂移与 `host:reason` 字符串 DSL；`console.public_url`、可信代理与出站端口实现真正的单一事实来源；通过 `yaml.Node` 与 `KnownFields(true)` 在不破坏 `redirector` 二进制隔离的前提下提供配置拼写与密钥防误配护栏。
- **代价**：旧有基于纯环境变量调优（如 `WORKER_CONCURRENCY`、`OIDC_ISSUER` 等）的部署方式需迁移至 `config.yaml` 配合密钥环境变量，并在升级时同步检查 `.env` 中的已删除变量。
