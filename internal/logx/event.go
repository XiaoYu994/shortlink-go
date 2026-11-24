package logx

import (
	"context"
	"sync"
)

type requestEventContextKey struct{}

// RequestEventSnapshot 是请求结束时对 RequestEvent 的只读快照，供访问日志统一写出。
type RequestEventSnapshot struct {
	TenantID    int64
	ActorKind   string
	ActorID     int64
	Cache       string
	ErrorMsg    string
	RetryAfter  int
	RateLimited bool
}

/*
RequestEvent 是挂在请求 context 上的轻量可变收集槽（对应 Java MDC 的请求摘要能力）。
外层观察中间件在入口挂载一次，内层鉴权、限流、缓存和错误写出顺手填入业务维度，
请求结束时由外层汇总成唯一一条 Canonical Log Line。
*/
type RequestEvent struct {
	mu          sync.Mutex
	tenantID    int64
	actorKind   string
	actorID     int64
	cache       string
	errorMsg    string
	retryAfter  int
	rateLimited bool
}

// WithRequestEvent 在 ctx 上挂载一个新的 RequestEvent，并返回派生 context 与事件指针。
func WithRequestEvent(ctx context.Context) (context.Context, *RequestEvent) {
	if ctx == nil {
		ctx = context.Background()
	}
	ev := &RequestEvent{}
	return context.WithValue(ctx, requestEventContextKey{}, ev), ev
}

// RequestEventFrom 取出 ctx 中的 RequestEvent；未挂载时返回 nil。
func RequestEventFrom(ctx context.Context) *RequestEvent {
	if ctx == nil {
		return nil
	}
	ev, _ := ctx.Value(requestEventContextKey{}).(*RequestEvent)
	return ev
}

// Snapshot 线程安全地复制当前收集到的请求字段。
func (e *RequestEvent) Snapshot() RequestEventSnapshot {
	if e == nil {
		return RequestEventSnapshot{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return RequestEventSnapshot{
		TenantID:    e.tenantID,
		ActorKind:   e.actorKind,
		ActorID:     e.actorID,
		Cache:       e.cache,
		ErrorMsg:    e.errorMsg,
		RetryAfter:  e.retryAfter,
		RateLimited: e.rateLimited,
	}
}

// SetActor 记录鉴权通过的租户与执行者身份；ctx 未挂载 RequestEvent 时空操作。
func SetActor(ctx context.Context, tenantID int64, actorKind string, actorID int64) {
	ev := RequestEventFrom(ctx)
	if ev == nil {
		return
	}
	ev.mu.Lock()
	if tenantID > 0 {
		ev.tenantID = tenantID
	}
	if actorKind != "" {
		ev.actorKind = actorKind
	}
	if actorID > 0 {
		ev.actorID = actorID
	}
	ev.mu.Unlock()
}

// SetCacheResult 记录跳转读路径的缓存结论（hit / neg_hit / miss / fallback）。
func SetCacheResult(ctx context.Context, result string) {
	ev := RequestEventFrom(ctx)
	if ev == nil || result == "" {
		return
	}
	ev.mu.Lock()
	ev.cache = result
	ev.mu.Unlock()
}

// SetError 记录 HTTP 错误响应的摘要信息；ctx 未挂载 RequestEvent 时空操作。
func SetError(ctx context.Context, msg string) {
	ev := RequestEventFrom(ctx)
	if ev == nil || msg == "" {
		return
	}
	ev.mu.Lock()
	ev.errorMsg = msg
	ev.mu.Unlock()
}

// SetRateLimited 记录限流拒绝与 Retry-After 秒数；ctx 未挂载 RequestEvent 时空操作。
func SetRateLimited(ctx context.Context, retryAfterSeconds int) {
	ev := RequestEventFrom(ctx)
	if ev == nil {
		return
	}
	ev.mu.Lock()
	ev.rateLimited = true
	ev.retryAfter = retryAfterSeconds
	ev.mu.Unlock()
}
