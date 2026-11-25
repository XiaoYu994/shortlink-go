# 0024. 进程边界净化、运维端口隔离与接入限流

- 状态：已采纳
- 日期：2026-11-24
- 相关：`0006`, `0008`, `0009`, `0013`, `0019`, `0021`

## 背景

`go list -deps ./cmd/redirector` 检查发现，尽管 [ADR 0009](0009-process-split-and-credentials.md) 明确要求公开跳转进程 `redirector` 不包含管理能力，但构建后的 `redirector` 二进制依然把 `identity`、`coreos/go-oidc`、`golang.org/x/oauth2`、`risk`、`job`、`audit` 和全量 `postgres`（含鉴权、邀请、组织管理、任务表）编译进了依赖图。根本原因有两条：

1. `internal/config` 把 `redirector`、`console`、`worker` 三个进程的配置与校验揉在一个包里，直接 import 了 `postgres`、`cache`、`job`、`risk`、`link`；而 `postgres` 又 import 了 `identity`、`audit`、`job`，导致 `cmd/redirector` 通过 `config` 传递依赖了全部管理与后台包。
2. `internal/postgres.Store` 与装饰器 `cache.Store`、`storemetrics.Store` 直接暴露全量管理接口；`internal/identity` 将无第三方依赖的领域模型与依赖 `go-oidc` / `oauth2` 的外部认证适配器混在一个包内。

同时，公网与运维面还有三项架构缺口：

3. `/metrics`、`/healthz`、`/readyz` 与公网路由共用同一监听端口，仅靠网关白名单隔绝外部抓取；一旦网关规则缺失或应用直接暴露即发生信息泄露。
4. `ClientIP` 原实现假设仅经过单层反向代理，在 CDN -> 网关两跳及以上链路中会取到前端 CDN/网关 IP，导致访客去重统计（UV）和日志 IP 失真。
5. `console` 的 `rateLimit` 放在 `authenticate` 之后，任何携带格式合法 `Bearer slk_...` 的伪造请求都会直接穿透到 PostgreSQL 查询 `api_keys`，缺乏鉴权前保护。

## 决策

### 1. 进程边界净化与静态依赖守卫（`make arch-check`）

- 拆分基础库与热路径包：
  - 新增 [`internal/pgdb`](../../internal/pgdb/pgdb.go)，仅依赖标准库与 `pgx/v5`，承载连接池打开（`Open` / `PoolConfig`）与 PostgreSQL 约束错误判定，不带任何业务依赖；
  - 新增 [`internal/pgredirect`](../../internal/pgredirect/store.go)，仅依赖 `pgdb`、`link`、`click`，实现公开跳转所需的 `GetByCode` 与点击批量写入 `RecordClicks`；[`postgres.Store`](../../internal/postgres/store.go) 内嵌 `*pgredirect.Store`，消除双份 SQL 漂移；
  - 拆分 [`internal/identity/oauth`](../../internal/identity/oauth/oidc.go) 承载 OIDC / GitHub OAuth 2.0 适配器，保持 [`internal/identity`](../../internal/identity/identity.go) 零第三方库依赖；
  - 为 `storemetrics` 与 `cache` 增加只读装饰器 [`storemetrics.NewReader`](../../internal/storemetrics/store.go) 与 [`cache.NewReader`](../../internal/cache/store.go)，全量 `Store` 内嵌 `*Reader`，`cmd/redirector` 只装配只读链路；
  - [`internal/config`](../../internal/config/config.go) 剥离全部业务包依赖，仅保留通用 `BaseConfig`、连接池/日志装配及通用环境解析与脱敏工具；进程专属配置分别下沉到 [`cmd/redirector/config.go`](../../cmd/redirector/config.go) 与 [`cmd/console/config.go`](../../cmd/console/config.go)。
- 新增 `make arch-check`（纳入 `make verify`），通过 `go list -deps` 自动化断言：
  - `cmd/redirector` 不得引入 `identity`、`risk`、`job`、`audit`、`postgres`、`go-oidc`、`oauth2`；
  - `internal/httpx` 不得引入任何业务域或存储包；
  - `internal/identity` 不得引入 `go-oidc`、`oauth2`、`pgx`、`go-redis`；
  - `internal/config` 不得引入 `postgres`、`cache`、`job`、`risk`、`link`、`identity`。

