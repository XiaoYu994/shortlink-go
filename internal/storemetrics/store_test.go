package storemetrics

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"shortlink/internal/link"
)

// stubStore 返回预设的结果，用来验证装饰器只观察、不改写返回值。
type stubStore struct {
	link *link.Link
	err  error
}

func (s stubStore) Create(context.Context, *link.Link) error { return s.err }

func (s stubStore) GetByCode(context.Context, string) (*link.Link, error) {
	return s.link, s.err
}

func TestCreateRecordsResult(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantResult string
	}{
		{"success", nil, resultSuccess},
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
