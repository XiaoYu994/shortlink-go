package storemetrics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"shortlink/internal/link"
)

// stubStore 返回预设的结果，用来验证装饰器只观察、不改写返回值。
type stubStore struct {
	link  *link.Link
	admin *link.AdminLink
	stats *link.Stats
	err   error
}

func (s stubStore) Create(context.Context, *link.Link) error { return s.err }

func (s stubStore) GetByCode(context.Context, string) (*link.Link, error) {
	return s.link, s.err
}

func (s stubStore) List(context.Context, link.ListOptions) ([]link.AdminLink, error) {
	if s.err != nil || s.admin == nil {
		return nil, s.err
	}
	return []link.AdminLink{*s.admin}, nil
}

func (s stubStore) GetForTenant(context.Context, int64, string) (*link.AdminLink, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.admin, nil
}

func (s stubStore) Update(context.Context, int64, string, link.Update) (*link.AdminLink, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.admin, nil
}

func (s stubStore) Delete(context.Context, int64, string) error { return s.err }

func (s stubStore) Stats(context.Context, int64, string, time.Time, time.Time) (*link.Stats, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.stats, nil
}

func (s stubStore) Ban(context.Context, string, link.BanReason, link.BanSource) (*link.Link, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.link, nil
}

func (s stubStore) Unban(context.Context, string) (*link.Link, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.link, nil
}

func TestCreateRecordsResult(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantResult string
	}{
		{"success", nil, resultSuccess},
		{"quota exceeded", link.ErrQuotaExceeded, resultQuotaExceeded},
		{"wrapped quota exceeded", fmt.Errorf("insert link: %w", link.ErrQuotaExceeded), resultQuotaExceeded},
		{"error", errors.New("db down"), resultError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			s := New(stubStore{err: tt.err}, reg)

			if err := s.Create(context.Background(), &link.Link{}); !errors.Is(err, tt.err) {
				t.Errorf("Create error = %v, want %v", err, tt.err)
			}

			if got := testutil.ToFloat64(s.operations.WithLabelValues(operationCreate, tt.wantResult)); got != 1 {
				t.Errorf("operations{operation=%q, result=%q} = %v, want 1", operationCreate, tt.wantResult, got)
			}
			other := resultSuccess
			if tt.wantResult == resultSuccess {
				other = resultError
			}
			if got := testutil.ToFloat64(s.operations.WithLabelValues(operationCreate, other)); got != 0 {
				t.Errorf("operations{operation=%q, result=%q} = %v, want 0", operationCreate, other, got)
			}
			assertDurationResult(t, reg, operationCreate, tt.wantResult)
		})
	}
}

func TestGetRecordsResult(t *testing.T) {
	wantLink := &link.Link{Code: "abc", URL: "https://example.com"}
	tests := []struct {
		name       string
		err        error
		wantResult string
		wantLink   *link.Link
	}{
		{"success", nil, resultSuccess, wantLink},
		{"not found", link.ErrNotFound, resultNotFound, nil},
		{"wrapped not found", fmt.Errorf("get abc: %w", link.ErrNotFound), resultNotFound, nil},
		{"canceled", context.Canceled, resultCanceled, nil},
		{"wrapped canceled", fmt.Errorf("get link: %w", context.Canceled), resultCanceled, nil},
		{"timeout", context.DeadlineExceeded, resultTimeout, nil},
		{"wrapped timeout", fmt.Errorf("get link: %w", context.DeadlineExceeded), resultTimeout, nil},
		{"error", errors.New("db down"), resultError, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			s := New(stubStore{link: tt.wantLink, err: tt.err}, reg)

			gotLink, err := s.GetByCode(context.Background(), "abc")
			if !errors.Is(err, tt.err) {
				t.Errorf("GetByCode error = %v, want %v", err, tt.err)
			}
			if gotLink != tt.wantLink {
				t.Errorf("GetByCode link = %v, want %v", gotLink, tt.wantLink)
			}

			if got := testutil.ToFloat64(s.operations.WithLabelValues(operationGet, tt.wantResult)); got != 1 {
				t.Errorf("operations{operation=%q, result=%q} = %v, want 1", operationGet, tt.wantResult, got)
			}
			if tt.wantResult != resultError {
				if got := testutil.ToFloat64(s.operations.WithLabelValues(operationGet, resultError)); got != 0 {
					t.Errorf("operations{operation=%q, result=%q} = %v, want 0", operationGet, resultError, got)
				}
			}
			assertDurationResult(t, reg, operationGet, tt.wantResult)
		})
	}
}

