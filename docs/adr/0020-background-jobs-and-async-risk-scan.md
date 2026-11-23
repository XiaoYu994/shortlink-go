# 0020. 可靠后台任务与异步 URL 风控

- 状态：Accepted
- 日期：2026-11-23

## 决策总览

| 决策点 | 结论 |
| --- | --- |
| 队列载体 | PostgreSQL 事务内 Outbox（`jobs` 表）+ `FOR UPDATE SKIP LOCKED`；`internal/job` 侧用使用方接口解耦，后续需要时再加 relay 接 Redis Streams / NATS |
| Redis 职责 | 保持 [ADR 0006](0006-redis-cache-design.md) 的 fail-open 缓存定位，只做风控扫描结果的按 host/URL 短时去重与缓存，不承载不能丢的任务队列 |
| 租户公平 | `jobs` 表引入可空 `tenant_id` 列，抢占时按租户做软并发上限，防止单租户批量灌任务饿死其他租户 |
| 进程归属 | 在 `cmd/console` 增加 `console worker` 子命令，作为独立进程运行（与 `console serve` 故障与连接池隔离，复用二进制与镜像） |
| 缓存一致性 | 兑现 [ADR 0016](0016-link-states-and-takedown.md) 预留：`cache.Store.Create` 写正缓存后补 `recheckAfterPositiveWrite`；风控封禁遇到 `cache.ErrNotUpdated` 时自动退避重试 |
| 并发改 URL 防误封 | 任务载荷记录送审时的 `url`，封禁时原子核对 `links.url` 仍一致；已被租户改写则跳过旧任务 |
| 周期维护 | 同一个 worker 进程顺带承载分批清理过期 `sessions`（复用迁移 007 的 `idx_sessions_expires_at`）和过期 `done` 任务 |

---

## 背景

项目已经具备链接状态机与平台封禁（[ADR 0016](0016-link-states-and-takedown.md)）和服务端会话体系（[ADR 0018](0018-users-orgs-and-rbac.md)），目前留下三处明确待补的后台能力：

1. **异步 URL 风控未接活**：`links.ban_source` 已预留 `risk`，[ADR 0016](0016-link-states-and-takedown.md) 约定“创建和修改 URL 时异步送审、发现违规调同一份 `Ban` 接口下架”，但目前没有待办队列、worker 与防 SSRF 的安全抓取。
2. **后台维护任务缺失**：`sessions` 表只增不减，迁移 007 已预建 `idx_sessions_expires_at` 索引等待定期清理。
3. **缺少可靠异步执行框架**：已有的 `ClickRecorder` 是内存 channel 批处理，允许进程崩溃或缓冲满时丢数据（[ADR 0007](0007-async-click-recording.md)）；风控审核与未来的租户 Webhook 投递不能静默丢任务，需要持久落盘、多实例不重复消费、崩溃恢复与退避重试。

### 容量与瓶颈估算（为什么队列不是瓶颈）

以下数字按仓库现有限制推算，具体上限由「怎么验证」中的 benchmark 实测校正：

- **入队流量上限**：受单租户限流默认 `10 req/s`（[ADR 0014](0014-per-tenant-rate-limit-and-quota.md)）和月配额默认 `1000` 条（[ADR 0015](0015-monthly-link-quota.md)）约束。即便 1,000 个租户同时把月配额打满，峰值约 10,000 jobs/s、持续约 100 秒（积压约 100 万条）；10,000 租户稳态月配额全满也仅约 4 条/s。入队只是创建/修改语句里多一次 `INSERT`，几乎不改变管理接口延迟。
- **真正瓶颈在出站抓取**：每条 URL 抓取耗时约 0.2–5s（受 5s 超时上限约束）。按 64 并发、平均 0.5s 算，单 worker 进程吞吐约 128 jobs/s；面对突发百万积压，消化时间取决于出站抓取并发、去重命中率与目标站点延迟，而不是队列本身。

---

## 决策

### 1. 队列方案：PostgreSQL Transactional Outbox + 可替换接口

