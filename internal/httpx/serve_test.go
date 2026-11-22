package httpx

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestServeDrainsBeforeShutdown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(logger, 2*time.Second, prometheus.NewRegistry())
	srv.SetReadiness()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var drained atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, srv.Handler(nil), logger, ServeConfig{
			RequestTimeout:  2 * time.Second,
			DrainDelay:      80 * time.Millisecond,
			ShutdownTimeout: 2 * time.Second,
			OnShutdown: func() {
				drained.Store(true)
				srv.SetDraining()
			},
		})
	}()

	client := &http.Client{Timeout: time.Second}
	baseURL := "http://" + ln.Addr().String()

	resp, err := client.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz before cancel: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz before cancel = %d, want 200", resp.StatusCode)
	}

	// 取消 ctx 后，摘流窗口内 /readyz 变 503，/healthz 仍为 200。
	cancel()
	deadline := time.Now().Add(time.Second)
	for !drained.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !drained.Load() {
		t.Fatal("OnShutdown was not called")
	}

	resp, err = client.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz during drain: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during drain = %d, want 503", resp.StatusCode)
	}

	resp, err = client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz during drain: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz during drain = %d, want 200", resp.StatusCode)
	}

	if err := <-done; err != nil {
		t.Fatalf("Serve() returned error: %v", err)
	}
}
