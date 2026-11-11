package httpapi

import "net/http"

const (
	// apiV1Prefix 是管理 API 的版本前缀。
	apiV1Prefix = "/api/v1"
	// redirectPrefix 是公开短链的固定前缀。短链一旦发出去就是永久地址，不能改路径。
	redirectPrefix = "/r/"
)

// Routes 返回注册好全部路由的 handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST "+apiV1Prefix+"/links", s.handleCreate)
	mux.HandleFunc("GET "+redirectPrefix+"{code}", s.handleRedirect)
	return mux
}