/*
TestAdminOperationsRecordResult 锁住列表、详情、修改、删除、统计和平台下架的 operation 标签。
这些方法共用 classifyResult；以前只有 Create 和 GetByCode 被断言过。
*/
func TestAdminOperationsRecordResult(t *testing.T) {
	tracedLink := &link.Link{ID: 7, Code: "abc", URL: "https://example.com"}
	admin := &link.AdminLink{Link: *tracedLink}
	stats := &link.Stats{Code: "abc", TotalClicks: 4}
	results := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, resultSuccess},
		{"not found", link.ErrNotFound, resultNotFound},
		{"banned", link.ErrBanned, resultBanned},
		{"wrapped banned", fmt.Errorf("update link: %w", link.ErrBanned), resultBanned},
		{"canceled", context.Canceled, resultCanceled},
		{"timeout", context.DeadlineExceeded, resultTimeout},
		{"error", errors.New("db down"), resultError},
	}
	ops := []struct {
		operation string
		call      func(t *testing.T, s *Store) error
	}{
		{operationList, func(t *testing.T, s *Store) error {
			got, err := s.List(context.Background(), link.ListOptions{Limit: 1})
			if err == nil && (len(got) != 1 || got[0].Code != admin.Code) {
				t.Errorf("List = %+v, want %s", got, admin.Code)
			}
			return err
		}},
		{operationGetTenant, func(t *testing.T, s *Store) error {
			got, err := s.GetForTenant(context.Background(), 1, "abc")
			if err == nil && (got == nil || got.Code != admin.Code) {
				t.Errorf("GetForTenant = %+v, want %s", got, admin.Code)
			}
			return err
		}},
		{operationUpdate, func(t *testing.T, s *Store) error {
			got, err := s.Update(context.Background(), 1, "abc", link.Update{})
			if err == nil && (got == nil || got.Code != admin.Code) {
				t.Errorf("Update = %+v, want %s", got, admin.Code)
			}
			return err
		}},
		{operationDelete, func(_ *testing.T, s *Store) error {
			return s.Delete(context.Background(), 1, "abc")
		}},
		{operationStats, func(t *testing.T, s *Store) error {
			got, err := s.Stats(context.Background(), 1, "abc", time.Time{}, time.Time{})
			if err == nil && (got == nil || got.TotalClicks != stats.TotalClicks) {
				t.Errorf("Stats = %+v, want total %d", got, stats.TotalClicks)
			}
			return err
		}},
		{operationBan, func(t *testing.T, s *Store) error {
			got, err := s.Ban(context.Background(), "abc", link.BanReasonPhishing, link.BanSourceOperator)
			if err == nil && (got == nil || got.Code != tracedLink.Code) {
				t.Errorf("Ban = %+v, want %s", got, tracedLink.Code)
			}
			return err
		}},
		{operationUnban, func(t *testing.T, s *Store) error {
			got, err := s.Unban(context.Background(), "abc")
			if err == nil && (got == nil || got.Code != tracedLink.Code) {
				t.Errorf("Unban = %+v, want %s", got, tracedLink.Code)
			}
			return err
		}},
	}
	for _, op := range ops {
		for _, tt := range results {
			t.Run(op.operation+" "+tt.name, func(t *testing.T) {
				reg := prometheus.NewRegistry()
				s := New(stubStore{link: tracedLink, admin: admin, stats: stats, err: tt.err}, reg)
				if err := op.call(t, s); !errors.Is(err, tt.err) {
					t.Fatalf("%s error = %v, want %v", op.operation, err, tt.err)
				}
				if got := testutil.ToFloat64(s.operations.WithLabelValues(op.operation, tt.want)); got != 1 {
					t.Errorf("operations{operation=%q, result=%q} = %v, want 1", op.operation, tt.want, got)
				}
				if tt.want != resultSuccess {
					if got := testutil.ToFloat64(s.operations.WithLabelValues(op.operation, resultSuccess)); got != 0 {
						t.Errorf("operations{operation=%q, result=%q} = %v, want 0", op.operation, resultSuccess, got)
					}
				}
				assertDurationResult(t, reg, op.operation, tt.want)
			})
		}
	}
}

// assertDurationResult 确认耗时直方图和计数器用了同一个 result，并且只有这一条序列。
func assertDurationResult(t *testing.T, reg *prometheus.Registry, operation, result string) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != durationMetricName {
			continue
		}
		if len(fam.GetMetric()) != 1 {
			t.Fatalf("duration series = %d, want 1", len(fam.GetMetric()))
		}
		got := map[string]string{}
		for _, lp := range fam.GetMetric()[0].GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		if got["operation"] != operation || got["result"] != result {
			t.Errorf("duration labels = %v, want operation=%s result=%s", got, operation, result)
		}
		return
	}
	t.Fatalf("metric %s not found", durationMetricName)
}
