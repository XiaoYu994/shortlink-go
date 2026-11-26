# 0030. 手机号短信验证码登录（Phone OTP）与 `cmd/console` 垂直特性装配演进

- 状态：Accepted
- 日期：2026-11-26
- 相关：`0004`, `0010`, `0018`, `0019`, `0020`, `0021`, `0024`, `0028`, `0029`

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 登录插件接入 | 新增 `console.PhoneOTPAuthMethod` 直接实现 [ADR 0029](0029-modular-feature-plugins.md) 的 `AuthMethod` 接口（`kind: "phone_otp"`），暴露 `POST /api/v1/auth/{id}/send-code` 与 `POST /api/v1/auth/{id}/verify`，核验成功后复用 `SessionIssuer.IssueSession` 签发统一 `sl_session` 会话，核心认证底座零改动 |
| 身份标识与邀请隔离 | 手机号采用严格 E.164 格式（`^\+[1-9][0-9]{6,14}$`）；映射为 `identity.ExternalIdentity{Issuer: "urn:shortlink:phone", Subject: phone, Name: MaskPhone(phone)}`、`Email: ""`、`EmailVerified: false`，复用现有 `user_identities (issuer, subject)` 唯一键，不自动兑现邮箱邀请 |
| 短信发送抽象 | 新增独立通道包 `internal/sms`（对标 `internal/mailer`，纳入 `make arch-check` 守门），提供 `Sender` 接口与两种实现：`webhook`（走 `safehttp.Client` 防 SSRF + 端口白名单 + Bearer 认证，生产用）与 `log`（本地开发仅日志输出，启动打 Warn 提醒）；由 `SMS_PROVIDER` 单一事实来源控制启用 |
| 同步发码 + PG 挑战表 | 发短信走请求期**同步发送**（秒级反馈、失败即删挑战不扣冷却），挑战持久化在 PostgreSQL `phone_otp_challenges` 表（无 Redis 极简模式同样可用）；HMAC-SHA256 加盐存哈希、`pg_advisory_xact_lock` 防并发双发、冷却与小时频控返回 `429` + `Retry-After`、`FOR UPDATE` 行锁保证单码原子消费且单码最多错试 5 次即作废 |
| 能力发现扩展 | `AuthMethodDescriptor` 新增可选 `send_code_url` 与 `verify_code_url`（`omitempty`），前端 `LoginPage` 完全由 `GET /api/v1/capabilities` 动态渲染表单与倒计时，零硬编码路径 |
| `cmd/console` 垂直装配演进 | 将 `cmd/console/` 中原先分散在 `serve.go`、`worker.go`、`workermodule.go`、`features.go` 的外围特性按垂直特性文件内聚（`feature_cache.go`、`feature_analytics.go`、`feature_risk.go`、`feature_webhooks.go`、`feature_email.go`、`feature_phoneotp.go`），由各特性自持其 `TxHook`、控制面模块/认证插件与后台 `WorkerModule`（含 Handler/Client 构造），`serve.go` 与 `worker.go` 退回薄组合根 |

---

## 背景

1. **验证 ADR 0029 认证插件边界**：[ADR 0029](0029-modular-feature-plugins.md) 设计了两层认证抽象（`AuthMethod` + `SessionIssuer` 与 `OAuthAuthMethod`），专门预留了 `AuthFlowPhoneOTP`（`"phone_otp"`）以承接非跳转型表单登录，但当时只接入了 OIDC 与 GitHub 两个 OAuth 提供方，尚未经过真实非跳转协议的实战验证。
2. **治理 `cmd/console` 装配膨胀**：随着 `risk_scan`、`webhooks`、`email`、`analytics`、`cache` 五个外围特性陆续落地，`cmd/console/worker.go` 内联了大量 HTTP Client / Cipher / SMTP 构造，再把结果塞进 `workermodule.go` 的超薄壳结构体；`serve.go`、`features.go` 各自维护一套启用分支。新增第 6 个跨进程特性（Phone OTP）时，正是顺势将 `cmd/console/` 演进为按垂直特性内聚的合适时机。

---

## 决策

### 1. 手机号身份与安全不变量

- **严格 E.164 规范化（`internal/identity`）**：
  - `NormalizePhone(raw string) (string, error)` 去除首尾空白并校验必须满足 `^\+[1-9][0-9]{6,14}$`（总长 8–16 字节，全球唯一无歧义）；
  - `MaskPhone(phone string) string` 将中间位脱敏（如 `+8613800138000` → `+86****8000`），用作用户主档默认展示名与日志安全脱敏值；业务日志绝不输出明文手机号与验证码（仅显式开启 `SMS_PROVIDER=log` 开发模式时由 `LogSender` 打印验证码方便本地联调）。
