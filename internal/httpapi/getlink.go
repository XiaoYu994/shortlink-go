package httpapi

import (
	"net/http"

	"shortlink/internal/idgen"
)

/*
handleGet 返回单条详情，包含已过期、已停用和已封禁的链接；被封禁的链接只读。
不存在和其他租户的链接都返回 404，不透露链接是否存在。
*/
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	code, ok := s.validLinkCode(w, r)
	if !ok {
		return
	}

	got, err := s.admin.GetForTenant(r.Context(), p.Tenant.ID, code)
	if err != nil {
		s.handleAdminStoreError(w, r, "get link", err, code)
		return
	}
	s.writeJSON(r.Context(), w, http.StatusOK, newLinkResponse(*got))
}

/*
validLinkCode 取出路径里的短码并校验格式。
管理接口对非法短码也按 404 处理，避免无意义的数据库查询。
*/
func (s *Server) validLinkCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	code := r.PathValue("code")
	if !idgen.ValidCode(code) {
		s.writeLinkNotFound(r, w)
		return "", false
	}
	return code, true
}

// writeLinkNotFound 统一管理接口的 404 正文，不区分不存在和其他租户。
func (s *Server) writeLinkNotFound(r *http.Request, w http.ResponseWriter) {
	s.writeError(r.Context(), w, http.StatusNotFound, "link not found")
}
