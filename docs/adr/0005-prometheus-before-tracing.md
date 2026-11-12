# 0005. 先做 Prometheus 指标，追踪以后再说

- 状态：Accepted
- 日期：2026-11-12

## 背景

阶段 5 要加 Redis 缓存，缓存有没有把跳转变快，需要一组上线前的数字做对照。第 10 课的 `ClickRecorder` 还有 dropped 计数，需要一个出口。课程本身不讲指标。

拆成 `cmd/redirector` 和 `cmd/console` 之后，两个进程互不调用。追踪能看到的是单个请求内部各环节的耗时，跨进程调用链还不存在。

## 决策

- 在 Redis 之前接入 `github.com/prometheus/client_golang`。`GET /metrics` 是运维端点：不版本化，不对公网开放。它和业务共用一个监听端口，应用本身不做鉴权；“不对公网开放”由部署层保证（反向代理不转发或网络策略限制）。
- Registry 由 `main` 创建并注入。指标在 `NewServer` 里注册一次，`Routes()` 只组装中间件，不注册指标：现有测试会在同一个 `Server` 上多次调用 `Routes()`，重复注册会 panic。测试构造多个 `Server` 时，每个都要注入新的 Registry。自建 Registry 是空的，Go 运行时指标要单独注册。
- `/metrics` 用 `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})` 暴露，不能用 `promhttp.Handler()`：后者默认抓 `DefaultGatherer`，注入的 Registry 里的指标不会出现。
- 抓取请求也走中间件链，会被 logging 记一条访问日志、被 HTTP 指标统计一次，`timeout` 也作用在抓取上。暂时接受这些噪声；影响排查时再把 `/metrics` 挪到链外。
- HTTP 指标按路由模式、方法、状态码记录请求数和耗时。label 只用取值有限的维度，短码和 request_id 不做 label。路由模式用 `mux.Handler(r)` 查，未匹配归到一个固定值；`mux.Handler` 对 405 也返回空 pattern，所以这个固定值会同时包含 404 和 405，状态码 label 仍能区分。外层中间件里 `r.Pattern` 是空串，因为 `requestID` 和 `timeout` 用 `WithContext` 换过 Request。中间件放在 `recoverer` 外面，和 `logging` 一样用 status writer 捕获状态码，否则 handler panic 被写成 500 时指标会漏掉。
- 存储耗时用一个实现 `LinkStore` 的装饰器包在 `postgres.Store` 外面，按操作记录耗时和失败次数。`link.ErrNotFound` 是正常结果，不算失败。组合顺序固定为 `HTTP -> CachedStore -> 指标装饰器 -> postgres.Store`，这样存储耗时只算真正打到数据库的调用，缓存命中率单独看。
- 连接池注册 `collectors.NewDBStatsCollector`，看等待时长和正在使用的连接数。
- 缓存上线后补命中、未命中、降级次数；`ClickRecorder` 的 dropped 计数进同一套 `/metrics`。
- 本地 Prometheus 放进 `docker-compose.yml` 抓取 `/metrics`。Prometheus 在容器里抓宿主机的 app，target 用 `host.docker.internal:8080`；Linux 下要加 `extra_hosts: host.docker.internal:host-gateway`，或者把 app 也放进 compose。先压测记下没有缓存时跳转的 p50、p99，缓存做好后再对比。
- OpenTelemetry 追踪先不做。等指标解释不了慢请求时再加。

实施顺序见 [路线图阶段 5](../roadmap.md)。

## 后果

- 缓存效果有基线，验收不靠感觉。
- 测试必须使用注入的 Registry，不能用 `prometheus.DefaultRegisterer`；指标只注册一次，`Routes()` 可以被重复调用。
- `/metrics` 和 `/healthz` 同属运维面，不进 `/api/v1`；应用不做鉴权，公网隔离靠部署层。
- 抓取会进访问日志和 HTTP 指标，暂时接受。
- 慢请求在这个阶段靠耗时直方图和日志排查；追踪留到指标不够用的时候。
