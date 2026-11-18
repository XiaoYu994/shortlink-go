package httpapi

import (
	"time"

	"shortlink/internal/link"
)

/*
linkResponse 是管理接口统一的链接表示：创建、详情、修改和列表项都用它。
expires_at、disabled_at、banned_at 和 ban_reason 为空时省略；
status 总是有，按 banned、disabled、expired、active 的优先级派生。
ban_source 只给平台看，不对外。clicks 和 clicks_today
在创建响应里是 0，之后由存储层填。
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
	Clicks      int64       `json:"clicks"`
	ClicksToday int64       `json:"clicks_today"`
}

func newLinkResponse(l link.AdminLink) linkResponse {
	resp := linkResponse{
		Code:        l.Code,
		URL:         l.URL,
		Status:      l.Status(time.Now()),
		ExpiresAt:   l.ExpiresAt,
		DisabledAt:  l.DisabledAt,
		CreatedAt:   l.CreatedAt,
		Clicks:      l.Clicks,
		ClicksToday: l.ClicksToday,
	}
	if l.Ban != nil {
		resp.BannedAt = &l.Ban.At
		resp.BanReason = string(l.Ban.Reason)
	}
	return resp
}

// newCreatedLinkResponse 把刚创建的 Link 转成响应；新链接还没有点击。
func newCreatedLinkResponse(l *link.Link) linkResponse {
	return newLinkResponse(link.AdminLink{Link: *l})
}
