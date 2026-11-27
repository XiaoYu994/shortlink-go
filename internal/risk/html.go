package risk

import (
	"errors"
	"io"
	"mime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	// DefaultMaxExtractedTextBytes 限制流式提取出的网页可见正文上限，控制规则匹配与 LLM token 成本。
	DefaultMaxExtractedTextBytes = 4096

	maxTitleBytes = 256
	maxDescBytes  = 512
)

// PageContent 保存从目标页面抽取并净化的最小文本上下文，不包含原始 HTML 标签与脚本噪音。
type PageContent struct {
	FinalURL    string
	ContentType string
	Title       string
	Description string
	Text        string
	SkippedBody bool
}

// CombinedText 拼接页面标题、摘要描述和正文可见文本，供本地关键词规则统一匹配。
func (p PageContent) CombinedText() string {
	parts := make([]string, 0, 3)
	if p.Title != "" {
		parts = append(parts, p.Title)
	}
	if p.Description != "" {
		parts = append(parts, p.Description)
	}
	if p.Text != "" {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\n")
}

/*
ExtractPageContent 根据 Content-Type 跳过二进制响应，或从 HTML/纯文本响应流中抽取净文本。
HTML 路径在 maxRawBytes 读取上限内使用流式 Tokenizer 剥离 script/style/noscript/template/iframe/svg，
越过现代网页常见的大块内联 <head> 样式与脚本，分别提取 <title>、<meta description> 与正文可见词。
*/
func ExtractPageContent(contentType string, r io.Reader, maxRawBytes, maxTextBytes int64) (PageContent, error) {
	if maxRawBytes <= 0 {
		maxRawBytes = DefaultMaxBodyBytes
	}
	if maxTextBytes <= 0 {
		maxTextBytes = DefaultMaxExtractedTextBytes
	}

	page := PageContent{ContentType: strings.TrimSpace(contentType)}
	isText, isHTML := classifyContentType(page.ContentType)
	if !isText {
		page.SkippedBody = true
		return page, nil
	}

	if !isHTML {
		raw, err := io.ReadAll(io.LimitReader(r, maxRawBytes))
		if err != nil {
			return PageContent{}, err
		}
		var b strings.Builder
		appendNormalizedText(&b, string(raw), int(maxTextBytes))
		page.Text = b.String()
		return page, nil
	}

	z := html.NewTokenizer(io.LimitReader(r, maxRawBytes))
	var (
		skipDepth int
		inTitle   bool
		titleBuf  strings.Builder
		descBuf   strings.Builder
		textBuf   strings.Builder
	)

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if err := z.Err(); err != nil && !errors.Is(err, io.EOF) {
				return PageContent{}, err
			}
			page.Title = titleBuf.String()
			page.Description = descBuf.String()
			page.Text = textBuf.String()
			return page, nil

		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if tok.DataAtom == atom.Meta {
				if descBuf.Len() == 0 {
					if desc := extractMetaDescription(tok.Attr); desc != "" {
						appendNormalizedText(&descBuf, desc, maxDescBytes)
					}
				}
				continue
			}
			if tt == html.StartTagToken {
				if isSkippedAtom(tok.DataAtom) {
					skipDepth++
					continue
				}
				if tok.DataAtom == atom.Title && skipDepth == 0 {
					inTitle = true
				}
			}

		case html.EndTagToken:
			tok := z.Token()
			if isSkippedAtom(tok.DataAtom) && skipDepth > 0 {
				skipDepth--
				continue
			}
			if tok.DataAtom == atom.Title {
				inTitle = false
			}

		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			data := z.Token().Data
			if inTitle {
				appendNormalizedText(&titleBuf, data, maxTitleBytes)
				continue
			}
			appendNormalizedText(&textBuf, data, int(maxTextBytes))
			if textBuf.Len() >= int(maxTextBytes) {
				page.Title = titleBuf.String()
				page.Description = descBuf.String()
				page.Text = textBuf.String()
				return page, nil
			}
		}
	}
}

func classifyContentType(contentType string) (isText, isHTML bool) {
	if contentType == "" {
		return true, true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		base, _, _ := strings.Cut(contentType, ";")
		mt = strings.ToLower(strings.TrimSpace(base))
	} else {
		mt = strings.ToLower(mt)
	}

	switch {
	case mt == "text/html", mt == "application/xhtml+xml":
		return true, true
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json",
		mt == "application/xml",
		strings.HasSuffix(mt, "+xml"),
		strings.HasSuffix(mt, "+json"):
		return true, false
	default:
		return false, false
	}
}

func isSkippedAtom(a atom.Atom) bool {
	return slices.Contains([]atom.Atom{
		atom.Script,
		atom.Style,
		atom.Noscript,
		atom.Template,
		atom.Iframe,
		atom.Svg,
	}, a)
}

func extractMetaDescription(attrs []html.Attribute) string {
	var isDesc bool
	var content string
	for _, attr := range attrs {
		key := strings.ToLower(strings.TrimSpace(attr.Key))
		val := strings.TrimSpace(attr.Val)
		switch key {
		case "name":
			if strings.EqualFold(val, "description") {
				isDesc = true
			}
		case "property":
			if strings.EqualFold(val, "og:description") {
				isDesc = true
			}
		case "content":
			content = val
		}
	}
	if !isDesc {
		return ""
	}
	return content
}

func appendNormalizedText(dst *strings.Builder, raw string, maxBytes int) {
	if maxBytes <= 0 || dst.Len() >= maxBytes {
		return
	}
	clean := strings.ToValidUTF8(raw, "")
	for field := range strings.FieldsFuncSeq(clean, unicode.IsSpace) {
		remaining := maxBytes - dst.Len()
		if remaining <= 0 {
			return
		}
		if dst.Len() > 0 {
			if remaining <= 1 {
				return
			}
			dst.WriteByte(' ')
			remaining--
		}
		chunk := truncateUTF8(field, remaining)
		if chunk == "" {
			return
		}
		dst.WriteString(chunk)
	}
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for i, r := range s {
		if i+utf8.RuneLen(r) > maxBytes {
			return s[:i]
		}
	}
	return s
}
