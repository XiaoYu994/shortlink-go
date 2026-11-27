# 0034. 分层 URL 风控漏斗（规则 + Safe Browsing + HTML 净文本提取 + OpenAI 兼容 AI 研判）与死信告警治理

- 状态：Accepted
- 日期：2026-11-27
- 相关：`0020`, `0025`, `0027`, `0030`, `0031`, `0032`, `0033`

## 背景

在 [ADR 0020](0020-background-jobs-and-async-risk-scan.md) 落地后，异步风控已经具备 PostgreSQL Outbox、租户公平抢占、拨号防 SSRF 与 Redis fail-open 结果缓存，但针对公网场景仍存在两类实质问题：

1. **底座可靠性与可观测性缺口**：
   - `ValidateURL`（短链业务写入入口）允许租户写入私网 IP、非 `80`/`443` 端口、不存在域名或超过 5 跳的重定向链；原风控处理器将这些不可扫描目标统统标记为 `job.Permanent` 或耗尽重试打入 `dead`，直接触发 `ShortlinkJobsDead` 运维误报，却未通过专门指标区分内部系统异常与外部不可扫描 URL。
   - `HostLimiter.Allow` 在本地规则命中前运行（导致命中的黑名单域也消耗令牌并被延迟），且 `HostLimiter`、`VerdictCache.NormalizeURL` 与 `safehttp.ValidateURL` 未统一剥离 FQDN 尾点（`example.com.`）；`VerdictCache` 缓存键未带规则与模型指纹，改配置重启后最长有 10 分钟陈旧放行窗口。
   - 3xx 重定向跨域名跳转未对落地新域名补做按 host 限速；`validateWorker` 漏校验 `RISK_FETCH_TIMEOUT < WORKER_JOB_TIMEOUT`；`RISK_BLOCKED_HOSTS` 沿用关键词切分逻辑导致不支持 IPv6 字面量且报错文案不准确。
2. **检测能力停留在占位规则**：
   - 原 `RuleScanner` 仅在前 `64 KiB` 原始 HTML 中按两条硬编码英文关键词做子串匹配，未过滤图片/PDF 等二进制流、未剥离 `<head>` 中的内联 `<style>`/`<script>`（常超过 64 KiB 挤占正文），面对语义诈骗、仿冒登录或正常安全文章误封均力不从心；同时直接把原始网页丢给大模型既浪费 token、又无法应对云厂商安全审核拦截（`content_filter` / `data_inspection_failed`）与 Prompt 注入风险。

## 决策

### 1. 分层风控责任链（L0 -> L1 -> L2 -> L3）

维持 `internal/risk.Scanner` 接口不变，通过 `risk.Pipeline` 按成本与命中确定性从低到高串联 4 层检测：

1. **L0 本地规则短路（零外网开销，不被限速阻塞）**：
   - 统一度量 FQDN 尾点（`strings.TrimRight(..., ".")`）；
   - 先查 `blockedHosts`（`RISK_BLOCKED_HOSTS`，支持 `[IPv6]:reason`），再查 `allowedHosts`（`RISK_ALLOWED_HOSTS` 高信誉域名直接放行，黑名单优先于白名单防误放行子域），最后查入口 URL 关键词（`RISK_BLOCKED_KEYWORDS`）。
2. **L1 可选威胁情报（Google Safe Browsing v4，`RISK_GSB_API_KEY`）**：
   - 仅当配置了 API Key 时启用；调用 `/v4/threatMatches:find` 检查入口 URL（及发生 3xx 重定向后的最终落地 URL）。
   - 请求/传输失败自动剥离查询串中的 `?key=...`，绝不在错误消息、日志和 OTel Span 中泄露密钥。
   - GSB 返回限流（429）或 5xx 时 **fail-open 降级**继续走 L2，并将未命中的安全结果标为 `Uncacheable`。
3. **L2 安全抓取 + HTML 净文本提取**：
   - 保留 `safehttp` 拨号层防 SSRF（DNS Rebinding/私网/云元数据/端口白名单）与 3xx 中间/落地 URL 检查，并对跳转向**不同域名**的重定向在拨号前补做 `HostLimiter` 限速（同域 `http -> https` 不重复扣令牌）。
   - `DefaultMaxBodyBytes` 扩至 `256 KiB`；`ExtractPageContent` 根据 `Content-Type` 跳过图片/音视频/PDF 等二进制体（零字节读取），对 HTML 通过流式 `golang.org/x/net/html` Tokenizer 剥离 `<script>`/`<style>`/`<noscript>`/`<template>`/`<iframe>`/`<svg>`，提取 `<title>`、`<meta description>` 与最多 `4 KiB` 归一化可见文本供关键词规则与 L3 使用。
