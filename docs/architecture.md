# 架构

## 目标

短链接服务：创建短链、按短码 302 跳转。当前是学习项目，优先把 HTTP、存储和错误处理写清楚。

## 目录结构

```txt
shortlink/
├── cmd/server/          # 程序入口，只做组装和启动
├── internal/
│   ├── httpapi/         # handler、路由、中间件；定义 LinkStore 接口
│   ├── link/            # 领域模型：Link、NewLink、错误、URL 校验
│   ├── idgen/           # 随机短码生成、Base62 编解码
│   ├── memorystore/     # 内存存储，仅测试和本地开发
│   ├── postgres/        # PostgreSQL 存储，生产使用
│   └── config/          # 配置加载（第 8 课）
├── migrations/          # 建表 SQL
├── docs/                # 架构、API、运行手册、路线图、ADR
└── Makefile
```

## 依赖方向

```txt
cmd/server  -> httpapi, postgres
httpapi     -> link
postgres    -> link, idgen
memorystore -> link, idgen
link        -> 标准库
idgen       -> 标准库
```

`LinkStore` 接口定义在 `httpapi`（使用方），内存版和 PostgreSQL 版都隐式实现它。`link` 不依赖存储和 HTTP。

## 请求流程

### 创建短链

```txt
POST /api/v1/links
  -> handleCreate
  -> 读取并限制请求体
  -> json.Unmarshal
  -> link.NewLink 校验
  -> store.Create 生成随机短码并写库
  -> 201 + Location: /r/{code}
```

### 跳转

```txt
GET /r/{code}
  -> handleRedirect
  -> store.GetByCode
  -> 302 Location: 原始 URL
```

## 存储

- `internal/memorystore`：map + `sync.RWMutex`，只用于测试和本地开发。
- `internal/postgres`：`database/sql` + pgx，生产使用。
- 表 `links`：`id` 自增主键（内部），`code` 唯一索引（对外），`url`、`clicks`、`expires_at`、`created_at`。
- 短码是 8 位 Base62 随机码，`crypto/rand` 生成；碰撞由唯一索引和最多 5 次重试处理。

## 配置

当前从环境变量读取（第 8 课会收进 `internal/config`）：

- `ADDR`：监听地址，默认 `:8080`
- `DATABASE_URL`：必填
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`
- `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME`
- `REDIS_ADDR`：阶段 5 使用
- `SHUTDOWN_TIMEOUT`：阶段 8 使用

## 尚未实现

- config 包、日志/恢复中间件、优雅退出（第 8 课）
- Redis 缓存、点击统计（第 9、10 课）
- 多租户、鉴权（阶段 6）
