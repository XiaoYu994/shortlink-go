package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

type failingWriter struct {
	header http.Header
	err    error
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingWriter) WriteHeader(int) {}

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestWriteJSONClientDisconnectDowngradedToDebug(t *testing.T) {
	tests := []struct {
		name      string
		cancelCtx bool
		writeErr  error
		wantLevel string
	}{
		{name: "broken pipe", writeErr: syscall.EPIPE, wantLevel: "DEBUG"},
		{name: "connection reset", writeErr: syscall.ECONNRESET, wantLevel: "DEBUG"},
		{name: "canceled context", cancelCtx: true, writeErr: errors.New("write failed"), wantLevel: "DEBUG"},
		{name: "unexpected write error", writeErr: errors.New("disk full"), wantLevel: "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			// Info 级别下，断连日志不输出。
			var infoBuf bytes.Buffer
			infoLogger := slog.New(slog.NewJSONHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			WriteJSON(infoLogger, ctx, &failingWriter{err: tt.writeErr}, http.StatusOK, map[string]bool{"ok": true})
			if tt.wantLevel == "DEBUG" && infoBuf.Len() != 0 {
				t.Fatalf("disconnect error logged at info level: %s", infoBuf.String())
			}

			// Debug 级别下核实准确级别。
			var debugBuf bytes.Buffer
			debugLogger := slog.New(slog.NewJSONHandler(&debugBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			WriteJSON(debugLogger, ctx, &failingWriter{err: tt.writeErr}, http.StatusOK, map[string]bool{"ok": true})
			entry := oneLogEntry(t, &debugBuf)
			if entry["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %s (entry=%+v)", entry["level"], tt.wantLevel, entry)
			}
			if tt.wantLevel == "ERROR" && !strings.Contains(debugBuf.String(), "disk full") {
				t.Errorf("error log missing cause: %s", debugBuf.String())
			}
		})
	}
}

func TestIsClientGone(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	if IsClientGone(logger, req) {
		t.Error("active context reported client gone")
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if !IsClientGone(logger, req.WithContext(canceledCtx)) {
		t.Error("canceled context not reported as client gone")
	}

	expiredCtx, expireCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expireCancel()
	if IsClientGone(logger, req.WithContext(expiredCtx)) {
		t.Error("deadline exceeded reported as client canceled")
	}
}
