package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"

	"shortlink/internal/audit"
	"shortlink/internal/cache"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/safehttp"
)

// LinkBanner 是风控处理器需要的最小封禁能力，由 cache.Store 实现。
type LinkBanner interface {
	BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
}

// ScanPayload 是 link.risk_scan 任务的 JSON 载荷，和 postgres 入队 CTE 的字段一致。
type ScanPayload struct {
	LinkID int64  `json:"link_id"`
	Code   string `json:"code"`
	URL    string `json:"url"`
}

// HandlerOptions 组装风控处理器需要的依赖。
type HandlerOptions struct {
	Scanner Scanner
	Banner  LinkBanner
	Cache   *VerdictCache
	Limiter *safehttp.HostLimiter
	Logger  *slog.Logger
}

/*
NewHandler 返回处理 link.risk_scan 任务的 job.Handler（ADR 0020）：
1. 解析载荷，损坏时标为 Permanent 直接进入 dead；
2. 查 Redis 结果缓存（fail-open），命中时免出站抓取；
3. 按目标 host 限速，超限时返回 job.Defer 延迟重排且不扣尝试次数；
4. 调用 Scanner 扫描，命中 SSRF 拦截（ErrBlockedAddress）或非法协议直接标为 Permanent；
5. 违规时调用 BanIfURL(code, expectedURL, reason, risk)：
  - link.ErrNotFound / link.ErrURLChanged 标为 job.Skip（链接已删或已改成新 URL）；
  - cache.ErrNotUpdated 记 Warn 日志返回 nil：数据库封禁已提交，不重抓外网 URL，旧正缓存按 5 分钟 TTL 自然失效（ADR 0031）。
*/
func NewHandler(opts HandlerOptions) job.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return func(ctx context.Context, j job.Job) error {
		var p ScanPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode risk payload: %w", err))
		}
		if p.Code == "" || p.URL == "" {
			return job.Permanent(errors.New("risk payload missing code or url"))
		}

		verdict, hit := opts.Cache.Get(ctx, p.URL)
		if !hit {
			u, err := url.Parse(p.URL)
			if err != nil || u.Hostname() == "" {
				return job.Permanent(fmt.Errorf("parse target url: %w", safehttp.ErrInvalidScheme))
			}
			if delay, allowed := opts.Limiter.Allow(u.Hostname()); !allowed {
				return job.Defer(delay, "host rate limited: "+u.Hostname())
			}

			verdict, err = opts.Scanner.Scan(ctx, p.URL)
			if err != nil {
				if errors.Is(err, safehttp.ErrBlockedAddress) || errors.Is(err, safehttp.ErrBlockedPort) || errors.Is(err, safehttp.ErrInvalidScheme) || errors.Is(err, safehttp.ErrTooManyRedirects) {
					return job.Permanent(err)
				}
				return err
			}
			if !verdict.Uncacheable {
				opts.Cache.Set(ctx, p.URL, verdict)
			}
		}

		if !verdict.Unsafe {
			return nil
		}

		banCtx := audit.WithMeta(ctx, audit.SystemMeta(fmt.Sprintf("job:%d", j.ID)))
		_, err := opts.Banner.BanIfURL(banCtx, p.Code, p.URL, verdict.Reason, link.BanSourceRisk)
		if err == nil {
			logger.InfoContext(ctx, "risk scan banned link",
				"code", p.Code, "reason", verdict.Reason, "detail", verdict.Detail)
			return nil
		}
		if errors.Is(err, link.ErrNotFound) {
			return job.Skip("link deleted before ban")
		}
		if errors.Is(err, link.ErrURLChanged) {
			return job.Skip("link url changed before ban")
		}
		if errors.Is(err, cache.ErrNotUpdated) {
			logger.WarnContext(ctx, "risk ban committed; cache write failed and will expire within ttl",
				"code", p.Code, "reason", verdict.Reason, "err", err)
			return nil
		}
		return err
	}
}
