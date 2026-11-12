package httpapi

import (
	"context"
	"net/http"
	"time"
)

/*
timeout 给每个请求的 context 加上处理时限。到点后 handler 里的数据库调用会收到
DeadlineExceeded，pgx 随即取消正在执行的查询，handler 把错误映射成 504。

http.Server 的 ReadTimeout、WriteTimeout 管的是连接上的读写，不会取消 handler 的 context：
慢查询照样跑完、照样占着连接池里的连接，访问日志还会把它记成成功。真正的处理时限只能靠它。

timeout 挂在 recoverer 里层（离 handler 最近）：外层中间件负责写日志、兜 panic，
它们的收尾不该被时限打断；被约束的只有 handler 本身。
*/
func timeout(d time.Duration) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
