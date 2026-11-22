package console

import (
	"net/http"

	"shortlink/internal/httpx"
)

/*
handleDelete 硬删除链接和它的统计行，成功返回 204，无响应体。
不存在和其他租户的链接都返回 404。
删除不是强撤销：已经在途的跳转仍可能返回 302，之后的新请求才会 404。
*/
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(s.logger, r) {
		return
	}
	p, ok := principalOrInternalError(s.logger, w, r)
	if !ok {
		return
	}
	code, ok := s.validLinkCode(w, r)
	if !ok {
		return
	}

	if err := s.admin.Delete(r.Context(), p.Tenant.ID, code); err != nil {
		s.handleAdminStoreError(w, r, "delete link", err, code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