- 使用 PostgreSQL 通用 `jobs` 表作为唯一事实来源，业务写与任务入队在**同一个数据库事务（或单条多 CTE 语句）**内原子提交。
- `internal/job` 按使用方定义极简 `Queue` 接口（`Claim`、`Complete`、`Retry`、`Dead`、`ReapExpired`），业务写路径只依赖 `postgres.Store` 的领域方法，不直接耦合具体 MQ 客户端。
- **升级路径**：未来如果吞吐或跨服务多消费者扇出真的超出 PG 承载能力，`jobs` 表仍保留为 Outbox，再加一个轻量 relay 把事件投递到 Redis Streams / NATS / Kafka，创建和修改的业务事务边界无需重写。

### 2. 数据模型：`jobs` 表（迁移 009）

沿用 [ADR 0010](0010-app-level-referential-integrity.md) 不建数据库外键，应用层在事务内维护一致性：

```sql
CREATE TABLE jobs (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         TEXT        NOT NULL,
    tenant_id    BIGINT,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    dedup_key    TEXT,
    status       TEXT        NOT NULL DEFAULT 'pending',
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 5,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at    TIMESTAMPTZ,
    locked_until TIMESTAMPTZ,
    locked_by    TEXT,
    last_error   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    CONSTRAINT ck_jobs_kind_length CHECK (octet_length(kind) BETWEEN 1 AND 128),
    CONSTRAINT ck_jobs_tenant_id_positive CHECK (tenant_id IS NULL OR tenant_id > 0),
    CONSTRAINT ck_jobs_status CHECK (status IN ('pending', 'running', 'done', 'dead')),
    CONSTRAINT ck_jobs_attempts CHECK (attempts >= 0 AND max_attempts > 0),
    CONSTRAINT ck_jobs_running_lock CHECK (
        status <> 'running' OR (locked_at IS NOT NULL AND locked_until IS NOT NULL AND locked_by IS NOT NULL)
    ),
    CONSTRAINT ck_jobs_finished_at CHECK (
        (status IN ('done', 'dead')) = (finished_at IS NOT NULL)
    )
);

-- 抢占只扫待执行行，历史 done/dead 行不拖慢索引。
CREATE INDEX idx_jobs_claimable
    ON jobs (run_at, id)
    WHERE status = 'pending';

-- 租约回收只扫运行中行。
CREATE INDEX idx_jobs_lease_expired
    ON jobs (locked_until)
    WHERE status = 'running';

-- 按租户统计当前运行中任务数，支撑公平调度软上限。
CREATE INDEX idx_jobs_running_tenant
    ON jobs (tenant_id)
    WHERE status = 'running' AND tenant_id IS NOT NULL;

-- 可选去重：同一类型和去重键在未完成时最多保留一条活跃任务。
CREATE UNIQUE INDEX uq_jobs_dedup_active
    ON jobs (kind, dedup_key)
    WHERE dedup_key IS NOT NULL AND status IN ('pending', 'running');

-- 支撑历史 done 任务的定期分批清理。
CREATE INDEX idx_jobs_finished_done
    ON jobs (finished_at, id)
    WHERE status = 'done';
```

说明：
- `tenant_id` 是**单向门**字段，第一期直接加进表结构：租户触发的任务（`link.risk_scan`、后续 `webhook.deliver`）写入 `tenant_id`；平台级任务留 `NULL`。
- `dedup_key` 第一期对 `link.risk_scan` 不设全局短码去重（同一短码被改 URL 后应触发新的审核，且载荷里的 `url` 不同），留给周期任务或后续按需使用。

### 3. 事务内入队与缓存一致性

1. **触发条件与原子性**：
   - `postgres.Store.Create`：占本月配额 + 插入 `links` + 插入 `jobs(kind='link.risk_scan', tenant_id, payload={"link_id", "code", "url"})` 放在同一条多 CTE 语句（或同一事务）内提交。配额不足或短码碰撞重试时一起回滚，不产生孤儿任务。
   - `postgres.Store.Update`：**仅当本次请求修改了 `url`（`u.URL != nil`）且更新成功时**，同事务/多 CTE 插入 `link.risk_scan`。只修改 `expires_at` 或 `disabled` 不入队。
