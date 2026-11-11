# 路线图

按顺序做，每个阶段结束跑 `make check` 和 `make lint`。

## 阶段 1：Base62（第 2、6 课）

- [x] `internal/idgen`：`EncodeBase62`、`DecodeBase62`
- [x] 表驱动测试，包含非法字符和溢出

## 阶段 2：内存版服务（第 3、4、6 课）

- [x] `internal/link`：`Link`、`NewLink`、`ErrNotFound`、`ValidationError`、URL 校验
- [x] 内存版 Store（map 加 `sync.RWMutex`），放在 `internal/memorystore`
- [x] `httpapi`：`POST /api/v1/links` 和 `GET /r/{code}`，在 `Server` 里定义 `LinkStore` 接口
- [x] 用 httptest 测试：创建成功、非法 URL 返回 400、不存在的短码返回 404、跳转返回 302
- [x] 限制请求体大小；短码统一放在 `/r/` 下，和 `/healthz`、`/api` 路由天然隔离

## 阶段 3：PostgreSQL（第 7 课）

- [x] `go get github.com/jackc/pgx/v5`
- [x] `internal/postgres`：`Store` 实现 `LinkStore`
- [x] 错误处理：`sql.ErrNoRows` 转 `link.ErrNotFound`，`23505` 触发短码重试
- [x] 集成测试（没有 `DATABASE_URL` 时 `t.Skip`）
- [x] 短码改为随机生成（8 位 Base62），id 仅作内部主键
- [x] 表名按 PostgreSQL 习惯使用 `links`
- [x] 连接池参数从 `.env` 读取（`DB_MAX_OPEN_CONNS` 等）

## 阶段 4：配置、日志、中间件、优雅退出（第 8 课）

- [ ] `internal/config`：`Load` 和测试
- [ ] logging、recoverer 中间件和 `chain`，各自有测试
- [ ] `signal.NotifyContext` 加 `Shutdown`，手动验证：请求处理到一半时按 Ctrl+C

## 阶段 5：Redis 缓存与点击统计（第 9、10 课）

- [ ] `go get github.com/redis/go-redis/v9`
- [ ] `CachedStore`：命中、负缓存、singleflight、Redis 故障降级
- [ ] `ClickRecorder`：非阻塞投递、批量写出、关停时不丢数据
- [ ] 用泛型的 `doTyped` 去掉类型断言

## 阶段 6：一个 SaaS 功能

- [ ] 多租户加 API Key 鉴权（中间件加 context 传值）
- [ ] 拆分 `cmd/redirector` 和 `cmd/console`
