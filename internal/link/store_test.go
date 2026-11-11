package link

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryStoreCreateAndGetByCode(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	l, err := NewLink("https://example.com")
	if err != nil {
		t.Fatalf("NewLink returned error: %v", err)
	}
	if err := store.Create(ctx, l); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if l.ID != 1 {
		t.Errorf("ID = %d, want 1", l.ID)
	}
	if l.Code != "1" {
		t.Errorf("Code = %q, want %q", l.Code, "1")
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

func TestMemoryStoreGetByCodeNotFound(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.GetByCode(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByCode error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreGetByCodeReturnsCopy(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	l, err := NewLink("https://example.com")
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

func TestMemoryStoreConcurrentCreate(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	const n = 100

	var wg sync.WaitGroup
	codes := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			l, err := NewLink("https://example.com")
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

func TestMemoryStoreContextCanceled(t *testing.T) {
	store := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l, err := NewLink("https://example.com")
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
