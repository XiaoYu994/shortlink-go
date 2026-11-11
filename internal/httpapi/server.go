// Package httpapi 放 HTTP 的 handler、路由和中间件。
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"shortlink/internal/link"
)

/*
LinkStore 是 handler 需要的存储能力。
接口定义在使用方，内存版和 PostgreSQL 版都只要满足它。
*/
type LinkStore interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

// Server 持有 handler 需要的依赖。
type Server struct {
	logger *slog.Logger
	store  LinkStore
}

// NewServer 组装 handler 依赖；store 由 main 注入，测试可以换成别的实现。
func NewServer(logger *slog.Logger, store LinkStore) *Server {
	return &Server{logger: logger, store: store}
}

type errorResponse struct {
	Error string `json:"error"`
}

// clientGone 报告请求是否因客户端断开而被取消，这时响应已经没人接收，handler 直接返回，不要记成服务端错误。
func (s *Server) clientGone(r *http.Request) bool {
	if r.Context().Err() == context.Canceled {
		s.logger.Debug("request canceled by client", "path", r.URL.Path)
		return true
	}
	return false
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	// 先编码到内存，成功后再写响应。直接 Encoder.Encode 会在状态码已经发出后才可能失败，客户端只能收到半截 body。
	data, err := json.Marshal(v)
	if err != nil {
		s.logger.Error("marshal JSON response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		s.logger.Error("write JSON response", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, errorResponse{Error: msg})
}
