// Package storemetrics 是 LinkStore 的指标与追踪装饰器，按操作记录耗时、结果和 store.<op> Span。
package storemetrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shortlink/internal/link"
)

const (
	storeTracerName = "shortlink/storemetrics"

	operationsMetricName = "shortlink_store_operations_total"
	durationMetricName   = "shortlink_store_operation_duration_seconds"

	operationCreate    = "create"
	operationGet       = "get"
	operationList      = "list"
	operationGetTenant = "get_tenant"
	operationUpdate    = "update"
	operationDelete    = "delete"
	operationStats     = "stats"
	operationBan       = "ban"
	operationBanIfURL  = "ban_if_url"
	operationUnban     = "unban"

	resultSuccess       = "success"
	resultNotFound      = "not_found"
	resultQuotaExceeded = "quota_exceeded"
	resultBanned        = "banned"
	resultURLChanged    = "url_changed"
	resultCanceled      = "canceled"
	resultTimeout       = "timeout"
	resultError         = "error"
)

/*
和 HTTP 指标用同一组桶：本地查询常在几毫秒，默认的 5ms 起步太粗。
从 0.1ms 开始按 2 倍增长，覆盖到约 13s。
*/
var durationBuckets = prometheus.ExponentialBuckets(0.0001, 2, 18)

/*
LinkReader 是公开跳转进程所需的只读存储能力，避免只读装饰器强绑定管理、统计与封禁方法（ADR 0024）。
*/
type LinkReader interface {
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

/*
LinkStore 是装饰器需要透传的完整存储能力，按基础、管理、统计和平台下架分组。
接口由使用方定义，这样 storemetrics 不依赖 redirector 和 console，也不依赖具体的 postgres 实现。
*/
type LinkStore interface {
	LinkBasics
	LinkAdmin
	LinkStats
	LinkTakedown
}

// LinkBasics 是创建和跳转查询。
type LinkBasics interface {
	LinkReader
	Create(ctx context.Context, l *link.Link) error
}

// LinkAdmin 是管理接口的列表、详情、修改和删除。
type LinkAdmin interface {
	List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error)
	GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error)
	Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error)
	Delete(ctx context.Context, tenantID int64, code string) error
}

// LinkStats 是点击统计查询。
type LinkStats interface {
	Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error)
}

// LinkTakedown 是平台封禁和解封。
type LinkTakedown interface {
	Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	Unban(ctx context.Context, code string) (*link.Link, error)
}

/*
Reader 是只读跳转路径的指标装饰器。Store 嵌入 *Reader 复用指标注册与 GetByCode 实现，消除双份代码（ADR 0024）。
*/
type Reader struct {
	next       LinkReader
	operations *prometheus.CounterVec
	duration   *prometheus.HistogramVec
}

// NewReader 包装只读存储 next，并把指标注册到 registry。
func NewReader(next LinkReader, registry *prometheus.Registry) *Reader {
	r := &Reader{
		next: next,
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: operationsMetricName,
			Help: "Total number of storage operations by operation and result.",
		}, []string{"operation", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    durationMetricName,
			Help:    "Storage operation duration in seconds by operation and result.",
			Buckets: durationBuckets,
		}, []string{"operation", "result"}),
	}
	registry.MustRegister(r.operations, r.duration)
	return r
}

// GetByCode 记录一次查询操作的耗时和结果，返回值原样返回。
func (r *Reader) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	ctx, span := startSpan(ctx, operationGet)
	start := time.Now()
	l, err := r.next.GetByCode(ctx, code)
	r.finish(span, operationGet, err, time.Since(start))
	return l, err
}

func startSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return otel.Tracer(storeTracerName).Start(ctx, "store."+operation,
		trace.WithAttributes(attribute.String("store.operation", operation)),
	)
}

