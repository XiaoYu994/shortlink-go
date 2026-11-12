package httpapi

import "net/http"

const (
	// apiV1Prefix 是管理 API 的版本前缀。
	apiV1Prefix = "/api/v1"
	// redirectPrefix 是公开短链的固定前缀。短链一旦发出去就是永久地址，不能改路径。
	redirectPrefix = "/r/"
)

/*
Routes 返回注册好全部路由的 handler。
requestID 在最外层，里面的 logging、recoverer 和 handler 拿到的 context 里已经有编号。
logging 在 recoverer 外面：panic 被写成 500 之后，访问日志里的状态码也是 500。
timeout 在最里层：它只约束 handler，日志和 panic 兜底要完整执行，不能被时限打断。
*/
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST "+apiV1Prefix+"/links", s.handleCreate)
	mux.HandleFunc("GET "+redirectPrefix+"{code}", s.handleRedirect)
	return chain(mux, requestID(), logging(s.logger), recoverer(s.logger), timeout(s.requestTimeout))
}
