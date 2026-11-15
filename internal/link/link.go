/*
Package link 是短链的核心业务：Link 结构体、业务错误、校验和管理侧类型（第 3、4、6、11 课）。

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
// 管理接口把它同时用于「不存在」和「属于其他租户」，不透露链接是否存在。
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
AdminLink 是管理接口看到的链接：Link 加上累计和今日点击。

计数会随点击变化，也不该跟着正缓存活一小时，所以不放进 Link，
缓存里仍然只存 Link。见 ADR 0011。
*/
type AdminLink struct {
	Link
	Clicks      int64
	ClicksToday int64
}

// Sort 是管理列表的排序字段；列表固定降序。
type Sort string

const (
	SortID          Sort = "id"           // 默认，按 ID 稳定排序
	SortClicks      Sort = "clicks"       // 按历史累计点击
	SortClicksToday Sort = "clicks_today" // 按今日点击
)

/*
ListCursor 是上一页最后一条记录的排序键，字段按 Sort 选用：
SortID 只用 ID；SortClicks 用 Clicks 加 ID；SortClicksToday 用 ClicksToday 加 ID。
ID 用来在排序值相同时消歧，避免翻页跳过或重复整组。
*/
type ListCursor struct {
	ID          int64
	Clicks      int64
	ClicksToday int64
}

/*
ListOptions 是管理列表查询。
Limit 是硬上限，调用方想判断有没有下一页时可以多要一条。
Cursor 为 nil 表示第一页。
*/
type ListOptions struct {
	TenantID int64
	Limit    int
	Sort     Sort
	Cursor   *ListCursor
}

/*
Update 描述 PATCH 要修改的字段。

URL 为 nil 表示不修改。
ExpiresAt 为 nil 且 ClearExpiresAt 为 false 表示不修改；
ClearExpiresAt 为 true 表示把 expires_at 清成 NULL（永不过期），
它和 ExpiresAt 不能同时设置。
*/
type Update struct {
	URL            *string
	ExpiresAt      *time.Time
	ClearExpiresAt bool
}

// Empty 报告 Update 有没有任何要修改的字段。
func (u Update) Empty() bool {
	return u.URL == nil && u.ExpiresAt == nil && !u.ClearExpiresAt
}

// DayClicks 是某一天（UTC）的点击总数。
type DayClicks struct {
	Day    time.Time
	Clicks int64
}

// DimensionCount 是某个维度取值的点击数，维度由查询决定。
type DimensionCount struct {
	Value  string
	Clicks int64
}

// DayVisitors 是某一天（UTC）去重后的访客数。
type DayVisitors struct {
	Day      time.Time
	Visitors int64
}

/*
Stats 是统计接口的领域结果，日表里没有数据的日期不出现在切片里。
补零和固定类别顺序属于 API 表示，由 HTTP 层完成。
*/
type Stats struct {
	Code           string
	TotalClicks    int64 // links.clicks，终身累计
	RangeClicks    int64 // 时间范围内日表之和
	From           time.Time
	To             time.Time
	Clicks         []DayClicks
	Referrers      []DimensionCount // 最多 10 个，按点击数降序、取值升序
	Devices        []DimensionCount
	Clients        []DimensionCount
	DailyVisitors  []DayVisitors
	UniqueVisitors int64 // 每日 UV 相加，不是跨天去重
}

/*
Expired 报告链接在 now 是否已经过期。
到期时刻本身算过期，和 SQL 的 expires_at > now() 是同一条界限。
*/
func (l *Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !l.ExpiresAt.After(now)
}

/*
ValidateURL 校验原始 URL，返回去掉首尾空白后的值。
创建和修改共用这一份规则，避免两处漂移；失败返回 *ValidationError。
*/
func ValidateURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", &ValidationError{Field: "url", Reason: "must not be empty"}
	}
	if len(rawURL) > MaxURLLength {
		return "", &ValidationError{Field: "url", Reason: fmt.Sprintf("longer than %d bytes", MaxURLLength)}
	}
	// TrimSpace 只去掉首尾空白；url.Parse 对 path 和 query 里的空格很宽容，
	// 会悄悄接受，这里显式拒绝，保证存进库的都是写明白的 URL。
	if strings.ContainsFunc(rawURL, unicode.IsSpace) {
		return "", &ValidationError{Field: "url", Reason: "must not contain whitespace"}
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", &ValidationError{Field: "url", Reason: "invalid URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", &ValidationError{Field: "url", Reason: "only http and https are allowed"}
	}
	if u.Hostname() == "" {
		return "", &ValidationError{Field: "url", Reason: "missing host"}
	}
	return rawURL, nil
}

/*
ValidateExpiresAt 校验到期时间并返回 UTC 副本。
nil 表示永不过期；非 nil 时必须是将来的时间。
*/
func ValidateExpiresAt(expiresAt *time.Time) (*time.Time, error) {
	if expiresAt == nil {
		return nil, nil
	}
	// 存 UTC 副本。调用方之后改自己的 time.Time，不影响这条链接。
	t := expiresAt.UTC()
	if !t.After(time.Now()) {
		return nil, &ValidationError{Field: "expires_at", Reason: "must be in the future"}
	}
	return &t, nil
}

/*
NewLink 校验 rawURL 并构造一条尚未持久化的链接。
expiresAt 为 nil 表示永不过期；非 nil 时必须是将来的时间。
校验失败返回 *ValidationError，调用方可以用 errors.As 取出。
*/
func NewLink(rawURL string, expiresAt *time.Time) (*Link, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	expires, err := ValidateExpiresAt(expiresAt)
	if err != nil {
		return nil, err
	}
	return &Link{URL: u, ExpiresAt: expires}, nil
}
