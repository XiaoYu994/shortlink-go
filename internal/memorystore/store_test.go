package memorystore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"shortlink/internal/idgen"
	"shortlink/internal/link"
)

func TestStoreCreateAndGetByCode(t *testing.T) {
	store := New()
	ctx := context.Background()

	l, err := link.NewLink("https://example.com", nil)
	if err != nil {
		t.Fatalf("NewLink returned error: %v", err)
	}
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if l.ID != 1 {
		t.Errorf("ID = %d, want 1", l.ID)
	}
	if len(l.Code) != idgen.CodeLength {
		t.Errorf("Code length = %d, want %d", len(l.Code), idgen.CodeLength)
	}
	if l.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode returned error: %v", err)
	}
	if got.URL != l.URL {
		t.Errorf("URL = %q, want %q", got.URL, l.URL)
	}
}

func TestStoreGetByCodeNotFound(t *testing.T) {
	store := New()

	_, err := store.GetByCode(context.Background(), "missing")
	if !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode error = %v, want link.ErrNotFound", err)
	}
}

func TestStoreGetByCodeReturnsCopy(t *testing.T) {
	store := New()
	ctx := context.Background()

	l, err := link.NewLink("https://example.com", nil)
	if err != nil {
		t.Fatalf("NewLink returned error: %v", err)
	}
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode returned error: %v", err)
	}
	got.URL = "https://changed.example"

	again, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("second GetByCode returned error: %v", err)
	}
	if again.URL != "https://example.com" {
		t.Errorf("stored URL changed to %q", again.URL)
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	store := New()
	ctx := context.Background()
	const n = 100

	var wg sync.WaitGroup
	codes := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			l, err := link.NewLink("https://example.com", nil)
			if err != nil {
				errs[i] = err
				return
			}
			if err := store.Create(ctx, l); err != nil {
				errs[i] = err
				return
			}
			codes[i] = l.Code
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	seen := make(map[string]bool, n)
	for _, code := range codes {
		if seen[code] {
			t.Errorf("duplicate code %q", code)
		}
		seen[code] = true
		if _, err := store.GetByCode(ctx, code); err != nil {
			t.Errorf("GetByCode(%q) returned error: %v", code, err)
		}
	}
}

func TestStoreContextCanceled(t *testing.T) {
	store := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l, err := link.NewLink("https://example.com", nil)
	if err != nil {
		t.Fatalf("NewLink returned error: %v", err)
	}
	if err := store.Create(ctx, l); !errors.Is(err, context.Canceled) {
		t.Errorf("Create error = %v, want context.Canceled", err)
	}
	if _, err := store.GetByCode(ctx, "1"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetByCode error = %v, want context.Canceled", err)
	}
}

func TestStoreExpiresAt(t *testing.T) {
	tests := []struct {
		name           string
		expiresIn      time.Duration
		mutateCaller   bool
		mutateReturned bool
		wantNotFound   bool
	}{
		{"caller mutation stays stored", time.Hour, true, false, false},
		{"returned mutation stays stored", time.Hour, false, true, false},
		{"past is not found", -time.Minute, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := New()
			ctx := context.Background()
			at := time.Now().Add(tt.expiresIn).UTC()

			var l *link.Link
			var err error
			if tt.wantNotFound {
				l, err = link.NewLink("https://example.com/past", nil)
				if err != nil {
					t.Fatalf("NewLink: %v", err)
				}
				l.ExpiresAt = &at
			} else {
				l, err = link.NewLink("https://example.com/future", &at)
				if err != nil {
					t.Fatalf("NewLink: %v", err)
				}
			}
			if err := store.Create(ctx, l); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tt.mutateCaller {
				*l.ExpiresAt = l.ExpiresAt.Add(-2 * time.Hour)
			}

			got, err := store.GetByCode(ctx, l.Code)
			if tt.wantNotFound {
				if !errors.Is(err, link.ErrNotFound) {
					t.Errorf("GetByCode error = %v, want link.ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetByCode: %v", err)
			}
			if got.ExpiresAt == nil || !got.ExpiresAt.Equal(at) {
				t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, at)
			}
			if !tt.mutateReturned {
				return
			}
			*got.ExpiresAt = time.Now().Add(-time.Hour)
			again, err := store.GetByCode(ctx, l.Code)
			if err != nil {
				t.Fatalf("second GetByCode: %v", err)
			}
			if again.ExpiresAt == nil || !again.ExpiresAt.Equal(at) {
				t.Errorf("stored ExpiresAt = %v, want %v", again.ExpiresAt, at)
			}
		})
	}
}
