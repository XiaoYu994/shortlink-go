package console

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"

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

/*
badRequestError 的文本就是要返回给客户端的短句。
解码时已经能判断的错误（例如 PATCH 什么都没改）用它，
不用再经过 decodeErrorMessage 的 JSON 语法映射。
*/
type badRequestError string

func (e badRequestError) Error() string { return string(e) }

/*
decodeStrict 按严格模式解码单个 JSON 值：
使用 Go 标准库 encoding/json/v2 一次性拒绝未知字段、重复键、大小写偏差与尾部多余内容，
并保留带 JSON Pointer 与哨兵错误的结构化 *jsonv2.SemanticError，避免靠截取 err.Error() 字符串分流。
*/
func decodeStrict(body []byte, dst any) error {
	return jsonv2.Unmarshal(body, dst, jsonv2.RejectUnknownMembers(true))
}

// isJSONFieldError 报告 err 是否是指定顶层 JSON 字段的值解码失败（不含未知字段错误）。
func isJSONFieldError(err error, field string) bool {
	serr, ok := errors.AsType[*jsonv2.SemanticError](err)
	if !ok || errors.Is(serr.Err, jsonv2.ErrUnknownName) {
		return false
	}
	return serr.JSONPointer == jsontext.Pointer("/"+field)
}

/*
decodeErrorMessage 把通用 JSON 解码错误收成给客户端的短句。
未知字段返回字段名（防止 expires_at 拼错成 expire_at 时静默忽略），其余语法与类型错统一收成 invalid JSON body。
需要按字段返回专属提示的接口先用 isJSONFieldError 映射，再回落到这里。
*/
func decodeErrorMessage(err error) string {
	if serr, ok := errors.AsType[*jsonv2.SemanticError](err); ok && errors.Is(serr.Err, jsonv2.ErrUnknownName) {
		return fmt.Sprintf("unknown field %q", serr.JSONPointer.LastToken())
	}
	return "invalid JSON body"
}

// expiresAtDecodeMessage 供带 expires_at 字段的请求复用：把 expires_at 的格式或类型错统一报成 invalid expires_at。
func expiresAtDecodeMessage(err error) string {
	if isJSONFieldError(err, "expires_at") {
		return "invalid expires_at"
	}
	return decodeErrorMessage(err)
}

/*
readSmallRequestBody 读取管理 API 共用的小型 JSON 请求体，并限制大小。
返回 false 表示已经处理完响应（客户端断开、413、408、400 或服务端误关 body 的 500），handler 直接返回。

一次读完整个 body：MaxBytesReader 的超限错误才能暴露出来，
不会只解析第一个 JSON 值就把没读的尾巴放过去。
客户端引发的读体失败（畸形 chunked、慢速读超时等）只记 Warn，不记 Error，
避免状态码 4xx 与服务端 Error 告警级别冲突。
*/
func readSmallRequestBody(logger *slog.Logger, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if httpx.IsClientGone(logger, r) {
		return nil, false
	}
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
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			logger.ErrorContext(r.Context(), "read request body", "err", err)
			httpx.WriteError(logger, r.Context(), w, http.StatusInternalServerError, "internal error")
			return nil, false
		}
		if isReadBodyTimeout(r.Context(), err) {
			logger.WarnContext(r.Context(), "read request body timed out", "err", err)
			httpx.WriteError(logger, r.Context(), w, http.StatusRequestTimeout, "request timed out")
			return nil, false
		}
		logger.WarnContext(r.Context(), "read request body", "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, "read request body")
		return nil, false
	}
	return body, true
}

func isReadBodyTimeout(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}
	return false
}
