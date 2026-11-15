package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"shortlink/internal/link"
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

/*
listLinksResponse 是列表响应。没有下一页时省略 next_cursor。
游标对客户端不透明，服务端按 sort 解码成对应的字段组合。
*/
type listLinksResponse struct {
	Items      []linkResponse `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

/*
handleList 返回当前租户的链接，包含已过期链接，固定降序。

limit 默认 20、最大 100；非法 limit、sort 或游标返回 400。
不返回总数：COUNT(*) 每次都要扫租户的行，当前没有需求。
*/
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if s.clientGone(r) {
		return
	}
	tn, ok := s.principal(w, r)
	if !ok {
		return
	}

	opts, pageLimit, err := parseListOptions(r, tn.ID)
	if err != nil {
		s.writeError(r.Context(), w, http.StatusBadRequest, err.Error())
		return
	}

	// 多要一条用来判断有没有下一页，不额外查询总数。
	items, err := s.admin.List(r.Context(), opts)
	if err != nil {
		s.handleStoreError(w, r, "list links", err)
		return
	}

	resp := listLinksResponse{Items: make([]linkResponse, 0, min(len(items), pageLimit))}
	hasNext := len(items) > pageLimit
	if hasNext {
		items = items[:pageLimit]
	}
	for _, item := range items {
		resp.Items = append(resp.Items, newLinkResponse(item))
	}
	if hasNext {
		resp.NextCursor = encodeListCursor(opts.Sort, items[len(items)-1])
	}
	s.writeJSON(r.Context(), w, http.StatusOK, resp)
}

/*
parseListOptions 解析 limit、sort 和 cursor，并把 limit 加一交给存储。
返回的 pageLimit 是客户端看到的页大小，opts.Limit 是实际查询条数。
*/
func parseListOptions(r *http.Request, tenantID int64) (link.ListOptions, int, error) {
	query := r.URL.Query()

	pageLimit := defaultListLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxListLimit {
			return link.ListOptions{}, 0, badRequestError("invalid limit")
		}
		pageLimit = n
	}

	sort := link.SortID
	if raw := query.Get("sort"); raw != "" {
		switch link.Sort(raw) {
		case link.SortID, link.SortClicks, link.SortClicksToday:
			sort = link.Sort(raw)
		default:
			return link.ListOptions{}, 0, badRequestError("invalid sort")
		}
	}

	cursor, err := decodeListCursor(query.Get("cursor"), sort)
	if err != nil {
		return link.ListOptions{}, 0, err
	}

	return link.ListOptions{
		TenantID: tenantID,
		Limit:    pageLimit + 1,
		Sort:     sort,
		Cursor:   cursor,
	}, pageLimit, nil
}

/*
encodeListCursor 把上一页最后一条的排序键编成字符串。
sort=id 时就是 id；其他两种排序是 "<排序值>:<id>"。
*/
func encodeListCursor(sort link.Sort, last link.AdminLink) string {
	switch sort {
	case link.SortClicks:
		return strconv.FormatInt(last.Clicks, 10) + ":" + strconv.FormatInt(last.ID, 10)
	case link.SortClicksToday:
		return strconv.FormatInt(last.ClicksToday, 10) + ":" + strconv.FormatInt(last.ID, 10)
	default:
		return strconv.FormatInt(last.ID, 10)
	}
}

/*
decodeListCursor 按 sort 解析游标，空字符串表示第一页。
非法游标返回 400；游标格式必须和 sort 对应，不能拿 clicks 的游标去查 id 排序。
*/
func decodeListCursor(raw string, sort link.Sort) (*link.ListCursor, error) {
	if raw == "" {
		return nil, nil
	}

	switch sort {
	case link.SortID:
		id, err := parseNonNegative(raw)
		if err != nil || id <= 0 {
			return nil, badRequestError("invalid cursor")
		}
		return &link.ListCursor{ID: id}, nil

	case link.SortClicks, link.SortClicksToday:
		keyPart, idPart, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, badRequestError("invalid cursor")
		}
		key, err := parseNonNegative(keyPart)
		if err != nil {
			return nil, badRequestError("invalid cursor")
		}
		id, err := parseNonNegative(idPart)
		if err != nil || id <= 0 {
			return nil, badRequestError("invalid cursor")
		}
		if sort == link.SortClicks {
			return &link.ListCursor{ID: id, Clicks: key}, nil
		}
		return &link.ListCursor{ID: id, ClicksToday: key}, nil
	}
	return nil, badRequestError("invalid sort")
}

// parseNonNegative 只接受十进制非负整数，拒绝前导加号、空格和溢出。
func parseNonNegative(s string) (int64, error) {
	if s == "" || strings.ContainsAny(s, "+- ") {
		return 0, errors.New("not a non-negative integer")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("not a non-negative integer")
	}
	return n, nil
}
