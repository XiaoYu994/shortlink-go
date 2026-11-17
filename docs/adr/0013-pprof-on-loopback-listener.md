# 0013. 剖析端点放在单独的回环监听上

- 状态：Accepted
- 日期：2026-11-17

## 背景

多副本演练测出单个 redirector 的点击写入上限约 4,660 次/秒，指标只能说明“写不过来”，
看不出时间花在哪。要回答这个问题需要 pprof 和 execution trace。

Go 没有 Java 那种从外部 attach 到进程的机制（JFR、async-profiler），
进程必须自己把剖析数据暴露出来。标准做法是 `net/http/pprof`，放在哪有两个约束：

- 剖析数据里有堆内容、启动参数和 goroutine 栈，端点也没有鉴权，不能跟着业务端口暴露。
- `/debug/pprof/profile?seconds=30` 在采样期间等的是请求的 context。
  业务路由的 `timeout` 中间件会在 `REQUEST_TIMEOUT`（默认 5s）取消它，采样被截断且不报错。

另外 `import _ "net/http/pprof"` 靠 `init` 往 `http.DefaultServeMux` 上注册。
本项目的路由都是 `http.NewServeMux()` 自己建的，这种导入不会生效，也不希望它生效：
任何依赖包都不该能靠副作用给进程加路由。

## 决策

- 新增 `PPROF_ADDR`。为空（默认）时不监听；设置后 redirector 另起一个 HTTP 服务，
  只提供 `/debug/pprof/`，不套任何中间件。
- `PPROF_ADDR` 只接受回环地址（`127.0.0.1`、`::1`、`localhost`）。`:6060`、`0.0.0.0:6060`
  这类会监听所有网卡的写法在 `config` 校验时拒绝，进程启动失败。
- 路由由 `httpapi.PprofHandler` 显式注册，不使用副作用导入。
- block 和 mutex profile 默认关闭。只在开了剖析端口时调用
  `runtime.SetBlockProfileRate` 和 `runtime.SetMutexProfileFraction`，平时没有采样开销。
- 剖析服务不设 `WriteTimeout`，不参与摘流和优雅退出：进程退出时在途的采样直接断开。
- 端口被占用时启动失败，不留下坏监听。

## 不做什么

- console 不接。它是低频管理写，目前没有要剖析的问题；`PPROF_ADDR` 对 console 不生效。
- 不做鉴权，不对回环以外开放。远程机器上用 SSH 端口转发或 `kubectl port-forward`。
- 不接持续剖析（Pyroscope、Parca）。现在是有具体问题时手动采一次。
- 这一轮不改点击写入的设计，只定位瓶颈。结论见 runbook“点击写入剖析”。

## 怎么验证

- `internal/httpapi` 的 httptest：`PprofHandler` 能返回索引和具名 profile；
  `RedirectRoutes`、`ConsoleRoutes` 上 `/debug/pprof/` 是 404。
- `internal/config` 的表驱动测试：回环地址通过，`:6060`、`0.0.0.0:6060`、缺端口都报错。
- 真实进程：设 `PPROF_ADDR` 启动 redirector，剖析端口返回 200、业务端口返回 404；
  压测中抓 `profile?seconds=10`，拿到的 profile 时长是 10s（没有被 5s 的请求时限截断）。

## 后果

- 排查性能问题不用改代码重新编译，设一个环境变量重启即可。
- 多了一个需要注意的端口。回环限制写在配置校验里，配错会启动失败而不是悄悄暴露。
- 容器里的回环地址从宿主机访问不到，要剖析容器内的进程得进容器或共享网络命名空间；
  有这个需求时再放宽，另写 ADR。
