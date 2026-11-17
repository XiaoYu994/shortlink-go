package httpapi

import (
	"net/http"
	"net/http/pprof"
)

/*
PprofHandler 返回只有剖析端点的 handler，给 main 挂到单独的回环监听上（ADR 0013）。

不放进 RedirectRoutes 和 ConsoleRoutes，有两个原因：
剖析数据里有堆内容和启动参数，不能跟着业务端口暴露出去；
timeout 中间件会取消请求的 context，/debug/pprof/profile?seconds=30
这样的采样会在 REQUEST_TIMEOUT 处被悄悄截断。所以这里不套任何中间件。

显式注册而不是 import _ "net/http/pprof"：那种写法靠 init 往
http.DefaultServeMux 上注册，本项目的路由都是自己建的 mux，不会生效。
Index 按路径最后一段分发 heap、goroutine、block、mutex 等具名 profile；
另外四个不是具名 profile，要单独注册。
*/
func PprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}
