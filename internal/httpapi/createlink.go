package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"shortlink/internal/link"
)

/*
maxBodyBytes 限制创建链接请求体的大小。
JSON 可以把每个字符写成 \uXXXX，最坏情况下 URL 的一个字节占 6 字节。
结构按 {"url":"...","expires_at":"..."} 算，时间用最长的 RFC3339
（9 位小数加数值时区）。再留 64 字节给空白，避免误杀合法请求。
*/
const (
	maxExpiresAtJSON = len("2006-01-02T15:04:05.999999999-07:00")
	maxBodyBytes     = link.MaxURLLength*6 + len(`{"url":"","expires_at":""}`) + maxExpiresAtJSON + 64
)

// errExtraJSON 表示第一个 JSON 值后面还有另一个值。
var errExtraJSON = errors.New("extra JSON value")

type createLinkRequest struct {
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type createLinkResponse struct {
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

/*
handleCreate 解析并校验 JSON 请求体，创建短链后返回 201 和 Location。
请求体先限制大小并整体解码，避免只读到第一个 JSON 值就放过尾部内容。
*/
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(maxBodyBytes))

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

	req, err := decodeCreateRequest(body)
	if err != nil {
		s.writeError(r.Context(), w, http.StatusBadRequest, createRequestMessage(err))
		return
	}

	l, err := link.NewLink(req.URL, req.ExpiresAt)
	if err != nil {
		if verr, ok := errors.AsType[*link.ValidationError](err); ok {
			s.writeError(r.Context(), w, http.StatusBadRequest, verr.Error())
			return
		}
		s.logger.ErrorContext(r.Context(), "new link", "err", err)
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}

	// 租户来自鉴权中间件。请求 JSON 里没有这个字段，客户端传了会被当成未知字段拒绝。
	tn, ok := principalFrom(r.Context())
	if !ok {
		s.logger.ErrorContext(r.Context(), "create link", "err", errors.New("missing tenant"))
		s.writeError(r.Context(), w, http.StatusInternalServerError, "internal error")
		return
	}
	l.TenantID = tn.ID

	if err := s.store.Create(r.Context(), l); err != nil {
		s.handleStoreError(w, r, "create link", err)
		return
	}

	w.Header().Set("Location", redirectPrefix+l.Code)
	s.writeJSON(r.Context(), w, http.StatusCreated, createLinkResponse{
		Code: l.Code, URL: l.URL, ExpiresAt: l.ExpiresAt,
	})
}

/*
decodeCreateRequest 解码创建请求。

未知字段默认会被丢掉。expire_at 这种拼写会变成没传，链接就永不过期。
DisallowUnknownFields 只在 Decoder 上，而 Decoder 只消费第一个 JSON 值，
所以还要再读一次，确认后面没有另一个值。
*/
func decodeCreateRequest(body []byte) (createLinkRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var req createLinkRequest
	if err := dec.Decode(&req); err != nil {
		return createLinkRequest{}, err
	}

	// RawMessage 接住第二个值。解进空结构体会把对方的字段报成未知字段。
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return createLinkRequest{}, errExtraJSON
		}
		return createLinkRequest{}, err
	}
	return req, nil
}

/*
createRequestMessage 把解码错误收成给客户端的短句。
expires_at 写成 "tomorrow" 时 JSON 本身是合法的，不能报成 invalid JSON body。
encoding/json 把未知字段报成普通 error，文本以 json: unknown field 开头。
*/
func createRequestMessage(err error) string {
	var parseErr *time.ParseError
	if errors.As(err, &parseErr) {
		return "invalid expires_at"
	}
	// 数字、布尔、数组、对象不会进 time.Time 的解析，而是 UnmarshalTypeError。
	// Field 用的是 json 名，例如 expires_at。
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && strings.Contains(typeErr.Field, "expires_at") {
		return "invalid expires_at"
	}
	if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return "unknown field " + rest
	}
	return "invalid JSON body"
}