2. **创建路径补写后复查（兑现 ADR 0016）**：
   - [ADR 0016](0016-link-states-and-takedown.md) 指出：`Create` 原本不做 `recheckAfterPositiveWrite` 是因为响应返回前没人知道短码；异步风控上线后，创建事务一提交 worker 即可抢到任务并封禁写负缓存，若 `console` 写正缓存晚到一步就会覆盖负缓存。
   - 本期在 `internal/cache/store.go` 的 `Store.Create` 写完正缓存后补上 `recheckAfterPositiveWrite`，与 `Update` 保持一致。
3. **封禁缓存未更新自动自愈**：
   - `cache.Store.Ban` 在库写成功但 Redis 写失败时返回包装了 `cache.ErrNotUpdated` 的错误。
   - 风控 handler 收到 `errors.Is(err, cache.ErrNotUpdated)` 时，按**可重试错误**归还任务并退避重跑；因 `Ban` 本身幂等，重试会再次写入负缓存直至成功。
4. **并发改 URL 防误封**：
   - 场景：租户先写入违规 `URL1`，在扫描完成前又通过 `PATCH` 改成合规 `URL2`。如果旧扫描任务直接按 `code` 封禁，会误封已改好的 `URL2`。
   - 做法：风控封禁走同一条 `Ban` 路径，支持可选的期望 URL 校验（运维 `console link ban` 传空表示无条件封禁；风控传任务载荷里的送审 `url`，SQL 增加 `AND url = $4`）。未更新到行时区分「链接已删除（`link.ErrNotFound`）」和「URL 已变更（如 `link.ErrURLChanged`）」：两种情况风控任务都直接标记 `done`（记为 `skipped` 指标），不重试；`URL2` 由它自己的扫描任务负责审核。

### 4. Worker Pool、租约回收、周期清理与优雅退出

对照 Java：这一层对应线程池 + MQ 消费者 + 分布式调度器，在 Go 里用显式 `context.Context` + `sync.WaitGroup` + `time.Ticker` 组装，确保每个 goroutine 都有明确的退出条件（`AGENTS.md`）。

1. **两段式短事务抢占（绝不持 DB 锁跑外部 IO）**：
   - 抢占：单条 CTE `FOR UPDATE SKIP LOCKED` 选出满足 `status = 'pending' AND run_at <= now()` 且未超过每租户并发软上限的行，立即 `UPDATE` 为 `running`、`attempts = attempts + 1`、`locked_until = now() + $lease`、`locked_by = $worker_id` 后提交事务，毫秒级归还数据库连接。
   - 租户公平软上限：在抢占 CTE 中统计每个 `tenant_id` 当前 `status = 'running'` 的数量，并按 `ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY run_at, id)` 过滤，限制单租户同时运行中的扫描数不超过配置值（例如默认 `4`）。这是**防饿死的软上限**而非强配额锁：两个 worker 在同一毫秒并发抢占时可能短暂超出 1–2 个，换取不锁 `tenants` 行的低竞争开销。
   - 执行：释放 DB 连接后，由固定大小 `N` 的 worker goroutine 各自在独立 `jobCtx`（受单任务超时控制）内调用对应 `Handler`。
   - 回写：开短事务写回最终状态：
     - 成功或显式跳过（链接已删、URL 已变）：`status = 'done'`, `finished_at = now()`。
     - 可重试错误且 `attempts < max_attempts`：`status = 'pending'`, 清空锁字段，`run_at = now() + backoff(attempts)`（指数退避 + 随机抖动，如 `2s * 2^(attempts-1)` 上限 `5m`），记录 `last_error`。
     - 不可重试错误（如载荷 JSON 损坏）或达到 `max_attempts`：`status = 'dead'`, `finished_at = now()`，记录 `last_error`。
2. **租约回收（Lease Reaper）**：
   - 独立 goroutine 定时扫描 `status = 'running' AND locked_until < now()`：若 `attempts < max_attempts` 重置为 `pending` 并设退避 `run_at`；否则标记 `dead`。进程崩溃或被 `SIGKILL` 后，任务会在一个租约周期后自动恢复。
3. **周期维护任务（清理膨胀）**：
   - **过期会话清理**：定时分批删除已过期或已吊销超过保留期（如 7 天）的 `sessions`（`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE expires_at < now() - $retention OR revoked_at < now() - $retention LIMIT 500)`），复用迁移 007 的 `idx_sessions_expires_at`，小批删除避免长事务锁表。
   - **已完成任务清理**：定时分批删除 `status = 'done' AND finished_at < now() - $retention`（如保留 7 天，复用 `idx_jobs_finished_done`），控制 PG 表膨胀与 vacuum 压力；`dead` 任务默认保留，供排障和人工复核。
