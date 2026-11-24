package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"shortlink/internal/httpx"
	"shortlink/internal/link"
)

/*
maxBodyBytes 限制创建与轻量管理 JSON 请求体的大小。
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

/*
badRequestError 的文本就是要返回给客户端的短句。
解码时已经能判断的错误（例如 PATCH 什么都没改）用它，
不用再经过 requestMessage 的 JSON 语法映射。
*/
type badRequestError string

func (e badRequestError) Error() string { return string(e) }

/*
decodeStrict 解码一个恰好完整的 JSON 值：
未知字段返回错误，第一个值后面还有内容也返回错误。
RawMessage 接住第二个值，解进空结构体会把对方的字段报成未知字段。
*/
func decodeStrict(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}

	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errExtraJSON
		}
		return err
	}
	return nil
}

/*
readSmallRequestBody 读取管理 API 共用的小型 JSON 请求体，并限制大小。
返回 false 表示已经写过响应（客户端断开、413 或读失败），handler 直接返回。

一次读完整个 body：MaxBytesReader 的超限错误才能暴露出来，
不会只解析第一个 JSON 值就把没读的尾巴放过去。
*/
func readSmallRequestBody(logger *slog.Logger, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxBodyBytes))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if httpx.IsClientGone(logger, r) {
			return nil, false
		}
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			httpx.WriteError(logger, r.Context(), w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		logger.ErrorContext(r.Context(), "read request body", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "read request body")
		return nil, false
	}
	return body, true
}