func (r *Reader) finish(span trace.Span, operation string, err error, d time.Duration) {
	result := classifyResult(err)
	r.operations.WithLabelValues(operation, result).Inc()
	r.duration.WithLabelValues(operation, result).Observe(d.Seconds())
	span.SetAttributes(attribute.String("store.result", result))
	if result == resultError || result == resultTimeout {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

/*
Store 包在真正的存储外面，按操作记录耗时和结果。
找不到、客户端断开、请求超时都是预期内的结束，不算存储故障。
耗时直方图带上同样的结果标签，慢超时才不会抬高成功查询的 p99。
*/
type Store struct {
	*Reader
	next LinkStore
}

// New 包装 next，并把指标注册到 registry。registry 由 main 创建，测试每次传新的。
func New(next LinkStore, registry *prometheus.Registry) *Store {
	return &Store{
		Reader: NewReader(next, registry),
		next:   next,
	}
}

// Create 记录一次创建操作的耗时和结果，错误原样返回。
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	ctx, span := startSpan(ctx, operationCreate)
	start := time.Now()
	err := s.next.Create(ctx, l)
	s.finish(span, operationCreate, err, time.Since(start))
	return err
}

// List、GetForTenant、Update、Delete、Stats 同理，只观察，不改写结果。
func (s *Store) List(ctx context.Context, opts link.ListOptions) ([]link.AdminLink, error) {
	ctx, span := startSpan(ctx, operationList)
	start := time.Now()
	got, err := s.next.List(ctx, opts)
	s.finish(span, operationList, err, time.Since(start))
	return got, err
}

func (s *Store) GetForTenant(ctx context.Context, tenantID int64, code string) (*link.AdminLink, error) {
	ctx, span := startSpan(ctx, operationGetTenant)
	start := time.Now()
	got, err := s.next.GetForTenant(ctx, tenantID, code)
	s.finish(span, operationGetTenant, err, time.Since(start))
	return got, err
}

func (s *Store) Update(ctx context.Context, tenantID int64, code string, u link.Update) (*link.AdminLink, error) {
	ctx, span := startSpan(ctx, operationUpdate)
	start := time.Now()
	got, err := s.next.Update(ctx, tenantID, code, u)
	s.finish(span, operationUpdate, err, time.Since(start))
	return got, err
}

func (s *Store) Delete(ctx context.Context, tenantID int64, code string) error {
	ctx, span := startSpan(ctx, operationDelete)
	start := time.Now()
	err := s.next.Delete(ctx, tenantID, code)
	s.finish(span, operationDelete, err, time.Since(start))
	return err
}

func (s *Store) Stats(ctx context.Context, tenantID int64, code string, from, to time.Time) (*link.Stats, error) {
	ctx, span := startSpan(ctx, operationStats)
	start := time.Now()
	got, err := s.next.Stats(ctx, tenantID, code, from, to)
	s.finish(span, operationStats, err, time.Since(start))
	return got, err
}

// Ban、BanIfURL、Unban 记录平台下架操作的结果；ErrNotFound、ErrBanned 和 ErrURLChanged 沿用 classifyResult。
func (s *Store) Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	ctx, span := startSpan(ctx, operationBan)
	start := time.Now()
	got, err := s.next.Ban(ctx, code, reason, source)
	s.finish(span, operationBan, err, time.Since(start))
	return got, err
}

func (s *Store) BanIfURL(ctx context.Context, code, expectedURL string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	ctx, span := startSpan(ctx, operationBanIfURL)
	start := time.Now()
	got, err := s.next.BanIfURL(ctx, code, expectedURL, reason, source)
	s.finish(span, operationBanIfURL, err, time.Since(start))
	return got, err
}

func (s *Store) Unban(ctx context.Context, code string) (*link.Link, error) {
	ctx, span := startSpan(ctx, operationUnban)
	start := time.Now()
	got, err := s.next.Unban(ctx, code)
	s.finish(span, operationUnban, err, time.Since(start))
	return got, err
}

/*
classifyResult 把存储错误分成指标结果。
context 错误、ErrNotFound、配额不足、封禁和 URL 变更都是 non-nil，要排在通用错误前面。
配额不足、封禁和 URL 变更都是策略生效，不是存储故障，单独分类才能和真正的失败区分开。
*/
func classifyResult(err error) string {
	switch {
	case err == nil:
		return resultSuccess
	case errors.Is(err, link.ErrNotFound):
		return resultNotFound
	case errors.Is(err, link.ErrQuotaExceeded):
		return resultQuotaExceeded
	case errors.Is(err, link.ErrBanned):
		return resultBanned
	case errors.Is(err, link.ErrURLChanged):
		return resultURLChanged
	case errors.Is(err, context.Canceled):
		return resultCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return resultTimeout
	default:
		return resultError
	}
}