4. **优雅退出**：
   - 收到 `SIGINT`/`SIGTERM` 后，`/readyz` 返回 503（若暴露运维端口），停止抢占新任务，等待所有在途 worker 在 `SHUTDOWN_TIMEOUT` 内完成当前任务并写回结果；再按一次信号立即退出。未能及时结束的任务会在租约到期后由 Reaper 接管。

### 5. 抓取侧保护、SSRF 防护与 Redis 去重缓存

1. **SSRF 防护（`internal/risk`）**：
   - **禁止先查 DNS 再请求（TOCTOU / DNS Rebinding 漏洞）**。自定义 `http.Transport.DialContext`：先解析目标主机全部 IP，使用标准库 `net/netip` 拒绝回环（`IsLoopback`）、私网（`IsPrivate`）、链路本地与云元数据（`IsLinkLocalUnicast`/`IsLinkLocalMulticast`）、组播（`IsMulticast`）、未指定地址（`IsUnspecified`）及 IPv4 映射的非法地址；校验通过后**直接对已校验的 IP 拨号**。
   - **重定向校验**：自定义 `http.Client.CheckRedirect`，限制最大跳数（默认 5 跳），每一跳强制校验 scheme 只能是 `http`/`https`，并复用上述安全 `DialContext`。
   - **资源上限**：单次抓取超时（默认 `5s`）+ `io.LimitReader` 限制响应体上限（默认 `64 KiB`），只读判断所需的头部与正文前缀，防止慢连接或无限响应流耗尽内存。
   - **测试豁免边界**：生产默认严格拒绝回环与私网；测试与本地冒烟只能通过代码注入或显式测试配置允许指定测试地址（绝不提供线上关闭 SSRF 检查的环境变量开关）。
2. **Redis 扫描结果缓存与去重（fail-open）**：
   - 按归一化 URL（及必要时按 host 维度）在 Redis 缓存最近的扫描结论（例如 key `shortlink:risk:url:<sha256>`，安全结论缓存如 10 分钟，违规结论缓存如 5 分钟），大量短链指向同一热门域名/落地页时避免重复出站抓取。
   - 严格沿用 [ADR 0006](0006-redis-cache-design.md) 原则：Redis 读/写失败只记日志和指标，降级为直接执行扫描，不影响正确性。
3. **按目标 host 限速**：
   - 在 worker 进程内按目标 host 维护令牌桶（复用已有依赖 `golang.org/x/time/rate`，附空闲桶清理，写法与 `internal/console/ratelimit.go` 一致），限制对同一外部站点的抓取速率，防止 worker 成为放大攻击第三方站点的跳板；触发 host 限速时将任务短暂延迟退避回 `pending`，不计入失败耗尽重试（或等待短窗口）。
4. **检测器抽象**：
   - 定义极简接口 `Scanner`：`Scan(ctx context.Context, targetURL string) (Verdict, error)`，结论为安全或命中某个合法 `link.BanReason`。
   - 第一版实现「安全抓取 + 可配置本地规则（URL/域名/页面关键词匹配）」，离线可测、可进 `make smoke` 和 CI；后续替换成 Google Safe Browsing / LLM 审核只换 `Scanner` 实现。

### 6. 进程归属与包结构