### 2. 运维端口物理隔离（`OPS_ADDR` / `CONSOLE_OPS_ADDR`）

- `redirector` 新增可选 `OPS_ADDR`，`console` 新增可选 `CONSOLE_OPS_ADDR`（沿用 `ADDR` 与 `CONSOLE_ADDR` 隔离约定，避免共用 `.env` 时端口冲突）。
- 启用独立运维端口后，业务监听仅挂载业务路由（`RoutesBusiness()`），不再暴露 `/healthz`、`/readyz`、`/metrics`；运维监听单独挂载 `/healthz`、`/readyz`、`/metrics`。
- 优雅退出顺序：运维监听先于业务服务启动，并在业务服务完成 `DrainDelay` 摘流与 `Shutdown` 之后才关闭，确保编排器在整个摘流窗口都能从 `/readyz` 稳定读到 `503`。
- 为兼容本地极简开发与单端口部署，未配置 `OPS_ADDR` / `CONSOLE_OPS_ADDR` 时保持回退到同端口挂载。

### 3. 多跳可信代理链解析（`TRUSTED_PROXY_CIDRS`）

- 在 [`httpx.ClientIPWithTrustedProxies`](../../internal/httpx/clientip.go) 支持 `TRUSTED_PROXY_CIDRS`：
  - `TRUST_PROXY_HEADERS=false` 时始终忽略转发头；
  - 配置了可信代理网段时，仅当直连对端 `RemoteAddr` 落入 `TRUSTED_PROXY_CIDRS` 才读取 `X-Forwarded-For`，并从右向左跳过所有可信代理跳转，取第一个不在可信网段内的合法 IP；遇到非 IP 伪造片段立即回退直连 IP；
  - 同时装配到 `redirector` 访客哈希/访问日志与 `console` 鉴权前限流/访问日志。

### 4. `console` 鉴权前有界按 IP 限流

- 在 [`internal/console/ratelimit.go`](../../internal/console/ratelimit.go) 增加独立于 `authenticate` 外层的 `ipRateLimiter`，对 `/api/v1/*` 生效（默认 `CONSOLE_IP_RATE_LIMIT=60`、突发 `CONSOLE_IP_RATE_BURST=120`，`0` 可关闭）。
- 为防恶意随机源 IP 撑爆进程内存，映射表设置硬上限 `maxBuckets = 65536`：满容时先清理空闲条目，仍满则 O(1) 随机驱逐一条已有条目（避免持锁 O(N) 扫描阻塞），确保内存有界。
- 搭配网关层 `deploy/nginx.conf` 的 `limit_req`（`/r/` 与 `/api/` 分别限速并返回 `429`）形成网关+应用双层防御。

## 不做什么

- **不在 Redis 引入"无效 API Key 负缓存"**：伪造随机 64 位十六进制密钥空间极大，负缓存命中率极低，反而会额外增加 Redis 往返开销，且容易被大量随机密钥刷爆 Redis 内存或导致键驱逐。有界进程内 IP 令牌桶 + 网关 `limit_req` 足以阻断单源/小规模爆破查询数据库。
- **不在 `redirector` 应用层引入进程内高基数 IP 限流映射**：跳转数据面由网关 `limit_req`、短码格式预校验和受限并发回源 `missSlots` / `fallbackSlots` 共同承压（ADR 0008）。
- **不在本阶段把 `internal/console` 按 handler/middleware 技术分层拆子包**：继续保持单一 `console` 包并用文件边界（新增 `request.go` 收口通用请求解析）组织代码（ADR 0019）。

## 验证标准

1. `make arch-check` 通过，`cmd/redirector` 二进制依赖中完全不包含管理/鉴权/风控/审计包；
2. `httpx` 单测覆盖单跳、多跳 CDN+Nginx、非可信对端伪造头、IPv4 映射 IPv6 等场景；
3. `console` 单测覆盖大量伪造 Bearer Token 在超过 IP 桶上限后返回 `429` 且不再触发 `APIKeyAuthenticator` 查库，且桶数量不超过 `maxBuckets`；
4. `scripts/smoke.sh` 验证启用 `OPS_ADDR` 与 `CONSOLE_OPS_ADDR` 后业务端口访问 `/metrics` 返回 `404`，运维端口 `/healthz`、`/readyz`、`/metrics` 返回 `200`；
5. `make verify` 与 `make selfcheck` 全部通过。
