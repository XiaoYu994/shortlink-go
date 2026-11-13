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
	store := New()
	ctx := context.Background()

	future := time.Now().Add(time.Hour).UTC()
	l, err := link.NewLink("https://example.com/future", &future)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create: %v", err)
	}
	*l.ExpiresAt = l.ExpiresAt.Add(-2 * time.Hour)

	got, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, future)
	}
	*got.ExpiresAt = time.Now().Add(-time.Hour)
	again, err := store.GetByCode(ctx, l.Code)
	if err != nil {
		t.Fatalf("second GetByCode: %v", err)
	}
	if again.ExpiresAt == nil || !again.ExpiresAt.Equal(future) {
		t.Errorf("stored ExpiresAt = %v, want %v", again.ExpiresAt, future)
	}

	past := time.Now().Add(-time.Minute)
	expired, err := link.NewLink("https://example.com/past", nil)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	expired.ExpiresAt = &past
	if err := store.Create(ctx, expired); err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if _, err := store.GetByCode(ctx, expired.Code); !errors.Is(err, link.ErrNotFound) {
		t.Errorf("GetByCode expired error = %v, want link.ErrNotFound", err)
	}
}
