package console

import (
	"net/http"
	"time"

	"shortlink/internal/click"
	"shortlink/internal/httpx"
	"shortlink/internal/link"
)

const (
	// defaultStatsDays 是默认范围：to - 7 天，两端所在整日都计入。
	defaultStatsDays = 7
	// maxStatsRange 是 from 到 to 的最大跨度，统计接口最多扫 90 天。
	maxStatsRange = 90 * 24 * time.Hour
)

type statsDayResponse struct {
	Day    string `json:"day"`
	Clicks int64  `json:"clicks"`
}

type statsVisitorsResponse struct {
	Day      string `json:"day"`
	Visitors int64  `json:"visitors"`
}

type statsReferrerResponse struct {
	Referrer string `json:"referrer"`
	Clicks   int64  `json:"clicks"`
}

type statsDeviceResponse struct {
	Device string `json:"device"`
	Clicks int64  `json:"clicks"`
}

type statsClientResponse struct {
	Client string `json:"client"`
	Clicks int64  `json:"clicks"`
}

/*
linkStatsResponse 是统计接口的表示。
clicks 和 daily_visitors 按天补零；devices 固定五个类别、clients 固定四个类别，
没有数据也返回 0，看板不需要自己对齐类别。
*/
type linkStatsResponse struct {
	Code           string                  `json:"code"`
	TotalClicks    int64                   `json:"total_clicks"`
	RangeClicks    int64                   `json:"range_clicks"`
	From           string                  `json:"from"`
	To             string                  `json:"to"`
	Clicks         []statsDayResponse      `json:"clicks"`
	Referrers      []statsReferrerResponse `json:"referrers"`
	Devices        []statsDeviceResponse   `json:"devices"`
	Clients        []statsClientResponse   `json:"clients"`
	DailyVisitors  []statsVisitorsResponse `json:"daily_visitors"`
	UniqueVisitors int64                   `json:"unique_visitors"`
}

/*
handleStats 返回一条链接的日趋势、Top 来源、设备、客户端和每日访客。
查询参数 from、to 是 RFC3339，转成 UTC 后按天取整；默认 to 为今天，
from 为 to - 7 天。要求 from < to，跨度不超过 90 天。
过期链接可以看，已删除或其他租户的链接返回 404。
*/
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(s.logger, r) {
		return
	}
	p, ok := principalOrInternalError(s.logger, w, r)
	if !ok {
		return
	}
	code, ok := s.validLinkCode(w, r)
	if !ok {
		return
	}

	from, to, err := parseStatsRange(r, time.Now().UTC())
	if err != nil {
		httpx.WriteError(s.logger, r.Context(), w, http.StatusBadRequest, err.Error())
		return
	}

	stats, err := s.stats.Stats(r.Context(), p.Tenant.ID, code, from, to)
	if err != nil {
		s.handleAdminStoreError(w, r, "link stats", err, code)
		return
	}
	httpx.WriteJSON(s.logger, r.Context(), w, http.StatusOK, newLinkStatsResponse(stats))
}

/*
parseStatsRange 解析 from/to 并归一到 UTC 日期的零点。
两个参数都不传时用默认范围；只传一个时另一个按默认规则补。
*/
func parseStatsRange(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	query := r.URL.Query()

	to, hasTo, err := parseDayParam(query.Get("to"), "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from, hasFrom, err := parseDayParam(query.Get("from"), "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	if !hasTo {
		to = link.DayStart(now)
	}
	if !hasFrom {
		from = to.AddDate(0, 0, -defaultStatsDays)
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, badRequestError("from must be before to")
	}
	if to.Sub(from) > maxStatsRange {
		return time.Time{}, time.Time{}, badRequestError("range must not exceed 90 days")
	}
	return from, to, nil
}

/*
parseDayParam 把 RFC3339 时间转成 UTC 日期零点。第二个返回值表示参数是否出现，
不用零值判断，避免把合法的公元 1 年当成没传。
*/
func parseDayParam(raw, name string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, badRequestError("invalid " + name)
	}
	return link.DayStart(at), true, nil
}

// newLinkStatsResponse 把领域结果补零成 API 表示。
func newLinkStatsResponse(stats *link.Stats) linkStatsResponse {
	deviceCategories := click.DeviceCategories()
	clientCategories := click.ClientCategories()
	resp := linkStatsResponse{
		Code:           stats.Code,
		TotalClicks:    stats.TotalClicks,
		RangeClicks:    stats.RangeClicks,
		From:           stats.From.Format(time.RFC3339),
		To:             stats.To.Format(time.RFC3339),
		Clicks:         make([]statsDayResponse, 0, daysBetween(stats.From, stats.To)),
		Referrers:      make([]statsReferrerResponse, 0, len(stats.Referrers)),
		Devices:        make([]statsDeviceResponse, 0, len(deviceCategories)),
		Clients:        make([]statsClientResponse, 0, len(clientCategories)),
		DailyVisitors:  make([]statsVisitorsResponse, 0, daysBetween(stats.From, stats.To)),
		UniqueVisitors: stats.UniqueVisitors,
	}

	clicksByDay := make(map[string]int64, len(stats.Clicks))
	for _, day := range stats.Clicks {
		clicksByDay[day.Day.Format("2006-01-02")] = day.Clicks
	}
	visitorsByDay := make(map[string]int64, len(stats.DailyVisitors))
	for _, day := range stats.DailyVisitors {
		visitorsByDay[day.Day.Format("2006-01-02")] = day.Visitors
	}

	for day := stats.From; !day.After(stats.To); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		resp.Clicks = append(resp.Clicks, statsDayResponse{Day: key, Clicks: clicksByDay[key]})
		resp.DailyVisitors = append(resp.DailyVisitors, statsVisitorsResponse{Day: key, Visitors: visitorsByDay[key]})
	}

	for _, ref := range stats.Referrers {
		resp.Referrers = append(resp.Referrers, statsReferrerResponse{Referrer: ref.Value, Clicks: ref.Clicks})
	}

	deviceCounts := make(map[string]int64, len(stats.Devices))
	for _, d := range stats.Devices {
		deviceCounts[d.Value] = d.Clicks
	}
	for _, category := range deviceCategories {
		resp.Devices = append(resp.Devices, statsDeviceResponse{Device: category, Clicks: deviceCounts[category]})
	}

	clientCounts := make(map[string]int64, len(stats.Clients))
	for _, c := range stats.Clients {
		clientCounts[c.Value] = c.Clicks
	}
	for _, category := range clientCategories {
		resp.Clients = append(resp.Clients, statsClientResponse{Client: category, Clicks: clientCounts[category]})
	}

	return resp
}

// daysBetween 返回闭区间 [from, to] 的天数，用来预分配切片。
func daysBetween(from, to time.Time) int {
	if to.Before(from) {
		return 0
	}
	return int(to.Sub(from)/(24*time.Hour)) + 1
}
