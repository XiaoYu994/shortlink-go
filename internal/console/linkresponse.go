package console

import (
	"time"

	"shortlink/internal/link"
)

/*
linkResponse 是管理接口统一的链接表示：创建、详情、修改和列表项都用它。
expires_at、disabled_at、banned_at 和 ban_reason 为空时省略；
status 总是有，按 banned、disabled、expired、active 的优先级派生。
ban_source 只给平台看，不对外。
启用 analytics 模块时 clicks 和 clicks_today 总是输出（包括 0）；
关闭 analytics 模块时指针留空，JSON 省略这两个字段，不伪造 0（ADR 0029）。
*/
type linkResponse struct {
	Code        string      `json:"code"`
	URL         string      `json:"url"`
	Status      link.Status `json:"status"`
	ExpiresAt   *time.Time  `json:"expires_at,omitempty"`
	DisabledAt  *time.Time  `json:"disabled_at,omitempty"`
	BannedAt    *time.Time  `json:"banned_at,omitempty"`
	BanReason   string      `json:"ban_reason,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	Clicks      *int64      `json:"clicks,omitempty"`
	ClicksToday *int64      `json:"clicks_today,omitempty"`
}

func newLinkResponse(l link.AdminLink, includeAnalytics bool) linkResponse {
	resp := linkResponse{
		Code:       l.Code,
		URL:        l.URL,
		Status:     l.Status(time.Now()),
		ExpiresAt:  l.ExpiresAt,
		DisabledAt: l.DisabledAt,
		CreatedAt:  l.CreatedAt,
	}
	if includeAnalytics {
		clicks := l.Clicks
		clicksToday := l.ClicksToday
		resp.Clicks = &clicks
		resp.ClicksToday = &clicksToday
	}
	if l.Ban != nil {
		resp.BannedAt = &l.Ban.At
		resp.BanReason = string(l.Ban.Reason)
	}
	return resp
}

// newCreatedLinkResponse 把刚创建的 Link 转成响应；启用统计时点击数为 0，关闭时省略统计字段。
func newCreatedLinkResponse(l *link.Link, includeAnalytics bool) linkResponse {
	return newLinkResponse(link.AdminLink{Link: *l}, includeAnalytics)
}
