package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestParseWorkerArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantOK    bool
		wantUsage bool
	}{
		{
			name:   "no flags",
			args:   nil,
			wantOK: true,
		},
		{
			name:   "help flag",
			args:   []string{"-h"},
			wantOK: false,
		},
		{
			name:      "rejects extra positional args",
			args:      []string{"extra"},
			wantOK:    false,
			wantUsage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			_, ok, err := parseWorkerArgs(tt.args, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, err)
			}
			if tt.wantUsage && !errors.Is(err, errUsage) {
				t.Fatalf("err = %v, want errUsage", err)
			}
			if !tt.wantUsage && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDrainCleanupBatches(t *testing.T) {
	var calls int
	counts := []int64{5, 5, 2}
	total, err := drainCleanupBatches(context.Background(), 5, 10, func(_ context.Context, limit int) (int64, error) {
		if limit != 5 {
			t.Fatalf("limit = %d, want 5", limit)
		}
		n := counts[calls]
		calls++
		return n, nil
	})
	if err != nil {
		t.Fatalf("drainCleanupBatches error = %v", err)
	}
	if total != 12 || calls != 3 {
		t.Errorf("total=%d calls=%d, want (12, 3)", total, calls)
	}
}
