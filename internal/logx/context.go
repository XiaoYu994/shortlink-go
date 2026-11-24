package logx

import (
	"context"
	"io"
	"log/slog"
	"slices"
)

// fieldsContextKey 用空结构体做 context key，避免与其他包产生键冲突。
type fieldsContextKey struct{}

/*
Fields 保存要自动注入到每一条带 context 日志的关联字段。
各层通过 WithRequestID、WithTenantID、WithJob 增量追加，由根 logger 的 contextHandler 统一提取。
*/
type Fields struct {
	RequestID       string
	ClientRequestID string
	TenantID        int64
	JobID           int64
	JobKind         string
}

// FieldsFrom 返回 ctx 中挂载的日志关联字段；ctx 为 nil 或未设置时返回零值。
func FieldsFrom(ctx context.Context) Fields {
	if ctx == nil {
		return Fields{}
	}
	f, _ := ctx.Value(fieldsContextKey{}).(Fields)
	return f
}

// WithRequestID 将请求编号写入 ctx，保留已有的其他日志字段。
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	f := FieldsFrom(ctx)
	f.RequestID = id
	return context.WithValue(ctx, fieldsContextKey{}, f)
}

// RequestIDFrom 从 ctx 读取请求编号；不存在时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	return FieldsFrom(ctx).RequestID
}

// WithClientRequestID 将调用方传入且通过字符校验的 X-Request-ID 记录到 client_request_id（不覆盖服务端签发编号）。
func WithClientRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	f := FieldsFrom(ctx)
	f.ClientRequestID = id
	return context.WithValue(ctx, fieldsContextKey{}, f)
}

// ClientRequestIDFrom 从 ctx 读取调用方传入的 client_request_id；未传入或非法时返回空字符串。
func ClientRequestIDFrom(ctx context.Context) string {
	return FieldsFrom(ctx).ClientRequestID
}

// WithTenantID 将租户编号写入 ctx，保留已有的其他日志字段。
func WithTenantID(ctx context.Context, tenantID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	f := FieldsFrom(ctx)
	f.TenantID = tenantID
	return context.WithValue(ctx, fieldsContextKey{}, f)
}

// TenantIDFrom 从 ctx 读取租户编号；未设置时返回 0。
func TenantIDFrom(ctx context.Context) int64 {
	return FieldsFrom(ctx).TenantID
}

// WithJob 将后台任务 ID、类型和所属租户写入 ctx，保留已有的其他日志字段。
func WithJob(ctx context.Context, jobID int64, kind string, tenantID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	f := FieldsFrom(ctx)
	f.JobID = jobID
	f.JobKind = kind
	if tenantID > 0 {
		f.TenantID = tenantID
	}
	return context.WithValue(ctx, fieldsContextKey{}, f)
}

/*
WrapContext 为 slog.Handler 套上统一的 context 字段提取层。
幂等设计：若 h 已经是 contextHandler 则原样返回，避免根 logger 与 httpx/job 重复包裹导致 JSON 字段重复输出。
*/
func WrapContext(h slog.Handler) slog.Handler {
	if h == nil {
		h = slog.NewTextHandler(io.Discard, nil)
	}
	if ch, ok := h.(contextHandler); ok {
		return ch
	}
	return contextHandler{
		root: h,
		fast: h,
	}
}

type handlerOp struct {
	attrs []slog.Attr
	group string
}

/*
contextHandler 在 Handle 阶段从 ctx 提取 request_id、tenant_id、job_id、job_kind 写入记录。
阶段 16 引入 OpenTelemetry 后，只需在这里追加 trace_id 与 span_id 提取，业务代码零侵入。

当调用方使用 WithGroup 时，通过 root + ops 回放保证关联 ID 始终落在顶层，不会下沉到子组内；
未开组时直接走 fast 路径，零额外 handler 构造开销。
*/
type contextHandler struct {
	root     slog.Handler
	fast     slog.Handler
	ops      []handlerOp
	hasGroup bool
}

func (h contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.fast.Enabled(ctx, level)
}

func (h contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	var buf [5]slog.Attr
	ctxAttrs := appendContextAttrs(buf[:0], FieldsFrom(ctx))
	if len(ctxAttrs) == 0 {
		return h.fast.Handle(ctx, rec)
	}
	if !h.hasGroup {
		rec = rec.Clone()
		rec.AddAttrs(ctxAttrs...)
		return h.fast.Handle(ctx, rec)
	}

	target := h.root.WithAttrs(ctxAttrs)
	for _, op := range h.ops {
		if op.group != "" {
			target = target.WithGroup(op.group)
		} else {
			target = target.WithAttrs(op.attrs)
		}
	}
	return target.Handle(ctx, rec)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	ops := make([]handlerOp, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = handlerOp{attrs: slices.Clone(attrs)}
	return contextHandler{
		root:     h.root,
		fast:     h.fast.WithAttrs(attrs),
		ops:      ops,
		hasGroup: h.hasGroup,
	}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	ops := make([]handlerOp, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = handlerOp{group: name}
	return contextHandler{
		root:     h.root,
		fast:     h.fast.WithGroup(name),
		ops:      ops,
		hasGroup: true,
	}
}

func appendContextAttrs(dst []slog.Attr, f Fields) []slog.Attr {
	if f.RequestID != "" {
		dst = append(dst, slog.String("request_id", f.RequestID))
	}
	if f.ClientRequestID != "" {
		dst = append(dst, slog.String("client_request_id", f.ClientRequestID))
	}
	if f.TenantID > 0 {
		dst = append(dst, slog.Int64("tenant_id", f.TenantID))
	}
	if f.JobID > 0 {
		dst = append(dst, slog.Int64("job_id", f.JobID))
	}
	if f.JobKind != "" {
		dst = append(dst, slog.String("job_kind", f.JobKind))
	}
	return dst
}
