package httpapi

import (
	"time"

	"shortlink/internal/link"
)

/*
linkResponse 是管理接口统一的链接表示：创建、详情、修改和列表项都用它。
expires_at 为空时省略，和创建接口原来的行为一致；clicks 和 clicks_today
在创建响应里是 0，之后由存储层填。
*/
type linkResponse struct {
	Code        string     `json:"code"`
	URL         string     `json:"url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Clicks      int64      `json:"clicks"`
	ClicksToday int64      `json:"clicks_today"`
}

func newLinkResponse(l link.AdminLink) linkResponse {
	return linkResponse{
		Code:        l.Code,
		URL:         l.URL,
		ExpiresAt:   l.ExpiresAt,
		CreatedAt:   l.CreatedAt,
		Clicks:      l.Clicks,
		ClicksToday: l.ClicksToday,
	}
}

// newCreatedLinkResponse 把刚创建的 Link 转成响应；新链接还没有点击。
func newCreatedLinkResponse(l *link.Link) linkResponse {
	return newLinkResponse(link.AdminLink{Link: *l})
}