- **进程归属**：在 `cmd/console` 新增 `console worker` 子命令，以独立进程运行。复用现有二进制与 `Dockerfile` 镜像，本地开发另开终端执行 `make run-worker`（`go run ./cmd/console worker`）；与 `console serve` 保持故障隔离、独立数据库连接池预算和独立 `/metrics`（监听 `WORKER_ADDR`，默认关闭或绑定回环如 `127.0.0.1:8082`）。后续若 worker 的依赖与管理进程明显分叉，再按 [ADR 0019](0019-httpapi-package-evolution.md) 拆成独立 `cmd/worker`（双向门）。
- **包布局（遵循 [ADR 0019](0019-httpapi-package-evolution.md) 与使用方定义接口原则）**：
  - `internal/job/`：任务状态、退避算法、Worker Pool `Runner`、Reaper、周期调度、指标和 `Queue`/`Handler` 接口（零 PG/Redis 依赖）。
  - `internal/risk/`：防 SSRF 的 `SafeTransport` / HTTP 客户端、按 host 限速、Redis 结果缓存封装、`Scanner` 接口与规则实现、`link.risk_scan` handler。
  - `internal/postgres/job.go`：`jobs` 表的入队、抢占、完成/重试/死信、租约回收与清理；在 `store.go`（`Create`）和 `admin.go`（`Update`、`Ban` 期望 URL 校验）接入。
  - `internal/postgres/session.go`：补分批清理过期会话方法。
  - `internal/cache/store.go`：`Create` 补上 `recheckAfterPositiveWrite`。
  - `internal/config/`：补 `LoadWorkerConfig`（按进程拆分配置，延续 `3349ed8` 约定）。

### 7. 可观测性（Prometheus 指标）

在 worker 的独立 Registry 上暴露：

- `shortlink_jobs_processed_total{kind,result}`：`result` 为 `done`、`skipped`、`retry`、`dead`
- `shortlink_job_duration_seconds{kind}`：单任务执行耗时直方图
- `shortlink_jobs_in_flight{kind}`：当前运行中的任务数 Gauge
- `shortlink_jobs_reaped_total{kind}`：被 Reaper 回收的超时租约次数
- `shortlink_jobs_oldest_pending_age_seconds{kind}`：当前最老待执行任务的等待秒数（反映违规链接的暴露窗口，比单纯看积压条数更适合配告警）
- `shortlink_risk_cache_lookups_total{result}`：`hit`、`miss`、`fallback`

---

## 不做什么

- **不引入 Kafka、RabbitMQ、NATS 等独立 MQ 中间件**：当前没有跨服务扇出、重放或十万级入队需求；Outbox 本身是引入 MQ 前的必经步骤。
- **不把 Redis 当作持久任务队列**：避免破坏 [ADR 0006](0006-redis-cache-design.md) “Redis 挂了可降级”的故障边界，避免 PG + Redis 双写不一致。
- **第一期不做租户 Webhook 订阅与投递**：等通用 `jobs` 引擎跑稳后，再把 Webhook 作为第二个 `kind` 接入，验证引擎抽象是否合理。
- **第一期不接入真实外部收费风控 API 或大模型**：保持离线可测和 CI 稳定；先通过 `Scanner` 接口留缝。
- **不做前端审核台、申诉流程和封禁历史表**：误封仍由运维执行 `console link unban` 解除（沿用 ADR 0016）。
- **不做 `jobs` 表按天分区**：先用部分索引 + 定期分批删 `done` 行，等表体积和 vacuum 成为真实瓶颈再演进。
- **不给 `redirector` 加任何风控或任务依赖**：跳转进程保持无状态、只读缓存与库、低延迟。

---

## 怎么验证

1. **单测与虚拟时钟（`internal/job`、`testing/synctest`）**：
   - Worker Pool 并发执行、失败指数退避、达到 `max_attempts` 进 `dead`、永久错误不重试；
   - Lease Reaper 回收过期租约；
   - 优雅退出：取消 ctx 后不再抢新任务、等待在途任务写回结果，超时按预期返回。
2. **SSRF 与风控单测（`internal/risk`）**：
   - 表驱动验证拦截 `127.0.0.1`、`::1`、`10.0.0.1`、`172.16.0.1`、`192.168.1.1`、`169.254.169.254`、IPv4-mapped IPv6（`::ffff:127.0.0.1`）等私网/回环地址；
   - 302 跳转到回环/私网地址、跳转到 `file://` 等非 HTTP scheme、超过最大跳数均被拒绝；
   - 超时与超大响应体被 `io.LimitReader` 截断不 OOM；
   - 按 host 限速生效；Redis 故障时结果缓存 fail-open 降级为正常扫描。