- **与现有会话体系的零侵入整合**：
  - 验证码通过后，产出 `ExternalIdentity{Issuer: "urn:shortlink:phone", Subject: normalizedPhone, Name: MaskPhone(normalizedPhone)}` 并交给 `SessionIssuer.IssueSession`；
  - `users.email` 与 `user_identities.email` 按既有约束允许空串，`email_verified = false`，不会误兑现基于邮箱的组织邀请；用户登录后可走 `/onboarding` 自助创建组织并正常使用全部控制台能力。

### 2. 挑战持久化与防刷状态机（`migrations/017_phone_otp_challenges`）

#### 2.1 为什么用 PostgreSQL 表而非 Redis？
- 守住 [ADR 0029](0029-modular-feature-plugins.md) 阶段 D 的“无 Redis 极简单 PG 模式”承诺：即使 `REDIS_URL=""`，手机号登录依然完整可用；
- 利用 PostgreSQL 唯一部分索引、行锁与事务级 advisory lock，天然消除“并发双发绕过冷却”与“并发重放单码多次建会话”两类竞态；
- `phone_otp_challenges` 属于登录前平台级认证表（与 `sessions`、`users` 一致，不含 `tenant_id`），不授予 `shortlink_tenant` 角色权限，也不挂载租户 RLS。

#### 2.2 发码两阶段编排（绝不在 DB 事务内做外网 I/O）
1. **短事务写挑战（`CreatePhoneOTPChallenge`）**：
   - 使用 `pg_advisory_xact_lock(hashtextextended(phone, 0))` 仅对同一手机号串行化；
   - 借助 `(phone, created_at DESC)` 索引检查冷却期（默认 `60s`）与近 1 小时发送上限（默认 `5` 次）；超限时返回带精确剩余秒数的 `ErrOTPCooldown` / `ErrOTPRateLimited`，HTTP 映射为 `429` + `Retry-After`；
   - 将该手机号此前未消费的活跃挑战置 `invalidated_at = now()`，再插入新挑战（16 字节 `crypto/rand` 随机盐 + `HMAC-SHA256(salt, phone + ":" + code)` 32 字节摘要），受唯一部分索引 `uq_phone_otp_active_phone` 兜底；
2. **事务外同步发短信（`sms.Sender.SendOTP`）**：
   - 短信验证码要求秒级低延迟且发送失败必须立刻反馈前端，因此**不走 Outbox 异步任务队列**（与可容忍秒级延迟的 `invitation.email` 形成明确分工）；
   - 若发短信失败或请求 `ctx` 取消，使用短超时独立 context 删除刚插入的未送达挑战行，避免浪费用户冷却期与小时额度；
   - 发码与核验两个 `POST` 端点均挂载 `AuthToolkit.IPRateLimit`（防扫号刷账单）与 `CrossOriginProtection`（防跨站触发发码与 Login CSRF）。

#### 2.3 原子核验与防暴力破解（`VerifyPhoneOTPChallenge`）
- 事务内以 `SELECT ... WHERE phone = $1 AND consumed_at IS NULL AND invalidated_at IS NULL FOR UPDATE` 锁住该手机号唯一活跃挑战；
- 未找到或已过期（顺手置 `invalidated_at = now()`）统一返回 `identity.ErrOTPInvalid`（HTTP `401 invalid verification code`），不暴露“未发码/已过期/码错”差异；
- 在 Go 侧用 `hmac.Equal` 常量时间比对：
  - 不匹配时原子 `attempts = attempts + 1`，当 `attempts + 1 >= max_attempts`（默认 `5`）同时置 `invalidated_at = now()` 锁定该码；
  - 匹配时原子置 `consumed_at = now()`，随后提交事务并调用 `IssueSession`。并发重放请求在 `READ COMMITTED` 下等锁释放后重新评估 `WHERE` 条件返回 0 行，严格保证一码仅生效一次。

#### 2.4 后台过期清理（`phone_otp.cleanup`）
- `console worker` 通过 `phoneOTPWorkerFeature` 定期按 `idx_phone_otp_expires_at` 分批清理超过保留期（`WORKER_OTP_RETENTION`，默认 `24h`）的历史挑战记录；
- 与 `sessions.cleanup_expired` 一致，`worker` 不需要重复配置 `SMS_*` 环境变量即可执行清理，避免跨进程重复配置，也能清理“先开后关”后的存量挑战行。

### 3. 短信发送通道 `internal/sms`

