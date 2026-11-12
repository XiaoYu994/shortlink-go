package httpapi

import (
	"errors"
	"net/http"

	"shortlink/internal/link"
)

/*
handleRedirect 把 /r/{code} 302 跳转到原始 URL。
短码不存在映射成 404，存储故障映射成 500；客户端断开时直接返回，不写响应。
*/
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}

	code := r.PathValue("code")

	l, err := s.store.GetByCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, link.ErrNotFound) {
			s.writeError(r.Context(), w, http.StatusNotFound, "link not found")
			return
		}
		s.handleStoreError(w, r, "get link", err)
		return
	}

	http.Redirect(w, r, l.URL, http.StatusFound)
}
