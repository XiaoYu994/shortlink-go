package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"syscall"

	"shortlink/internal/logx"
)

type errorResponse struct {
	Error string `json:"error"`
}

// IsClientGone 报告请求是否因客户端断开而被取消，这时响应已经没人接收，handler 直接返回，不要记成服务端错误。
func IsClientGone(logger *slog.Logger, r *http.Request) bool {
	if r.Context().Err() == context.Canceled {
		logger.DebugContext(r.Context(), "request canceled by client", "path", r.URL.Path)
		return true
	}
	return false
}

// WriteJSON 先将 v 编码到内存再写入响应，避免写出一半状态码后才编码失败。
func WriteJSON(logger *slog.Logger, ctx context.Context, w http.ResponseWriter, status int, v any) {
	/*
		先编码到内存，成功后再写响应。直接 Encoder.Encode 会在状态码已经发出后才可能失败，客户端只能收到半截 body。
		ctx 只负责把 request_id 带进日志，不用它取消已经开始的写出。
	*/
	data, err := json.Marshal(v)
	if err != nil {
		logger.ErrorContext(ctx, "marshal JSON response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		if isClientDisconnect(ctx, err) {
			logger.DebugContext(ctx, "write JSON response canceled by client", "err", err)
			return
		}
		logger.ErrorContext(ctx, "write JSON response", "err", err)
	}
}

func isClientDisconnect(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// WriteError 统一输出 {"error": msg} 格式的 JSON 错误响应，并在请求宽事件里记录 err_msg。
func WriteError(logger *slog.Logger, ctx context.Context, w http.ResponseWriter, status int, msg string) {
	logx.SetError(ctx, msg)
	WriteJSON(logger, ctx, w, status, errorResponse{Error: msg})
}

/*
temporaryError 由临时过载错误实现，比如 cache 在 Redis 故障时回源并发已满。
HTTP 层不认识具体存储实现，只看这个能力；这类失败返回 503，让客户端稍后重试。
*/
type temporaryError interface {
	error
	Temporary() bool
}

/*
HandleStoreError 把存储调用的失败映射成响应。
客户端断开时什么都不写；请求超时返回 504；临时过载返回 503；其余情况记错误日志并返回 500。
attrs 是额外的日志字段。跳转失败时带上短码；创建失败时短码还没生成，不传。

超时和过载是正常生效的机制，不是存储故障，所以记 Warn：
数据库慢或负载高时这类日志会成片出现，不该和真正的 bug 混在一个级别。
*/
func HandleStoreError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, op string, err error, attrs ...any) {
	if IsClientGone(logger, r) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		args := append([]any{"op", op, "err", err}, attrs...)
		logger.WarnContext(r.Context(), "request timed out", args...)
		WriteError(logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
		return
	}
	if temp, ok := errors.AsType[temporaryError](err); ok && temp.Temporary() {
		args := append([]any{"op", op, "err", err}, attrs...)
		logger.WarnContext(r.Context(), "store overloaded", args...)
		WriteError(logger, r.Context(), w, http.StatusServiceUnavailable, "service temporarily overloaded")
		return
	}
	args := append([]any{"err", err}, attrs...)
	logger.ErrorContext(r.Context(), op, args...)
	WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
}
