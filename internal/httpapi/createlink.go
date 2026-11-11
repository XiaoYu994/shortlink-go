package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"shortlink/internal/link"
)

// maxBodyBytes 限制创建链接请求体的大小；URL 本身最多 2048 字节。
const maxBodyBytes = 4 << 10

type createLinkRequest struct {
	URL string `json:"url"`
}

type createLinkResponse struct {
	Code string `json:"code"`
	URL  string `json:"url"`
}

/*
handleCreate 解析并校验 JSON 请求体，创建短链后返回 201 和 Location。
请求体先限制大小并整体解码，避免只读到第一个 JSON 值就放过尾部内容。
*/
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	/*
		一次读完整个 body：MaxBytesReader 的超限错误才能暴露出来，
		不会只解析第一个 JSON 值就把没读的尾巴放过去。
	*/
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if s.clientGone(r) {
			return
		}
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.writeError(r.Context(), w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		s.logger.ErrorContext(r.Context(), "read request body", "err", err)
		s.writeError(r.Context(), w, http.StatusBadRequest, "read request body")
		return
	}

	// Unmarshal 要求整个输入恰好是一个 JSON 值，尾部多一个字符也会报错。
	var req createLinkRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(r.Context(), w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	l, err := link.NewLink(req.URL)
	if err != nil {
		if verr, ok := errors.AsType[*link.ValidationError](err); ok {
			s.writeError(r.Context(), w, http.StatusBadRequest, verr.Error())
			return
		}
		s.logger.ErrorContext(r.Context(), "new link", "err", err)
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.store.Create(r.Context(), l); err != nil {
		if s.clientGone(r) {
			return
		}
		s.logger.ErrorContext(r.Context(), "create link", "err", err)
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Location", redirectPrefix+l.Code)
	s.writeJSON(r.Context(), w, http.StatusCreated, createLinkResponse{Code: l.Code, URL: l.URL})
}
