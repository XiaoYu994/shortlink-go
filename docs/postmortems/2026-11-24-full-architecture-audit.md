# 全量架构深度审查与四批次系统加固复盘（2026-11-24）

- 日期：2026-11-24
- 来源：代码库全链路架构审查与静态/动态验证
- 关联：[ADR 0024](../adr/0024-process-boundary-and-ops-isolation.md)、[ADR 0025](../adr/0025-reconcile-cache-async-purge-and-audit-hardening.md)、[ADR 0026](../adr/0026-timeseries-partitioning-and-retention.md)

## 问题与风险总览

在对控制面（`console` / `worker`）、跳转数据面（`redirector`）、PostgreSQL 存储、`internal/risk` SSRF 与前端控制台做全量逐模块审查后，确认底层叶子包（`logx`、`job`、`httpx`）、写路径单 CTE 原子性与拨号层 SSRF 防护方向正确，但梳理出 10 类结构性隐患（涵盖 5 项 P1 与多项 P2），分四个批次逐一落地并补充回归验证与自动化护栏。

| 批次 | 解决的关键问题 | 核心改动与决策 |
| --- | --- | --- |
| 第 1 批（合规索引、风控、告警、网关限流与前端修复） | `dead jobs`/过期邀请无清理策略；`ClickRecorder` panic 后静默停摆无告警；SSRF 未拦 NAT64/6to4 与非标准端口控制、4xx 被长期缓存；单 CTE 写事务分散；`alerts.yml` 仅 1 条；nginx 无 `limit_req`、compose 缺 `worker`；`--allow-test-host` 进默认生产编译；前端刷新恢复/去重/统计边界瑕疵 | 迁移 `012` 部分索引与清理任务；`shortlink_clicks_recorder_stopped` 指标与 9 条 Prometheus 告警；`64:ff9b::/96` & `2002::/16` 与端口白名单/4xx `Uncacheable`；`updateLinkSQL`/`RevokeAPIKey` 单 CTE 与 `fillfactor=85`；nginx `limit_req`；`devtest` build tag 隔离 |
| 第 2 批（进程边界净化、运维隔离、代理链与鉴权前限流） | `cmd/redirector` 误打包 `identity`、`go-oidc`、`oauth2`、`risk`、`job`、`audit`；`/metrics`/`/healthz` 与公网共端口；多跳反代取 `X-Forwarded-For` 最右一段导致 UV 塌缩；伪造 token 直接穿透至 Postgres；`postgres.Store` 单包单结构体跨界耦合 | 拆出 `internal/pgdb`、`internal/pgredirect`、`internal/identity/oauth`、只读 `storemetrics.NewReader`/`cache.NewReader`；`internal/config` 彻底去业务依赖；新增 `make arch-check`；支持 `OPS_ADDR` / `CONSOLE_OPS_ADDR` 独立监听、`TRUSTED_PROXY_CIDRS` 右向回溯、`console` 鉴权前有界按 IP 限流（`ADR 0024`） |
| 第 3 批（缓存对账、异步清理、审计强约束与权威请求号） | 自助 `Update`/`Delete` Redis 失败无重试兜底；`risk/handler` 遇 `ErrNotUpdated` 重抓外网 URL；`Delete` 同步删三张统计表拉长行锁；`resolveAuditMeta` 静默降级 `system`/`operator`；客户端可伪造 `X-Request-ID` | 新增无 `dedup_key` 电平触发 `cache.reconcile` Outbox 任务与 `cache.Reader.Reconcile`；`Delete` 改为同事务入队 `link.purge_stats`（PK 元组批删）；`requireAuditMeta` fail-closed（`audit.ErrMissingMeta`）；`httpx.requestID` 始终服务端签发并将合法客户端值记入 `client_request_id`（`ADR 0025`） |
| 第 4 批（按月分区治理、保留期自动化与文档沉淀） | 三张日统计表与 `audit_logs` 无保留期、长期膨胀 | 迁移 `013` 按月范围分区 + `_default` 兜底分区；`partition.go` 与 worker `partitions.maintain`（`90d`/`180d`）自动建月表及整月 `DROP TABLE`；全套文档与决策沉淀（`ADR 0026`） |

## 第 4 批 `EXPLAIN (ANALYZE, BUFFERS)` 实测证据

在 PostgreSQL 16（迁移 013 已落地）构造跨 3 个自然月（`2026_09`、`2026_10`、`2026_11`）的千行级多租户/多链接背景数据集并执行 `ANALYZE` 后，默认查询规划器下的实测执行计划（测试文件：[partition_test.go](../../internal/postgres/partition_test.go)）：

1. **日点击趋势查询（分区裁剪验证）**：

