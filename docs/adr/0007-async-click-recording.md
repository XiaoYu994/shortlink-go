# 0007. 异步批量记录点击

- 状态：Accepted
- 日期：2026-11-12

## 背景

跳转命中缓存时不再访问数据库。点击如果在 handler 里同步更新，每次 302 又会占用一条连接，热门短码还会锁住同一行。`clicks` 列已经存在。缓存里存完整 `Link`，就是为了命中时还能拿到内部 `ID`（[ADR 0006](0006-redis-cache-design.md)）。

## 决策

- `internal/click.Recorder` 用带缓冲的 channel 接收链接 ID。`Record` 在缓冲满时立即返回，并把 `shortlink_clicks_dropped_total` 加一。
- 一条 goroutine 按 100 条或 1 秒写一批。同一 ID 先在内存里加总，再用一条 `UPDATE ... FROM unnest` 写回。写出的点击计入 `shortlink_clicks_written_total{result}`，`success` 和 `error` 分开。
- 只对 `GET /r/{code}` 记点击。Go 1.22 的 ServeMux 里 GET 模式同时匹配 HEAD；HEAD 仍返回 302，但不计点击。查出链接时客户端已经断开的，也不计点击。
- `Close` 在 `http.Server.Shutdown` 之后调用，关闭 channel，写完剩余点击，然后才关闭数据库。`Record` 持有读锁再发送，`Close` 拿写锁后才关闭 channel，避免向已关闭的 channel 发送。可以多次调用，排空时限以第一次的 ctx 为准。
- 运行中的写失败记日志并丢掉这一批，避免 worker 停住以后把缓冲撑满。channel 关闭之后的满批和尾部共用 `Close` 的 ctx，错误交给 `Close` 返回；`main` 只打日志，不改变进程退出码。
- worker 入口会 recover。panic 时停掉记录并让 `Close` 返回错误，避免没人消费的 channel 把后续点击都记成 dropped。
- `AddClicks` 不放进 `LinkStore`，点击写出不经过缓存和存储指标。

## 后果

- 跳转路径不再为计数等待数据库。计数会少的情况有两种：缓冲满（`shortlink_clicks_dropped_total`）和写失败（`shortlink_clicks_written_total{result="error"}`）。dropped 一直是 0 不代表没丢点击。
- 关停时，已经开始的普通批次继续用自己的 `writeTimeout`（默认 2s），不跟 `Close` 的 ctx 取消，避免把已经交给数据库的点击丢掉。channel 关闭之后的满批和尾部共用 `Close` 的 ctx。`main` 在 HTTP `Shutdown` 之后为 `Close` 另起一份 `SHUTDOWN_TIMEOUT`，排空被这一份时限盖住，不会按批次数再乘 `writeTimeout`。
- 整个退出因此是 HTTP 的 `SHUTDOWN_TIMEOUT`，加上可能还在写的那一批 `writeTimeout`，再加上排空的 `SHUTDOWN_TIMEOUT`。ctx 到期后 `Close` 仍会等当前这一批结束，再关数据库；没写完的点击丢掉，并打错误日志。第二次信号立刻结束进程。进程崩溃或收到 SIGKILL（包括 Kubernetes 默认在终止宽限后发送的那一次）会丢掉缓冲区里还没写完的点击。

## 参考

- `internal/click/recorder.go`、`internal/httpapi/redirect.go`、`internal/postgres/store.go`
- `docs/architecture.md`、`docs/api.md`、`docs/runbook.md`
- [ADR 0005](0005-prometheus-before-tracing.md)、[ADR 0006](0006-redis-cache-design.md)
