package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

/*
recoverer 接住 handler 的 panic，记下栈并返回 500。
panic 留在 goroutine 里会让这条连接的服务循环退出；转成响应后，其他请求不受影响。

http.ErrAbortHandler 是 net/http 用来中止当前连接的哨兵，必须原样继续抛出。
把它当成普通 panic 吃掉的话，服务器无法按自己的方式关掉这条连接。
*/
func recoverer(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic",
					"err", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				// 响应已经开始写出时，状态码改不到 500，这里仍然尝试写一条明文错误。
				http.Error(w, "internal error", http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
