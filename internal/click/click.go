package click

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/url"
	"strings"
	"time"

	"shortlink/internal/link"
)

/*
一次点击的元数据上限。超长头来自客户端，直接哈希或分类会放大内存和 CPU 开销。
Referer 超限直接归 other；User-Agent 截断后再参与分类和哈希。
*/
const (
	maxRefererBytes   = 2048
	maxUserAgentBytes = 512
	visitorHashBytes  = 16
)

// 来源、设备和客户端的固定取值。设备、客户端的顺序也是统计接口补零的顺序。
const (
	ReferrerDirect = "direct"
	ReferrerOther  = "other"

	DeviceDesktop = "desktop"
	DeviceMobile  = "mobile"
	DeviceTablet  = "tablet"
	DeviceBot     = "bot"
	DeviceOther   = "other"

	ClientBrowser = "browser"
	ClientBot     = "bot"
	ClientCLI     = "cli"
	ClientOther   = "other"
)

// DeviceCategories 返回统计接口固定返回的设备类别，顺序固定。
func DeviceCategories() []string {
	return []string{DeviceDesktop, DeviceMobile, DeviceTablet, DeviceBot, DeviceOther}
}

// ClientCategories 返回统计接口固定返回的客户端类别，顺序固定。
func ClientCategories() []string {
	return []string{ClientBrowser, ClientBot, ClientCLI, ClientOther}
}

/*
Click 是一次跳转产生的点击。

At 在进入 Recorder 时取，不等到批量写出；日期按 At.UTC() 计算，
写库延迟不会把点击挪到第二天。
VisitorHash 是 16 字节的按天伪匿名哈希，没有访客信息时为 nil。
*/
type Click struct {
	LinkID      int64
	At          time.Time
	Referrer    string
	Device      string
	Client      string
	VisitorHash []byte
}

/*
NewClick 从请求元数据构造一次点击。referer 和 userAgent 是原始请求头，
clientIP 是已经校验过的客户端地址；salt 为空或 clientIP 为空时不记访客。
分类只是启发式，用来看看板，不是安全边界。
*/
func NewClick(linkID int64, at time.Time, referer, userAgent, clientIP string, salt []byte) Click {
	ua := truncate(userAgent, maxUserAgentBytes)
	lower := strings.ToLower(ua)
	bot := isBot(lower)
	return Click{
		LinkID:      linkID,
		At:          at,
		Referrer:    referrerHost(referer),
		Device:      deviceOf(lower, bot),
		Client:      clientOf(lower, bot),
		VisitorHash: visitorHash(salt, at, clientIP, ua),
	}
}

/*
referrerHost 只保留 Referer 的 host：转小写并去掉一个前导 www.。
空值记 direct；非法、没有 hostname 或超过 255 字节记 other。
不存完整 URL，活动归因（UTM）不在这一课。
*/
func referrerHost(raw string) string {
	if raw == "" {
		return ReferrerDirect
	}
	if len(raw) > maxRefererBytes {
		return ReferrerOther
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ReferrerOther
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if host == "" || len(host) > 255 {
		return ReferrerOther
	}
	return host
}

/*
deviceOf 按屏幕形态分类。先判机器人：爬虫也会带桌面或移动 UA，
但它不是人在看页面，应该单独统计。lower 已经由 NewClick 转成小写，
bot 是同一个 UA 的机器人判定，避免重复扫描。
*/
func deviceOf(lower string, bot bool) string {
	switch {
	case bot:
		return DeviceBot
	case strings.Contains(lower, "ipad"),
		strings.Contains(lower, "tablet"),
		strings.Contains(lower, "kindle"),
		strings.Contains(lower, "silk/"),
		strings.Contains(lower, "android") && !strings.Contains(lower, "mobile"):
		return DeviceTablet
	case strings.Contains(lower, "mobile"),
		strings.Contains(lower, "iphone"),
		strings.Contains(lower, "ipod"),
		strings.Contains(lower, "android"),
		strings.Contains(lower, "windows phone"),
		strings.Contains(lower, "blackberry"):
		return DeviceMobile
	case strings.Contains(lower, "windows nt"),
		strings.Contains(lower, "macintosh"),
		strings.Contains(lower, "mac os x"),
		strings.Contains(lower, "x11"),
		strings.Contains(lower, "linux"),
		strings.Contains(lower, "cros"):
		return DeviceDesktop
	default:
		return DeviceOther
	}
}

/*
clientOf 回答「是不是浏览器访问」：先判机器人，再判命令行客户端，
再判常见浏览器，其余归 other。手机 App 的 WebView 常带 Safari/Chrome 的 UA，
所以这是启发式，不能用来做访问控制。
*/
func clientOf(lower string, bot bool) string {
	switch {
	case bot:
		return ClientBot
	case isCLI(lower):
		return ClientCLI
	case isBrowser(lower):
		return ClientBrowser
	default:
		return ClientOther
	}
}

// 关键词预先放在包级数组里，避免每次跳转重建切片。
var (
	botTokens = [...]string{
		"bot", "crawler", "spider", "slurp", "facebookexternalhit",
		"embedly", "pinterest", "headlesschrome", "lighthouse",
		"pingdom", "uptime", "gtmetrix",
	}
	cliTokens = [...]string{
		"curl/", "wget/", "httpie/", "go-http-client/",
		"python-requests/", "python-urllib/", "aiohttp/",
		"okhttp/", "java/", "apache-httpclient",
		"libwww-perl", "axios/", "node-fetch/",
	}
	browserTokens = [...]string{
		"edg/", "opr/", "opera", "samsungbrowser",
		"firefox/", "fxios/", "crios/", "chrome/", "chromium/",
		"safari/", "version/", "msie ", "trident/", "mozilla/",
	}
)

func isBot(lower string) bool {
	return containsAny(lower, botTokens[:])
}

func isCLI(lower string) bool {
	return containsAny(lower, cliTokens[:])
}

func isBrowser(lower string) bool {
	return containsAny(lower, browserTokens[:])
}

func containsAny(s string, tokens []string) bool {
	for _, token := range tokens {
		if strings.Contains(s, token) {
			return true
		}
	}
	return false
}

/*
visitorHash 计算 HMAC-SHA256(salt, "v1\x00" + UTC 日期 + "\x00" + IP + "\x00" + UA)
并取前 16 字节。日期参与哈希，同一访客跨天得到不同值，跨天不可关联；
每日 UV 精确，区间 UV 是每日 UV 相加。

不存原始 IP 和 User-Agent。盐是密钥，不能提交到仓库；换盐会让当天的
新旧哈希分裂，UV 可能偏高，所以安排在 UTC 零点后换。
*/
func visitorHash(salt []byte, at time.Time, clientIP, ua string) []byte {
	if len(salt) == 0 || clientIP == "" {
		return nil
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte("v1\x00"))
	mac.Write([]byte(link.DayKey(at)))
	mac.Write([]byte{0})
	mac.Write([]byte(clientIP))
	mac.Write([]byte{0})
	mac.Write([]byte(ua))
	sum := mac.Sum(nil)
	return sum[:visitorHashBytes]
}

// truncate 按字节截断，避免恶意超长头放大内存和哈希开销。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
