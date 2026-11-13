# 0010. 引用完整性放在应用层，不使用数据库外键

- 状态：Accepted
- 日期：2026-11-13

## 背景

决策时，`links.tenant_id` 和 `api_keys.tenant_id` 由数据库外键约束到 `tenants.id`：
`links` 上是显式命名的 `fk_links_tenant`，`api_keys` 上是内联外键，默认名
`api_keys_tenant_id_fkey`，都没有使用级联。

《阿里巴巴 Java 开发手册》MySQL 章节有一条强制规则：

> 【强制】不得使用外键与级联，一切外键概念必须在应用层解决。

手册的理由是外键适合单机低并发，不适合分布式、高并发集群；级联更新强阻塞，
有更新风暴的风险；外键影响插入速度。本项目决定遵守这条规则，统一数据访问方式。

PostgreSQL 的外键由系统触发器实现，成本与 MySQL 的 InnoDB 不完全相同。当前是
单库、管理写路径低频、跳转读路径大部分命中 Redis，性能不是这次决策的主要原因；
主要原因是遵守统一规范，以及为以后按租户拆分存储留出空间——分片之后外键在
物理上就无法跨库，早晚要在应用层解决。

## 决策

- 删除外键，不使用 `ON DELETE/UPDATE CASCADE`。`migrations/002_add_tenants.up.sql`
  直接建成没有外键的表，并加上 `idx_links_tenant_id`、`idx_api_keys_tenant_id`。
  接入迁移工具时还没有任何库记录过版本，所以没有保留先建外键再删除的后续文件。
- `tenant_id` 保持 `NOT NULL`，字段含义不变：由鉴权决定，不从请求 JSON 读取。
- 引用完整性由应用层保证，现有代码已经满足：
  - 创建短链时，`tenant_id` 只来自鉴权中间件写入 context 的租户
    （`internal/httpapi/createlink.go`）。
  - 签发 API Key 给已有租户前，先查询租户是否存在，查不到返回
    `ErrTenantNotFound`（`internal/postgres/apikey.go` 的 `IssueAPIKey`）。
  - 列出密钥前先用 `EXISTS` 确认租户存在（`ListAPIKeys`）。
  - 目前没有删除租户的入口。
- 以后加删除租户的命令时，必须在同一事务里检查或处理该租户的 `links` 和
  `api_keys`；有引用就拒绝删除，或先删子行。没有这个流程之前，不提供删除入口。
- 引用列建索引：`api_keys (tenant_id, id)` 服务 `ListAPIKeys` 的
  `WHERE tenant_id = $1 ORDER BY id`；`links (tenant_id, id)` 留给按租户列表，
  分页游标用 `id`，不用 `created_at`。
- 用对账 SQL 定期检查孤儿行，正常情况下结果应为 0：

  ```sql
  SELECT 'links' AS tbl, count(*) AS orphans
  FROM links l LEFT JOIN tenants t ON t.id = l.tenant_id
  WHERE t.id IS NULL
  UNION ALL
  SELECT 'api_keys', count(*)
  FROM api_keys k LEFT JOIN tenants t ON t.id = k.tenant_id
  WHERE t.id IS NULL;
  ```

## 后果

- 数据库不再兜底。遗漏应用层校验会留下孤儿行，只能靠测试和定期对账发现；
  新增写路径时要显式检查引用的租户是否存在。
- 删除租户比有外键时复杂，必须显式处理子表。外键原本提供的“删不掉”保护，
  现在要在命令里写出来。
- 写入路径少一次父表检查和行锁；数据导入、归档、批量订正不再受外键顺序限制；
  以后按租户分片时不需要再拆外键。
- 如果以后放弃分片、又希望数据库兜底完整性，可以重新加回外键。代价是一次
  全表校验，所以应在对账结果为 0 时执行。
- 需要重新评估的条件：团队放弃这条规范，或确定不再分片且更看重数据库层的
  完整性保护。