4. **L3 可选 LLM 语义研判（`RISK_LLM_BASE_URL` + `RISK_LLM_MODEL`）**：
   - 将 `LLMJudge` 内部正交拆成三个插槽，隔离不同变化频率：
     1. **`PolicyProvider`（高变动策略层）**：当前通过 `//go:embed prompts/risk_policy.txt`（[internal/risk/prompts/risk_policy.txt](../../internal/risk/prompts/risk_policy.txt)）配合 `StaticPolicyProvider` 随二进制嵌入业务判定准则并生成内容哈希版本号（`v1-<sha256前8位>`），变更策略自动使缓存失效；未来如需上线运维提示词管理界面，只需新增读库 `DBPolicyProvider`（`risk_prompt_versions` 不可变版本表 + `draft/shadow/active` 状态 + 审计日志）。
     2. **`PromptAssembler`（代码锁死协议外壳，[internal/risk/prompt.go](../../internal/risk/prompt.go)）**：固定注入 `<untrusted_page_data>` 防注入声明（并转义待检文本和策略正文中的 `<`/`>`）、严格 `"json"` 输出 Schema 与 `link.BanReason` 白名单枚举，绝不允许运维页面编辑协议结构破坏解析与隔离边界。
     3. **`ChatModel` 防腐层（[internal/risk/chatmodel.go](../../internal/risk/chatmodel.go)）**：默认提供 `OpenAICompatModel` 对接 AI 网关（如本地 Magpie / Higress）或兼容 `/chat/completions` 端点（默认 `max_tokens = 512`，预留 `DisableJSONMode` 与 `OmitTemperature` 方言兼容开关；未配置时零开销关闭）；若未来必须直连非 OpenAI 兼容协议（如 Anthropic `/v1/messages`），只需新增 `ChatModel` 实现，不改动上层调度与缓存。
   - **支持 `ban` / `shadow` 双模式**：默认 `ban`（置信度 `>= RISK_LLM_MIN_CONFIDENCE` 时自动封禁）；设为 `shadow` 或置信度不足时只向 `Handler` 返回含 `Reason`/`Detail` 的 `Uncacheable` 安全结论，由 `Handler` 记录带短码 `code` 的观测日志与 `shortlink_risk_scans_total` 而不自动封禁、不写安全缓存。
   - **上游安全审核与预算防护**：
     - 显式识别厂商内容审核/拒答（HTTP 400/403 含 `data_inspection_failed`/`content_filter`/`prohibited_content`/`moderation`/`safety`，或 `finish_reason` 为 `content_filter`/`safety`、`message.refusal` 非空），记为 `outcome="filtered"`、`Uncacheable: true`（不重试、不误封）；
     - 进程内按分钟预算令牌桶限流（`RISK_LLM_MAX_PER_MIN`，默认 30/min），超预算即 fail-open `outcome="budget_limited"`（`Uncacheable`）；
     - 收到上游 `429` 时解析 `Retry-After`（夹紧到 `1s..5m`）返回 `job.RetryAfter` 退避重试，末次重试仍限流则 fail-open 完成，绝不因大模型限流触发死信告警；5xx/解析失败同样 fail-open 降级为 `Uncacheable`。

### 2. 底座可靠性、缓存指纹与死信/告警治理

1. **统一尾点归一化与单点限速归属**：`safehttp.HostLimiter`、`safehttp.ValidateURL`、`risk.NormalizeURL` 与 `RuleScanner` 统一剥离末尾 `.`；删除 `HandlerOptions` 上与抓取路径脱节的冗余 `Limiter` 字段，收口至 `RuleScannerOptions.Limiter`。
2. **缓存规则指纹（`ComputeRuleVersion`）**：`VerdictCache` 仅在实际启用对应层时将 `AllowedHosts`、`BlockedHosts`、`BlockedKeywords`、`GSBEnabled`、`LLMModel`、`LLMMode`、`LLMMinConfidence` 及 `PolicyVersion` 的 SHA-256 短哈希混入缓存键（绝不含 API Key），更新规则、策略文件或模型重启后旧缓存自然失效。
3. **落地页 GSB 优先于 4xx/二进制短路**：在 `Pipeline.Scan` 中，只要发生 3xx 重定向，即使落地页返回 `403` 或 `application/octet-stream` 等二进制流，也在返回 `OutcomeHTTP4xx` / `OutcomeBinarySkipped` 前先执行 `FinalURL` 的 L1 GSB 核验，防止以二进制下载或 403 反爬绕过已知恶意地址检测。
4. **区分“内部故障”与“外部不可扫描目标”**：
   - `link.risk_scan` 载荷损坏仍返回 `job.Permanent` 进入 `dead`（代表真实系统/Schema Bug）；
   - `ErrBlockedAddress`、`ErrBlockedPort`、`ErrInvalidScheme`、`ErrTooManyRedirects` 记入 `shortlink_risk_scans_total{stage="fetch",outcome=...}` 后返回 `job.Skip`，不再触发 `ShortlinkJobsDead`；
   - 外部站点 5xx/超时在耗尽 `MaxAttempts` 前照常退避重试，最终仍不可达时记 `outcome="unreachable"` 并 `job.Skip`（父 `ctx` 已取消时直接返回 `ctx.Err()`，不在关停期间误标完成或跳过）。
