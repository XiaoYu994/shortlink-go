// Package httpapi 放 HTTP 的 handler、路由和中间件。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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
	logger         *slog.Logger
	store          LinkStore
	requestTimeout time.Duration
	registry       *prometheus.Registry
	metrics        *httpMetrics
}

/*
NewServer 组装 handler 依赖；store 由 main 注入，测试可以换成别的实现。
logger 包上一层 requestIDHandler：带请求 context 的日志会自动写上 request_id。
requestTimeout 是每个请求的处理时限，由 timeout 中间件执行，必须为正。
registry 由 main 创建并注入，/metrics 从这里收集；测试每次传一个新的 Registry，
避免同名指标重复注册。HTTP 指标也在这个构造函数里注册一次，Routes() 只组装中间件。
*/
func NewServer(logger *slog.Logger, store LinkStore, requestTimeout time.Duration, registry *prometheus.Registry) *Server {
	return &Server{
		logger:         slog.New(requestIDHandler{Handler: logger.Handler()}),
		store:          store,
		requestTimeout: requestTimeout,
		registry:       registry,
		metrics:        newHTTPMetrics(registry),
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

// clientGone 报告请求是否因客户端断开而被取消，这时响应已经没人接收，handler 直接返回，不要记成服务端错误。
func (s *Server) clientGone(r *http.Request) bool {
	if r.Context().Err() == context.Canceled {
		s.logger.DebugContext(r.Context(), "request canceled by client", "path", r.URL.Path)
		return true
	}
	return false
}

func (s *Server) writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	/*
		先编码到内存，成功后再写响应。直接 Encoder.Encode 会在状态码已经发出后才可能失败，客户端只能收到半截 body。
		ctx 只负责把 request_id 带进日志，不用它取消已经开始的写出。
	*/
	data, err := json.Marshal(v)
	if err != nil {
		s.logger.ErrorContext(ctx, "marshal JSON response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		s.logger.ErrorContext(ctx, "write JSON response", "err", err)
	}
}

func (s *Server) writeError(ctx context.Context, w http.ResponseWriter, status int, msg string) {
	s.writeJSON(ctx, w, status, errorResponse{Error: msg})
}

/*
handleStoreError 把存储调用的失败映射成响应。
客户端断开时什么都不写；请求超时返回 504；其余情况记错误日志并返回 500。
attrs 是额外的日志字段。跳转失败时带上短码；创建失败时短码还没生成，不传。

超时是 timeout 中间件的 deadline 正常生效，不是存储故障，所以记 Warn：
数据库慢或负载高时这类日志会成片出现，不该和真正的 bug 混在一个级别。
*/
func (s *Server) handleStoreError(w http.ResponseWriter, r *http.Request, op string, err error, attrs ...any) {
	if s.clientGone(r) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		args := append([]any{"op", op, "err", err}, attrs...)
		s.logger.WarnContext(r.Context(), "request timed out", args...)
		s.writeError(r.Context(), w, http.StatusGatewayTimeout, "request timed out")
		return
	}
	args := append([]any{"err", err}, attrs...)
	s.logger.ErrorContext(r.Context(), op, args...)
	s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
}
