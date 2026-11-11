// Package httpapi 放 HTTP 的 handler、路由和中间件。
package httpapi

import (
	"io"
	"log/slog"
	"net/http"
)

/*
Server 持有 handler 需要的依赖。

TODO（第 6、7 课）：加上 store 字段。接口由这里（使用方）定义，例如：

	type LinkStore interface {
	    Create(ctx context.Context, url string) (code string, err error)
	    Get(ctx context.Context, code string) (url string, err error)
	}
*/
type Server struct {
	logger *slog.Logger
}

func NewServer(logger *slog.Logger) *Server {
	return &Server{logger: logger}
}

// Routes 返回注册好全部路由的 handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// TODO(第 6 课): mux.HandleFunc("POST /links", s.handleCreate)
	// TODO(第 6 课): mux.HandleFunc("GET /{code}", s.handleRedirect)

	// TODO(第 8 课): 用 chain 包上 logging 和 recoverer 中间件。
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}