5. **可观测性与告警**：新增 `shortlink_risk_scans_total{stage,outcome}`（严格使用内部有限枚举标签）与 OpenTelemetry 子 Span `risk.gsb` / `risk.llm`；在 [deploy/alerts.yml](../../deploy/alerts.yml) 新增 `ShortlinkRiskUpstreamErrors`（监控 GSB/LLM 持续 `error` / `rate_limited` / `filtered`）。

## 不做什么

1. **不引入无头浏览器（Headless Chrome / Playwright）、LangChainGo/Eino 等重型框架或运行期读磁盘提示词文件**：渲染单页 JS 跳转、强绑定通用 AI 编排框架及宿主机路径挂载维护成本远高于收益，纯静态无正文 SPA 暂仅过 L0/L1/L2。
2. **本次不预先建 `risk_prompt_versions` 数据库表和提示词运营前端，也不堆用不上的多厂商原生客户端**：先通过 `PolicyProvider` 与 `ChatModel` 接口留缝（配合 AI 网关平滑换模型），待真正上线运营后台或直连异构厂商时再增量实现（红线 #7）。
3. **不放开 LLM 自主联网工具**：模型仅分析经 `safehttp` 防护后抓回并脱壳的 `PageContent`，禁止由上游模型直接抓取租户 URL。
4. **不对全部存量活跃短链做后台周期重扫，也不对图片/PDF 做 OCR**：维持“创建/改 URL 入队 + 5–10 分钟 TTL 去重缓存”模型。
5. **不在 LLM 被内容审核拦截（`filtered`）或低置信度时自动封禁短链**：宁可不写入“安全”缓存留待观察，避免误封正常科普/新闻/合规业务页面。

## 怎么验证

1. **单元测试（`internal/safehttp`、`internal/risk`、`cmd/console`）**：
   - FQDN 尾点归一化、`ExtractPageContent` 越过 96 KiB `<head>` 提取与二进制零读取短路；
   - `httptest` 离线模拟 GSB（钓鱼/恶意/未命中/429/错误链路脱敏 `APIKey`）；
   - `httptest` 离线模拟 LLM（防 `</untrusted_page_data>` 注入、`ban`/`shadow`/低置信度、400 `data_inspection_failed` 与 200 `content_filter` 拦截、429 `Retry-After` 退避及末次 fail-open、本地预算限流零出站）；
   - 分层 `Pipeline` 验证 L0 黑名单优先于白名单且零下游调用、L1 命中跳过抓取与 LLM、GSB 降级后结果 `Uncacheable`、3xx 跨域名限速、SSRF/超跳/耗尽不可达返回 `job.Skip` 并准确累加 `shortlink_risk_scans_total`；
   - `WorkerConfig` 校验与日志脱敏单测（确保 `RISK_GSB_API_KEY`、`RISK_LLM_API_KEY` 绝不出现在日志或 `%+v` 输出中）。
2. **全量自检**：`go mod tidy && gofmt -w . && make selfcheck`（含 `-race`、集成测试、`arch-check`、`golangci-lint` 与 `smoke.sh`）。
3. **真实 URL 实测**：使用临时命令行跑通真实公网 URL 的 HTML 净文本提取与回环地址 SSRF 拦截对比验证（仅手工验证，不进离线 CI）。

## 后果

- **收益**：风控底座消除了租户可控 URL 引发的死信告警噪声、限速短路顺序倒挂与规则热更新缓存陈旧问题；同时具备从本地黑白名单、Safe Browsing、HTML 净文本规则到可选 AI 语义研判的完整生产级工程漏斗，未配外部 Key 时零开销退回纯本地安全抓取。
- **代价**：新增可选环境变量 `RISK_ALLOWED_HOSTS`、`RISK_GSB_*`、`RISK_LLM_*` 与依赖 `golang.org/x/net/html`（已从间接依赖转为直接依赖）；启用云端 API 时需关注超时预算（`RISK_FETCH_TIMEOUT` / `RISK_GSB_TIMEOUT` / `RISK_LLM_TIMEOUT` 各自须小于 `WORKER_JOB_TIMEOUT`）与 `ShortlinkRiskUpstreamErrors` 告警。
