# 路线图

按顺序做，每个阶段结束跑 `make check` 和 `make lint`。

## 阶段 1：Base62（第 2、6 课）

- [x] `internal/idgen`：`EncodeBase62`、`DecodeBase62`（阶段 3 改为随机短码后不再使用，现已删除，见 git 历史）
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
- [x] 集成测试（没有 `TEST_DATABASE_URL` 时 `t.Skip`）
- [x] 短码改为随机生成（8 位 Base62），id 仅作内部主键
- [x] 表名按 PostgreSQL 习惯使用 `links`
- [x] 连接池参数从 `.env` 读取（`DB_MAX_OPEN_CONNS` 等）

## 阶段 4：配置、日志、中间件、优雅退出（第 8 课）

- [x] `internal/config`：`LoadConfig` 和测试
- [x] logging、recoverer、request_id 中间件和 `chain`，各自有测试
- [x] `signal.NotifyContext` 加 `Shutdown`，手动验证：请求处理到一半时按 Ctrl+C

## 阶段 5：指标、Redis 缓存与点击统计（第 9、10 课）

指标不在课程里，排在 Redis 前面做：缓存有没有效果要靠数字验收，第 10 课 `ClickRecorder` 的 dropped 计数也要有地方暴露。

- [ ] `go get github.com/prometheus/client_golang`；`GET /metrics` 暴露指标（运维端点，不版本化，不对公网开放）
  - Registry 自己建、由 `main` 注入：测试里会创建很多个 `Server`，往同一个 Registry 重复注册同名指标，`MustRegister` 会 panic
  - 指标在 `NewServer` 里注册一次，`Routes()` 只组装中间件、不注册：现有测试会在同一个 `Server` 上多次调用 `Routes()`（`internal/httpapi/requestid_test.go`），在 `Routes()` 里注册第二次就 panic
  - 自建的 Registry 是空的，Go 运行时指标（`go_goroutines` 等）要自己注册
  - `/metrics` 用 `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})` 暴露，不用 `promhttp.Handler()`（后者默认抓 `DefaultGatherer`）
  - 与业务共用 `ADDR` 端口；“不对公网开放”由部署层保证（反向代理不转发或网络策略限制），应用本身不做鉴权
  - 抓取也走中间件链，会被 logging 记日志、被 HTTP 指标统计一次；暂时接受，噪声影响排查时再挪到链外
- [ ] HTTP 指标中间件：按路由模式、方法、状态码记请求数和耗时直方图
  - label 只用取值有限的维度，短码、request_id 不能做 label
  - 路由模式用 `mux.Handler(r)` 查，未匹配的归到一个固定值；`mux.Handler` 对 405 也返回空 pattern，会归进同一个值，状态码 label 仍能区分
  - 外层中间件里读 `r.Pattern` 是空串（`requestID`、`timeout` 用 `WithContext` 换过 Request）
  - 中间件放在 `recoverer` 外面（和 `logging` 一样），用 status writer 捕获状态码；否则 panic 被写成 500 时指标会漏掉
- [ ] 存储耗时：写一个实现 `LinkStore` 的装饰器包在 `postgres.Store` 外层，按操作记耗时和失败次数（`link.ErrNotFound` 是正常结果，不算失败）
  - 组合顺序固定为 `HTTP -> CachedStore -> 指标装饰器 -> postgres.Store`，存储耗时只算真正打到数据库的调用
- [ ] 连接池指标：注册 `collectors.NewDBStatsCollector`，重点看等待时长和使用中的连接数
- [ ] `docker-compose.yml` 加 Prometheus 抓取 `/metrics`；Prometheus 在容器里抓宿主机的 app，target 用 `host.docker.internal:8080`（Linux 下加 `extra_hosts: host.docker.internal:host-gateway`），或把 app 也放进 compose；压测一轮，用 PromQL 记下没有缓存时跳转的 p50、p99 作为基线
- [ ] `go get github.com/redis/go-redis/v9`
- [ ] `CachedStore`：命中、负缓存、singleflight、Redis 故障降级
- [ ] 缓存指标：命中、未命中、降级次数；同样的压测再跑一轮，和基线对比
- [ ] `ClickRecorder`：非阻塞投递、批量写出、关停时不丢数据
- [ ] `ClickRecorder` 的 dropped 计数暴露到 `/metrics`
- [ ] 用泛型的 `doTyped` 去掉类型断言

## 阶段 6：一个 SaaS 功能

- [ ] 多租户加 API Key 鉴权（中间件加 context 传值）
- [ ] 拆分 `cmd/redirector` 和 `cmd/console`
- [ ] （可选）OpenTelemetry 追踪：拆分后两个进程互不调用，追踪看的是单个请求内部各环节的耗时，等指标解释不了慢请求时再加