```txt
Sort  (cost=1.23..1.25 rows=5 width=12) (actual time=0.014..0.015 rows=5 loops=1)
  Sort Key: link_clicks_daily.day
  Sort Method: quicksort  Memory: 25kB
  Buffers: shared hit=1
  ->  Seq Scan on link_clicks_daily_2026_11 link_clicks_daily  (cost=0.00..1.18 rows=5 width=12) (actual time=0.005..0.006 rows=5 loops=1)
        Filter: ((day >= '2026-11-01'::date) AND (day < '2026-12-01'::date) AND (link_id = '1'::bigint))
        Rows Removed by Filter: 5
        Buffers: shared hit=1
Planning Time: 0.621 ms
Execution Time: 0.029 ms
```

历史月份与未来月份分区全部被裁剪，只访问目标月 `link_clicks_daily_2026_11`。

2. **日去重访客查询（分区裁剪 + 主键索引）**：

```txt
Aggregate  (cost=13.91..13.92 rows=1 width=8) (actual time=0.020..0.020 rows=1 loops=1)
  Buffers: shared hit=3
  ->  Bitmap Heap Scan on link_visitors_daily_2026_11 link_visitors_daily  (cost=4.92..13.79 rows=50 width=0) (actual time=0.014..0.016 rows=50 loops=1)
        Recheck Cond: ((link_id = '1'::bigint) AND (day >= '2026-11-01'::date) AND (day < '2026-12-01'::date))
        Heap Blocks: exact=1
        Buffers: shared hit=3
        ->  Bitmap Index Scan on link_visitors_daily_2026_11_pkey  (cost=0.00..4.90 rows=50 width=0) (actual time=0.006..0.006 rows=50 loops=1)
              Index Cond: ((link_id = '1'::bigint) AND (day >= '2026-11-01'::date) AND (day < '2026-12-01'::date))
              Buffers: shared hit=2
Execution Time: 0.048 ms
```

3. **审计日志游标列表查询（分区表全局二级索引 `Merge Append`）**：

```txt
Limit  (cost=1.42..43.33 rows=20 width=22) (actual time=0.052..0.057 rows=20 loops=1)
  Buffers: shared hit=17
  ->  Merge Append  (cost=1.42..108.29 rows=51 width=22) (actual time=0.052..0.055 rows=20 loops=1)
        Sort Key: audit_logs.id DESC
        Buffers: shared hit=17
        ->  Index Scan using audit_logs_2026_08_tenant_id_id_idx on audit_logs_2026_08 audit_logs_1 ...
        ->  Index Scan using audit_logs_2026_09_tenant_id_id_idx on audit_logs_2026_09 audit_logs_2 ...
        ->  Index Scan using audit_logs_2026_10_tenant_id_id_idx on audit_logs_2026_10 audit_logs_3 ...
        ->  Index Scan using audit_logs_2026_11_tenant_id_id_idx on audit_logs_2026_11 audit_logs_4 ...
        ...
Execution Time: 0.147 ms
```

各月子分区全部命中继承自 `idx_audit_logs_tenant_id` 的 `(tenant_id, id DESC)` 索引并按归并排序在 `LIMIT 20` 处截断，不存在全表顺序扫描。

4. **异步统计清理批查询（基于 PK 元组）**：

```txt
Limit  (cost=0.00..13.19 rows=50 width=29) (actual time=0.012..0.019 rows=50 loops=1)
  Buffers: shared hit=3
  ->  Append  (cost=0.00..40.64 rows=154 width=29) (actual time=0.011..0.016 rows=50 loops=1)
        ->  Seq Scan on link_visitors_daily_2026_08 ... (rows=0)
        ->  Bitmap Heap Scan on link_visitors_daily_2026_09 ... (rows=50)
              ->  Bitmap Index Scan on link_visitors_daily_2026_09_pkey ...
        ->  Bitmap Heap Scan on link_visitors_daily_2026_10 ... (never executed)
        ...
Execution Time: 0.090 ms
```

命中 `link_visitors_daily_*_pkey` 索引且满足 `LIMIT` 后其余分区立即短路（`never executed`）。

## 沉淀为仓库长期护栏的约束

1. **静态包边界门禁（`make arch-check`）**：写入 `Makefile` 并纳入 `make verify` 与 CI 必过检查，阻止未来新依赖重新击穿 `cmd/redirector`、`internal/httpx`、`internal/identity` 与 `internal/config` 边界。
2. **审计元数据 `fail-closed` 回归测试**：`internal/postgres/audit_test.go` 锁死缺失/非法 `audit.Meta` 直接返回 `audit.ErrMissingMeta`，防止未来新增写路径悄悄回退到静默降级。
3. **测试态隔离编译标签（`devtest`）**：`--allow-test-host` 等仅测试旁路参数严格隔离在 `//go:build devtest`，线上构建产物不可出现。
4. **分区清理 DDL 防误删护栏**：`DropExpired*Partitions` 对子分区名做严格正则校验且只以整个自然月上界判定过期，永久保护 `*_default` 兜底分区。
