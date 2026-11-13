/*
Package link 是短链的核心业务：Link 结构体、业务错误和 URL 校验（第 3、4、6 课）。

Link 只表达业务数据，不关心短码怎么生成、存在哪里。创建流程是：
NewLink 校验并构造 Link，Store 负责分配 ID 和短码并持久化。

只允许 http 和 https 的绝对 URL；其他 scheme（javascript:、file: 等）
既不是短链的使用场景，也会带来安全风险。
*/
package link

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

/*
MaxURLLength 限制原始 URL 的字节数，避免把超大输入存进数据库。
导出是因为 HTTP 层要按它计算请求体上限：JSON 转义最坏情况下，URL 的每个字节会占 6 字节。
*/
const MaxURLLength = 2048

// ErrNotFound 表示按短码查不到链接；HTTP 层用 errors.Is 映射成 404。
var ErrNotFound = errors.New("link not found")

/*
ValidationError 表示调用方传入的链接数据不合法。
HTTP 层用 errors.As 取出它，映射成 400。
*/
type ValidationError struct {
	Field  string // 出错的字段，例如 "url"
	Reason string // 面向调用方的原因
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("link: invalid %s: %s", e.Field, e.Reason)
}

/*
Link 是一条短链。

ID 和 Code 由 Store 在创建时分配，NewLink 负责校验 URL 和到期时间；
CreatedAt 也由存储层设置，因为数据库和内存实现的时钟来源可能不同。
TenantID 由鉴权中间件写入，不从创建请求的 JSON 读取。
*/
type Link struct {
	ID   int64
	Code string
	URL  string
	// ExpiresAt 为 nil 表示永不过期。time.Time 的零值是公元 1 年，不能拿来表示「没有」。
	ExpiresAt *time.Time
	CreatedAt time.Time
	// TenantID 是这条链接所属的租户。
	TenantID int64
}

/*
Expired 报告链接在 now 是否已经过期。
到期时刻本身算过期，和 SQL 的 expires_at > now() 是同一条界限。
*/
func (l *Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !l.ExpiresAt.After(now)
}

/*
NewLink 校验 rawURL 并构造一条尚未持久化的链接。
expiresAt 为 nil 表示永不过期；非 nil 时必须是将来的时间。
校验失败返回 *ValidationError，调用方可以用 errors.As 取出。
*/
func NewLink(rawURL string, expiresAt *time.Time) (*Link, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, &ValidationError{Field: "url", Reason: "must not be empty"}
	}
	if len(rawURL) > MaxURLLength {
		return nil, &ValidationError{Field: "url", Reason: fmt.Sprintf("longer than %d bytes", MaxURLLength)}
	}
	// TrimSpace 只去掉首尾空白；url.Parse 对 path 和 query 里的空格很宽容，
	// 会悄悄接受，这里显式拒绝，保证存进库的都是写明白的 URL。
	if strings.ContainsFunc(rawURL, unicode.IsSpace) {
		return nil, &ValidationError{Field: "url", Reason: "must not contain whitespace"}
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &ValidationError{Field: "url", Reason: "invalid URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &ValidationError{Field: "url", Reason: "only http and https are allowed"}
	}
	if u.Hostname() == "" {
		return nil, &ValidationError{Field: "url", Reason: "missing host"}
	}
	if expiresAt != nil {
		// 存 UTC 副本。调用方之后改自己的 time.Time，不影响这条链接。
		t := expiresAt.UTC()
		if !t.After(time.Now()) {
			return nil, &ValidationError{Field: "expires_at", Reason: "must be in the future"}
		}
		expiresAt = &t
	}

	return &Link{URL: rawURL, ExpiresAt: expiresAt}, nil
}
