# 0003. 复用 PostgreSQL 和连接池配置

- 状态：Accepted
- 日期：2026-11-11

## 背景

本机已有运行中的 PostgreSQL（`si-crm-db-1`），不需要再启动一套 compose。数据库可能和其他项目共用，表名要和 PostgreSQL 习惯一致，连接池参数需要可配置。

## 决策

- 复用已有 PostgreSQL，`DATABASE_URL` 必填；缺失时启动失败。
- 表名使用 `links`，遵循 PostgreSQL 复数命名习惯。
- `id` 用 `GENERATED ALWAYS AS IDENTITY` 自增，只作内部主键。
- `code` 唯一索引，`url`、`created_at` 等字段见 migration。
- 连接池参数通过环境变量配置：`DB_MAX_OPEN_CONNS`、`DB_MAX_IDLE_CONNS`、`DB_CONN_MAX_LIFETIME`、`DB_CONN_MAX_IDLE_TIME`。
- 内存存储只用于测试，不参与生产启动。

## 后果

- 本地启动依赖已有数据库；`.env` 不提交。
- 集成测试会清空 `links` 表，只能指向开发库。
- 如果其他项目也使用 `links` 表名，需要改为独立数据库或重新评估命名。
- 表结构变更后续用迁移工具管理，不手改生产库。
