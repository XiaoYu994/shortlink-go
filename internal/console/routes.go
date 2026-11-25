package console

import (
	"net/http"

	"shortlink/internal/identity"
)

const (
	// apiV1Prefix 是管理 API 的版本前缀。
	apiV1Prefix = "/api/v1"
	// redirectPrefix 用于创建响应里的 Location 拼装（/r/{code}）。
	redirectPrefix = "/r/"
)

/*
Routes 返回管理进程的路由：能力发现、已注册 AuthMethod 与会话路由、核心短链 CRUD、
可选业务模块、前端 SPA 以及运维端点。
鉴权和限流只包受保护的管理 API。能力发现、登录流程、登出、/me、存活/就绪检查与指标不经过按租户限流。
每个管理路由单独包：ServeMux 没有路由组，套在 /api/v1/ 前缀上的中间件不会被更具体的路由继承。
*/
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.Handle("GET "+capabilitiesPath, s.ipRateLimit(http.HandlerFunc(s.handleCapabilities)))
	if s.auth != nil {
		s.auth.registerRoutes(mux, s.ipRateLimit)
	}
	reg := RouteRegistrar{
		Mux:         mux,
		IPRateLimit: s.ipRateLimit,
		Console:     s.consoleRoute,
		CSRF:        s.csrf,
		Logger:      s.logger,
	}
	for _, m := range s.modules {
		m.RegisterRoutes(reg)
	}
	mux.Handle("POST "+apiV1Prefix+"/links", s.consoleRoute(identity.PermLinksWrite, s.handleCreate))
	mux.Handle("GET "+apiV1Prefix+"/links", s.consoleRoute(identity.PermLinksRead, s.handleList))
	mux.Handle("GET "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksRead, s.handleGet))
	mux.Handle("PATCH "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksWrite, s.handleUpdate))
	mux.Handle("DELETE "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksWrite, s.handleDelete))
	if s.web != nil {
		mux.Handle("/", s.web)
	}
}

func (s *Server) Routes() http.Handler {
	return s.base.Handler(s.registerRoutes)
}

// RoutesBusiness 只返回管理 API 与前端路由（供启用 CONSOLE_OPS_ADDR 时使用）。
func (s *Server) RoutesBusiness() http.Handler {
	return s.base.BusinessHandler(s.registerRoutes)
}

// OpsHandler 只返回 /healthz、/readyz、/metrics 运维端点。
func (s *Server) OpsHandler() http.Handler {
	return s.base.OpsHandler()
}

/*
consoleRoute 给管理 handler 套上鉴权前按 IP 限流、鉴权、按租户限流和权限校验。
顺序不能反：
1. ipRateLimit 挡掉鉴权前伪造 token 的单源洪水；
2. authenticate 确定租户和执行者身份；
3. rateLimit 放在 requirePerm 外面，避免低权限密钥疯狂刷写接口时绕过限流（也保护以后更重的 Authorizer 实现）；
4. requirePerm 最后按动作校验权限。
超限或无权限的请求都不会进入 handler，也不会查库或更新缓存。
*/
func (s *Server) consoleRoute(perm identity.Perm, h http.HandlerFunc) http.Handler {
	return s.ipRateLimit(s.authenticate(s.rateLimit(s.requirePerm(perm, h))))
}