- 单一事实来源：`SMS_PROVIDER` 为空时关闭手机号登录；支持：
  - `webhook`：通过 `safehttp.NewClient`（`DisableRedirects: true`、支持 `SMS_ALLOWED_PORTS` 与 `devtest` `--allow-test-host`）向 `SMS_WEBHOOK_URL` 投递 JSON `{"to","code","ttl_seconds"}`，可选 `SMS_WEBHOOK_TOKEN` Bearer 认证；
  - `log`：本地开发零外部依赖模式，通过 `slog` 输出掩码手机号与验证码，启动时输出 `WARN` 日志提示禁止用于生产。
- `Makefile` 的 `arch-check` 同步禁止 `cmd/redirector` 依赖 `internal/sms`，并约束 `internal/sms` 仅允许依赖 `internal/safehttp` 与 `internal/logx`。

### 4. `cmd/console` 垂直特性装配演进

对照 Java Spring Boot 按垂直业务模块拆分 `*AutoConfiguration` 的思路，在 `cmd/console` 包内按特性拆分为自包含文件，不引入反射、`init()` 隐式注册或额外多层 `internal/` 中间层：

- `feature_cache.go`：Redis 缓存 `TxHook` + `cache.reconcile` Worker 模块；
- `feature_analytics.go`：点击统计 `TxHook` + `StatsModule` + `link.purge_stats` 与 stats 分区启动/维护 Worker 模块；
- `feature_risk.go`：异步风控 `TxHook` + 内部自建 `SafeHTTPClient`/`RuleScanner`/`HostLimiter`/`VerdictCache` 的 `WorkerModule`；
- `feature_webhooks.go`：Webhook `TxHook` + Cipher 解析与 `WebhooksModule` + 内部自建 `webhook.Handler` 与投递清理的 `WorkerModule`；
- `feature_email.go`：异步邀请邮件 `TxHook` + 内部自建 `SMTPSender` 与邮件处理器的 `WorkerModule`；
- `feature_phoneotp.go`：手机号 OTP `sms.Sender` 构造 + `PhoneOTPAuthMethod` + `phone_otp.cleanup` Worker 模块；
- `workermodule.go` 专注保留 `WorkerRegistrar` / `WorkerModule` 通用装配底座与核心平台维护模块（`coreWorkerModule`）；`serve.go` 与 `worker.go` 收敛为清晰声明式的组合根。

---

## 不做什么

1. **不做多身份账号自动合并（手机号与已有邮箱账号绑定）**：延续 [ADR 0018](0018-users-orgs-and-rbac.md) 的 `(issuer, subject)` 独立身份模型；后续若支持个人设置页绑定/解绑手机号，另立 ADR。
2. **不把短信发送塞进 Outbox 异步任务队列**：验证码登录属于强交互实时路径，同步发送才能把网关故障立即反馈给用户并回滚冷却。
3. **不在仓库引入第三方商业短信 SDK（阿里云/Twilio 等）**：通过通用 `webhook` 适配器和 `sms.Sender` 接口隔离供应商差异，保持零外部商业 SDK 依赖。

---

## 怎么验证

1. **迁移可逆与索引验证**：`make test-db` 验证 `017` 迁移 `up -> down -> up`；在集成测试中验证唯一活跃挑战约束、冷却/小时频控、5 次错试作废、并发重放仅 1 次成功与分批清理。
2. **单测与负向副作用断言**：
   - `internal/identity`：E.164 合法/非法边界与 `MaskPhone`；
   - `internal/sms`：`WebhookSender` 鉴权、超时、`ctx` 取消零网络调用、SSRF/端口拦截与脱敏；
   - `internal/console`：`PhoneOTPAuthMethod` 全流程（发码、冷却 429 + `Retry-After`、发短信失败回滚挑战、CSRF 拦截、错码 401、成功建会话写 Cookie 并访问 `/api/v1/me`、取消 `ctx` 零副作用）；
   - `cmd/console`：各垂直特性独立构造校验与装配单测。
3. **前端与端到端冒烟**：
   - `make web-check`：Vitest 覆盖 `LoginPage` 仅手机号、仅 OAuth 与混合共存三种形态、倒计时与错误提示翻译；
   - `make selfcheck`：`scripts/smoke.sh` 启动本地 SMS Webhook 桩，走完 `GET /api/v1/capabilities` → `POST /api/v1/auth/phone/send-code` → 从桩读取验证码 → `POST /api/v1/auth/phone/verify` → `GET /api/v1/me` → `POST /api/v1/orgs` 完整闭环，并验证 `arch-check` 与极简模式全绿。