3. **PostgreSQL 集成测试（`internal/postgres`）**：
   - `Create` 成功同事务入队；超配额或短码冲突回滚时不留脏任务；
   - `Update` 仅改 `url` 时入队，改 `expires_at`/`disabled` 不入队；
   - 多 goroutine 并发 `Claim` 验证 `FOR UPDATE SKIP LOCKED` 无重复抢占；
   - 按 `tenant_id` 的软并发上限在单租户积压时仍能调度其他租户的任务；
   - 并发改 URL 后，携带旧 URL 的 `Ban` 返回 URL 已变更而不误封新链接；
   - 过期 `sessions` 与 `done` 任务的分批清理只删超过保留期的行；
   - `make test-db` 的 `up -> down -> up` 验证迁移 009 回滚干净。
4. **缓存交错测试（`internal/cache`）**：
   - `Create` 写完正缓存后，若并发风控已封禁，复查补写负缓存；
   - `Ban` 遇 Redis 故障返回 `cache.ErrNotUpdated`，worker 重试后负缓存补齐。
5. **端到端冒烟测试（`make smoke`）**：
   - 启动本地测试站点（提供正常页和命中规则的假钓鱼页）与 worker；
   - 创建指向假钓鱼页的短链 → 异步扫描完成后自动被封禁（`ban_source = risk`）→ `GET /r/{code}` 变 404 → `GET /api/v1/links/{code}` 返回 `status: "banned"` 且带对应 `ban_reason`；
   - `SIGTERM` 后 worker 退出码为 0。
6. **基准测试与吞吐实测（记录到 `docs/runbook.md`）**：
   - `BenchmarkEnqueueInCreate`：对比“创建 + 同事务入队”与“仅创建”的额外耗时与分配开销；
   - `SKIP LOCKED` 队列上限：空 handler 下测 `N = 8, 32, 64` 并发 worker 的抢占与完成吞吐曲线、连接池使用与等待；
   - 全链路吞吐：用 `httptest` 假站点注入可控延迟（如 50ms / 500ms），验证真实抓取并发、host 去重缓存命中前后的吞吐变化及 `shortlink_jobs_oldest_pending_age_seconds` 指标表现。

---

## 后果

- **收益**：
  - 补齐可靠异步任务底座，同时激活 `ban_source = 'risk'` 和 `sessions` 定期清理，并为后续阶段（Webhook 投递、邀请邮件）提供统一执行引擎。
  - 创建和修改接口的 P99 延迟不受外部 URL 抓取拖慢；Redis 继续保持可降级缓存语义。
- **代价与运维注意**：
  - PostgreSQL 新增高频写入与更新表 `jobs`，依赖定期清理 `done` 行和 autovacuum 避免表膨胀；worker 独立占用一份数据库连接池预算，需要计入 PostgreSQL `max_connections`（延续多副本演练的连接预算核算）。
  - 异步风控存在审核窗口：在扫描完成前违规短码短暂可访问，窗口长度由 `shortlink_jobs_oldest_pending_age_seconds` 监控。
  - `cache.Store.Create` 每次创建多一次按 `code` 的复查读库（低频管理写，代价可接受，同 ADR 0016 的 `Update`）。
- **同步文档**：落地时同步更新 `docs/architecture.md`、`docs/runbook.md`（worker 启动、压测记录与排障）、`docs/api.md`（新增指标说明）、`docs/roadmap.md` 和 `.env.example`。

---

## 备选方案

- **Redis List / Streams / asynq 作为主队列**：无法与 PostgreSQL 写操作共事务，业务成功但入队失败会永久漏审；若为了防漏再加 Outbox + relay，等于同时维护两套队列。同时会把 Redis 从可降级缓存变成强依赖持久化组件，违背 ADR 0006。
- **直接引入 Kafka / RabbitMQ / NATS**：运维与本地开发复杂度高，且保证业务写与发消息原子性依然需要 Outbox；当前没有多服务扇出与十万级入队需求，留作未来加 relay 的演进路径。
- **在 `SELECT ... FOR UPDATE` 事务内直接抓取 URL**：一次外部 HTTP 请求耗时可达数秒，会长时间占用数据库连接和行锁，极易打满连接池；必须改为短事务抢占 + 租约模式。
- **单独建 `risk_scans` 表而非通用 `jobs` 表**：字段更贴合风控，但很快引入 Webhook 或异步邮件时又要复制一套抢占、重试、租约回收和清理逻辑；通用 `jobs` 表成本几乎相同且能直接复用。
