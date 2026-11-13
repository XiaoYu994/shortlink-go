# 0003. PostgreSQL 和连接池配置

- 状态：Accepted
- 日期：2026-11-11

## 背景

短链服务使用 PostgreSQL 存储。表名要和 PostgreSQL 习惯一致，连接池参数需要可配置。

## 决策

- 使用 PostgreSQL，`DATABASE_URL` 必填；缺失时启动失败。
- 表名使用 `links`，遵循 PostgreSQL 复数命名习惯。
- `id` 用 `GENERATED ALWAYS AS IDENTITY` 自增，只作内部主键。
- `code` 唯一索引，`url`、`created_at` 等字段见 migration。
- 连接池参数通过环境变量配置：`DB_MAX_OPEN_CONNS`、`DB_MAX_IDLE_CONNS`、`DB_CONN_MAX_LIFETIME`、`DB_CONN_MAX_IDLE_TIME`。
- 内存存储只用于测试，不参与生产启动。

## 后果

- 本地启动需要一个可用的 PostgreSQL；`.env` 不提交。
- 集成测试使用独立的 `TEST_DATABASE_URL`（库名以 `_test` 结尾），不使用应用的 `DATABASE_URL`；原先“清空开发库”的做法已废弃。
- 表结构用 golang-migrate 管理，不手改生产库。`make migrate` 执行 `migrations/` 里的 up SQL，版本记在 `schema_migrations`。HTTP 进程启动时不迁移。
