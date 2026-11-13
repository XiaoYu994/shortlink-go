package httpapi

import (
	"errors"
	"net/http"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

/*
handleRedirect 把 /r/{code} 302 跳转到原始 URL。
短码不存在或已过期映射成 404，存储故障映射成 500，处理超时映射成 504。
客户端断开时直接返回，不写响应，也不记点击。
ServeMux 的 GET 模式同时匹配 HEAD。点击只统计 GET：探针、预览和扫描器的 HEAD 仍返回 302。
*/
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}

	code := r.PathValue("code")
	// 非法短码不可能存在。先 404，避免超长 code 变成超长缓存 key。
	if !idgen.ValidCode(code) {
		s.writeError(r.Context(), w, http.StatusNotFound, "link not found")
		return
	}

	l, err := s.store.GetByCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, link.ErrNotFound) {
			s.writeError(r.Context(), w, http.StatusNotFound, "link not found")
			return
		}
		s.handleStoreError(w, r, "get link", err, "code", code)
		return
	}

	/*
		查询返回之后再看一次。singleflight 不接收等待者的 ctx，
		等待期间断开的请求仍会拿到领头请求的链接。
		连接已经不在，这次跳转没有发生。
	*/
	if s.clientGone(r) {
		return
	}
	if r.Method == http.MethodGet {
		s.recordClick(l.ID)
	}
	http.Redirect(w, r, l.URL, http.StatusFound)
}

/*
recordClick 在知道链接存在之后调用。
clicks 为 nil 时跳过。这里认的是接口本身为 nil，
(*click.Recorder)(nil) 放进接口后不等于 nil，调用 Record 会 panic。
*/
func (s *Server) recordClick(id int64) {
	if s.clicks == nil || id <= 0 {
		return
	}
	s.clicks.Record(id)
}
