# 本地运行手册

## 前置条件

- 本机 Docker 中有可用的 PostgreSQL。当前开发环境复用 `si-crm-db-1`（`127.0.0.1:5432`）。
- 项目根目录有 `.env`，至少包含：

```txt
DATABASE_URL=postgres://user:password@127.0.0.1:5432/dbname?sslmode=disable
```

`.env` 已被 gitignore，不会提交。

- 数据库里已经执行过 `migrations/001_create_links.sql` 和 `002_add_links_constraints.sql`，存在 `links` 表。

如果使用项目自带的 `docker-compose.yml`：

```bash
cp .env.example .env
make up
make run
```

如果复用已经有 PostgreSQL 的容器（当前环境）：

```bash
# 只执行一次建表和约束
docker exec -i si-crm-db-1 psql -U si_crm -d si_crm < migrations/001_create_links.sql
docker exec -i si-crm-db-1 psql -U si_crm -d si_crm < migrations/002_add_links_constraints.sql

make run
```

不要在这时再执行 `make up`：它会尝试占用 5432 端口，和已有容器冲突。

## 启动

```bash
make run
```

看到 `connected to postgres` 和 `listening addr=:8080` 表示启动成功。`make run` 会自动加载 `.env`。

检查：

```bash
curl -i localhost:8080/healthz
```

## 创建一条短链

```bash
curl -i -X POST localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

响应示例：

```http
HTTP/1.1 201 Created
Location: /r/AbC123xy

{"code":"AbC123xy","url":"https://example.com"}
```

访问短链：

```bash
curl -i localhost:8080/r/AbC123xy
```

浏览器打开 `http://localhost:8080/r/AbC123xy` 也可以。

## 查看数据库

当前环境复用 `si-crm-db-1`，可以直接执行：

```bash
docker exec si-crm-db-1 psql -U si_crm -d si_crm \
  -c 'SELECT id, code, url, created_at FROM links ORDER BY id DESC LIMIT 5'
```

如果使用项目自带的 compose：

```bash
make psql
```

## 测试

```bash
make test
make lint
```

注意：PostgreSQL 集成测试会 `TRUNCATE links`，`DATABASE_URL` 只能指向开发库，不要指向有真实数据的库。

## 停止

前台 `make run` 用 `Ctrl+C` 停止（优雅退出在第 8 课实现）。`make down` 只停止项目自带的 compose 服务，不会影响 `si-crm-*` 容器。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| `DATABASE_URL is required` | `.env` 缺失，或没有通过 `make run` 启动 |
| `connect postgres` 失败 | 检查容器是否运行、端口、用户名密码 |
| `relation "links" does not exist` | 执行 `migrations/001_create_links.sql` 和 `002_add_links_constraints.sql` |
| `make up` 报端口占用 | 已有 PostgreSQL 占用 5432；复用已有实例，不要启动 compose |
| 测试清空了链接 | 集成测试会清空 `links` 表；不要指向生产库 |
