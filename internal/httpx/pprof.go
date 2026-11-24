package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"
)

const maxLogLevelBodyBytes = 1024

type logLevelPayload struct {
	Level string `json:"level"`
}

/*
PprofHandler 返回只有剖析端点的 handler，给 main 挂到单独的回环监听上（ADR 0013）。

不放进业务路由，有两个原因：
剖析数据里有堆内容和启动参数，不能跟着业务端口暴露出去；
timeout 中间件会取消请求的 context，/debug/pprof/profile?seconds=30
这样的采样会在 REQUEST_TIMEOUT 处被悄悄截断。所以这里不套任何中间件。

显式注册而不是 import _ "net/http/pprof"：那种写法靠 init 往
http.DefaultServeMux 上注册，本项目的路由都是自己建的 mux，不会生效。
Index 按路径最后一段分发 heap、goroutine、block、mutex 等具名 profile；
另外四个不是具名 profile，要单独注册。
*/
func PprofHandler() http.Handler {
	return DebugHandler(nil)
}

/*
DebugHandler 在回环管理端口暴露 pprof 端点；当 level 非 nil 时，
同时暴露 GET/PUT /debug/log/level，允许在不重启进程的情况下动态切换 slog 日志级别。
*/
func DebugHandler(level *slog.LevelVar) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	if level != nil {
		mux.HandleFunc("GET /debug/log/level", func(w http.ResponseWriter, _ *http.Request) {
			writeLogLevelJSON(w, http.StatusOK, logLevelPayload{Level: level.Level().String()})
		})
		mux.HandleFunc("PUT /debug/log/level", func(w http.ResponseWriter, r *http.Request) {
			raw, ok := readLogLevelInput(r)
			if !ok {
				writeLogLevelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
				return
			}
			var next slog.Level
			if raw == "" || next.UnmarshalText([]byte(raw)) != nil {
				writeLogLevelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid log level"})
				return
			}
			level.Set(next)
			writeLogLevelJSON(w, http.StatusOK, logLevelPayload{Level: level.Level().String()})
		})
	}
	return mux
}

func readLogLevelInput(r *http.Request) (string, bool) {
	if q := strings.TrimSpace(r.URL.Query().Get("level")); q != "" {
		return q, true
	}
	if r.Body == nil {
		return "", true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLogLevelBodyBytes+1))
	if err != nil || len(body) > maxLogLevelBodyBytes {
		return "", false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", true
	}
	var payload logLevelPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	return strings.TrimSpace(payload.Level), true
}

func writeLogLevelJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
