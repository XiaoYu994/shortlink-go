/*
Package postgres 是存储层的 PostgreSQL 实现。

要做的事（第 7 课）：

  - 用 database/sql 加 pgx 驱动打开连接池，设置连接数上限。
  - PostgresStore：Create 和 Get，全部用占位符和带 Context 的方法。
  - sql.ErrNoRows 转成 link.ErrNotFound，唯一约束冲突（23505）转成业务错误。
  - 集成测试：没有 DATABASE_URL 环境变量时 t.Skip。

表结构见 migrations/001_create_links.sql。
*/
package postgres
