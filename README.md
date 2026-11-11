# shortlink

用来学 Go 的短链接服务。配套笔记：Notion《Go 学习笔记：从 Java 到 Go（短链接 SaaS 项目）》。

## 快速开始

```bash
make run                            # 启动服务，默认 :8080
curl -i localhost:8080/healthz      # 应返回 200 和 ok
make test                           # 跑测试（带 -race）
make lint                           # golangci-lint 完整静态检查
make help                           # 查看全部命令
```

需要数据库和 Redis 时（阶段 3 起）：

```bash
cp .env.example .env
make up        # 启动 PostgreSQL 和 Redis，首次启动自动建表
make psql      # 进数据库看看
make down      # 停止
```

## 目录结构

```txt
shortlink/
├── cmd/server/main.go      # 程序入口，只做组装和启动
├── internal/
│   ├── httpapi/            # handler、路由、中间件
│   ├── link/               # 短链核心：结构体、业务错误、校验
│   ├── idgen/              # Base62 短码
│   ├── config/             # 配置加载
│   └── postgres/           # PostgreSQL 存储实现
├── migrations/             # 建表 SQL
├── docker-compose.yml      # 本地的 PostgreSQL 和 Redis
├── AGENTS.md               # 协作方式和代码约定（活文档）
└── Makefile
```

每个包的包注释写在这个包的第一个文件里（`link/link.go`、`idgen/base62.go` 等），
说明了这个包做什么、对应哪一课。包名和依赖方向：`httpapi` 依赖 `link`，
`postgres` 依赖 `link`，`link` 不依赖它们；`main` 负责把它们组装起来。

代码约定和 AI 协作方式见 [AGENTS.md](AGENTS.md)。

## 路线图

按顺序做，每个阶段结束时 `make check` 都应该通过。

### 阶段 1：Base62（第 2、6 课）

- [x] `internal/idgen`：`EncodeBase62`、`DecodeBase62`
- [x] 表驱动测试，包含非法字符和溢出

### 阶段 2：内存版服务（第 3、4、6 课）

- [ ] `internal/link`：`Link`、`NewLink`、`ErrNotFound`、`ValidationError`、URL 校验
- [ ] 内存版 Store（map 加 `sync.RWMutex`）
- [ ] `httpapi`：`POST /links` 和 `GET /{code}`，在 `Server` 里定义 `LinkStore` 接口
- [ ] 用 httptest 测试：创建成功、非法 URL 返回 400、不存在的短码返回 404、跳转返回 302
- [ ] 限制请求体大小；拦截保留字（如 healthz）

### 阶段 3：PostgreSQL（第 7 课）

- [ ] `go get github.com/jackc/pgx/v5`
- [ ] `internal/postgres`：`PostgresStore` 实现 `LinkStore`
- [ ] 错误转换：`sql.ErrNoRows`、唯一约束冲突
- [ ] 集成测试（没有 `DATABASE_URL` 时 `t.Skip`）

### 阶段 4：配置、日志、中间件、优雅退出（第 8 课）

- [ ] `internal/config`：`LoadConfig` 和测试
- [ ] logging、recoverer 中间件和 `chain`，各自有测试
- [ ] `signal.NotifyContext` 加 `Shutdown`，手动验证：请求处理到一半时按 Ctrl+C

### 阶段 5：Redis 缓存与点击统计（第 9、10 课）

- [ ] `go get github.com/redis/go-redis/v9`
- [ ] `CachedStore`：命中、负缓存、singleflight、Redis 故障降级
- [ ] `ClickRecorder`：非阻塞投递、批量写出、关停时不丢数据
- [ ] 用泛型的 `doTyped` 去掉类型断言

### 阶段 6：一个 SaaS 功能

- [ ] 多租户加 API Key 鉴权（中间件加 context 传值）
- [ ] 拆分 `cmd/redirector` 和 `cmd/console`
