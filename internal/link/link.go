/*
Package link 是短链的核心业务：Link 结构体、业务错误、校验、状态派生和管理侧类型（第 3、4、6、11 课）。

Link 只表达业务数据，不关心短码怎么生成、存在哪里。创建流程是：
NewLink 校验并构造 Link，Store 负责分配 ID 和短码并持久化。

过期、停用和封禁是三个独立的事实，可以同时成立；Status 按固定优先级派生
对外展示的状态，Servable 是「能不能跳转」的唯一判断。

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
ErrQuotaExceeded 表示租户本月（UTC 自然月）的创建配额已经用完，不能再创建。
配额由 tenants.link_usage、link_usage_month 和 link_quota 在写库时原子判断；
HTTP 层用 errors.Is 映射成 403。删除链接不返还配额，
下个月 1 日 00:00 UTC 起用量重新从 0 计。
*/
var ErrQuotaExceeded = errors.New("link quota exceeded")

/*
ErrBanned 表示链接被平台封禁。租户不能修改或删除被封禁的链接，
HTTP 层用 errors.Is 映射成 403；跳转视角把它和过期、停用一样看成找不到。
*/
var ErrBanned = errors.New("link is banned")

/*
BanReason 是封禁原因的封闭类别集合，不是自由文本。
它会原样出现在租户的 API 响应里，前端按类别给文案；加类别要同时改这里、
数据库 CHECK 和迁移。
*/
type BanReason string

const (
	BanReasonPhishing BanReason = "phishing"
	BanReasonMalware  BanReason = "malware"
	BanReasonAdult    BanReason = "adult"
	BanReasonGambling BanReason = "gambling"
	BanReasonDrugs    BanReason = "drugs"
	BanReasonSpam     BanReason = "spam"
	BanReasonOther    BanReason = "other"
)

// Valid 报告 r 是不是允许的封禁原因。
func (r BanReason) Valid() bool {
	switch r {
	case BanReasonPhishing, BanReasonMalware, BanReasonAdult, BanReasonGambling,
		BanReasonDrugs, BanReasonSpam, BanReasonOther:
		return true
	default:
		return false
	}
}

// BanSource 记录封禁是谁做出的：平台人工还是风控。只在平台侧使用，不对外。
type BanSource string

const (
	BanSourceOperator BanSource = "operator"
	BanSourceRisk     BanSource = "risk"
)

// Valid 报告 s 是不是允许的封禁来源。
func (s BanSource) Valid() bool {
	switch s {
	case BanSourceOperator, BanSourceRisk:
		return true
	default:
		return false
	}
}

/*
Ban 是一条链接的封禁信息。数据库里 banned_at、ban_reason、ban_source
三个字段同有同无，所以 Link 里用一个指针表达「有没有封禁」。
*/
type Ban struct {
	At     time.Time
	Reason BanReason
	Source BanSource
}

/*
Status 是对外可见的链接状态。三种事实可以同时成立，状态按优先级派生：
banned、disabled、expired、active。顺序按租户能不能自己解决来排：
封禁要找平台，停用和过期自己可以改。
*/
type Status string

const (
	StatusActive   Status = "active"
	StatusExpired  Status = "expired"
	StatusDisabled Status = "disabled"
	StatusBanned   Status = "banned"
)

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
	// DisabledAt 是租户停用的时间，nil 表示没有停用。
	DisabledAt *time.Time
	// Ban 为 nil 表示没有封禁。数据库里三个封禁字段同有同无。
	Ban *Ban
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
Disabled 为 nil 表示不改；true 停用，false 启用。重复停用保留第一次的时间。
*/
type Update struct {
	URL            *string
	ExpiresAt      *time.Time
	ClearExpiresAt bool
	Disabled       *bool
}

// Empty 报告 Update 有没有任何要修改的字段。
func (u Update) Empty() bool {
	return u.URL == nil && u.ExpiresAt == nil && !u.ClearExpiresAt && u.Disabled == nil
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
Status 返回链接在 now 的派生状态，按 banned、disabled、expired、active
的顺序取第一个成立的。三种事实可以同时成立，优先级只是选择对外的展示。
*/
func (l *Link) Status(now time.Time) Status {
	switch {
	case l.Ban != nil:
		return StatusBanned
	case l.DisabledAt != nil:
		return StatusDisabled
	case l.Expired(now):
		return StatusExpired
	default:
		return StatusActive
	}
}

// Servable 报告链接在 now 是否可以跳转。只有 active 可以。
// SQL 跳转查询的过滤条件是它的等价写法，两者要对同一组用例保持一致。
func (l *Link) Servable(now time.Time) bool {
	return l.Status(now) == StatusActive
}

/*
DayStart 取时间所在 UTC 日期的零点。
点击日聚合、统计范围和今日排序都用这个口径，避免各处按本地时区取整。
*/
func DayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// DayKey 返回 UTC 日期字符串，格式 2006-01-02，用作日聚合的键。
func DayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
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
