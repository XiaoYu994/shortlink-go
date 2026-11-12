// Package storemetrics 是 LinkStore 的指标装饰器，按操作记录耗时和结果。
package storemetrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/link"
)

const (
	operationsMetricName = "shortlink_store_operations_total"
	durationMetricName   = "shortlink_store_operation_duration_seconds"

	operationCreate = "create"
	operationGet    = "get"

	resultSuccess  = "success"
	resultNotFound = "not_found"
	resultCanceled = "canceled"
	resultTimeout  = "timeout"
	resultError    = "error"
)

/*
和 HTTP 指标用同一组桶：本地查询常在几毫秒，默认的 5ms 起步太粗。
从 0.1ms 开始按 2 倍增长，覆盖到约 13s。
*/
var durationBuckets = prometheus.ExponentialBuckets(0.0001, 2, 18)

/*
LinkStore 是装饰器需要的最小存储能力。接口由使用方定义，
这样 storemetrics 不依赖 httpapi，也不依赖具体的 postgres 实现。
*/
type LinkStore interface {
	Create(ctx context.Context, l *link.Link) error
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

/*
Store 包在真正的存储外面，按操作记录耗时和结果。
找不到、客户端断开、请求超时都是预期内的结束，不算存储故障。
耗时直方图带上同样的结果标签，慢超时才不会抬高成功查询的 p99。
*/
type Store struct {
	next       LinkStore
	operations *prometheus.CounterVec
	duration   *prometheus.HistogramVec
}

// New 包装 next，并把指标注册到 registry。registry 由 main 创建，测试每次传新的。
func New(next LinkStore, registry *prometheus.Registry) *Store {
	s := &Store{
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
	registry.MustRegister(s.operations, s.duration)
	return s
}

// Create 记录一次创建操作的耗时和结果，错误原样返回。
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	start := time.Now()
	err := s.next.Create(ctx, l)
	s.observe(operationCreate, err, time.Since(start))
	return err
}

// GetByCode 记录一次查询操作的耗时和结果，返回值原样返回。
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	start := time.Now()
	l, err := s.next.GetByCode(ctx, code)
	s.observe(operationGet, err, time.Since(start))
	return l, err
}

func (s *Store) observe(operation string, err error, d time.Duration) {
	result := classifyResult(err)
	s.operations.WithLabelValues(operation, result).Inc()
	s.duration.WithLabelValues(operation, result).Observe(d.Seconds())
}

/*
classifyResult 把存储错误分成指标结果。
context 错误和 ErrNotFound 都是 non-nil，要排在通用错误前面。
*/
func classifyResult(err error) string {
	switch {
	case err == nil:
		return resultSuccess
	case errors.Is(err, link.ErrNotFound):
		return resultNotFound
	case errors.Is(err, context.Canceled):
		return resultCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return resultTimeout
	default:
		return resultError
	}
}
