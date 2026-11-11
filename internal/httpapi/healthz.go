package httpapi

import (
	"io"
	"net/http"
)

/*
handleHealthz 返回固定的 ok，供负载均衡和容器探活使用。
它故意不访问存储：进程活着就返回 200，依赖状态不属于 liveness 的职责。
*/
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}
