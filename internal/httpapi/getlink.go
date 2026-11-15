package httpapi

import (
	"errors"
	"net/http"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

/*
handleGet 返回单条详情，包含已过期链接。
不存在和其他租户的链接都返回 404，不透露链接是否存在。
*/
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}
	tn, ok := s.principal(w, r)
	if !ok {
		return
	}
	code, ok := s.validLinkCode(w, r)
	if !ok {
		return
	}

	got, err := s.admin.GetForTenant(r.Context(), tn.ID, code)
	if err != nil {
		if errors.Is(err, link.ErrNotFound) {
			s.writeLinkNotFound(r, w)
			return
		}
		s.handleStoreError(w, r, "get link", err, "code", code)
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
