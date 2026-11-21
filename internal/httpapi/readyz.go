package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

/*
readyTimeout 是单次就绪探测的上限。
探针要尽快失败，不能把 REQUEST_TIMEOUT 占满；编排器看到非 2xx 就摘流量。
*/
const readyTimeout = time.Second

// errReadyPingMissing 表示就绪项没有接上探测函数。漏接线时按未就绪处理。
var errReadyPingMissing = errors.New("readiness ping is nil")

/*
ReadyCheck 是 /readyz 要问的一项依赖。
Name 只写进日志，响应体不带它，避免把内部结构暴露给探针。

只放「没有它，这个进程一个请求也处理不了」的依赖。
依赖是所有副本共用的：它一出问题，每个副本的探针同时失败，
编排器会把流量全部摘掉。进程本来还能降级服务的话，这样反而把降级变成了全站不可用。
*/
type ReadyCheck struct {
	Name string
	Ping func(context.Context) error
}

/*
SetReadiness 接入就绪检查，后一次调用覆盖前一次。
从没调用过时 /readyz 返回 503：漏接线不能看起来像已经就绪。
不带参数调用表示「启动完成就算就绪」，不探任何依赖。
/healthz 不看这些检查。依赖抖动应该摘流量，不应该让编排器重启进程。
*/
func (s *Server) SetReadiness(checks ...ReadyCheck) {
	s.ready = checks
	s.readyWired = true
}

/*
SetDraining 让 /readyz 开始返回 503，/healthz 不变。

关停时先调用它，等一个探测周期，再关闭监听。
监听还没关，这段时间进来的请求照常处理。存活探针如果也失败，
编排器会把正在退出的进程当成需要重启。
*/
func (s *Server) SetDraining() {
	s.draining.Store(true)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// 关停已经开始。不再探依赖，也不要把每次探针记成故障。
	if s.draining.Load() {
		writeUnavailable(w)
		return
	}
	if !s.readyWired {
		s.logger.WarnContext(r.Context(), "readiness check failed", "check", "unconfigured")
		writeUnavailable(w)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	for _, check := range s.ready {
		err := errReadyPingMissing
		if check.Ping != nil {
			err = check.Ping(ctx)
		}
		if err == nil {
			continue
		}
		// 探针自己取消时，依赖不一定坏，不要记成故障。
		if r.Context().Err() == nil {
			s.logger.WarnContext(ctx, "readiness check failed", "check", check.Name, "err", err)
		}
		writeUnavailable(w)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

func writeUnavailable(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "unavailable\n")
}
