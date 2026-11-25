package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type spyReadCloser struct {
	reads  int
	err    error
	onRead func()
}

func (r *spyReadCloser) Read([]byte) (int, error) {
	r.reads++
	if r.onRead != nil {
		r.onRead()
	}
	return 0, r.err
}

func (r *spyReadCloser) Close() error { return nil }

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

type logRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func parseJSONLogs(t *testing.T, raw []byte) []logRecord {
	t.Helper()
	var records []logRecord
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan logs: %v", err)
	}
	return records
}

func TestReadSmallRequestBodyErrorHandlingAndLogLevels(t *testing.T) {
	tests := []struct {
		name       string
		setupReq   func() (*http.Request, *spyReadCloser)
		wantStatus int
		wantErrSub string
		wantLevel  string // 空串表示不应产生任何 WARN/ERROR 日志
		wantReads  int    // >= 0 时校验底层 Read 调用次数
	}{
		{
			name: "already canceled context skips body read",
			setupReq: func() (*http.Request, *spyReadCloser) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				body := &spyReadCloser{err: io.ErrUnexpectedEOF}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body).WithContext(ctx)
				return req, body
			},
			wantStatus: 0,
			wantLevel:  "",
			wantReads:  0,
		},
		{
			name: "client disconnects mid-read",
			setupReq: func() (*http.Request, *spyReadCloser) {
				ctx, cancel := context.WithCancel(context.Background())
				body := &spyReadCloser{
					err:    io.ErrUnexpectedEOF,
					onRead: cancel,
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body).WithContext(ctx)
				return req, body
			},
			wantStatus: 0,
			wantLevel:  "",
			wantReads:  1,
		},
		{
			name: "body too large returns 413 without error log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(strings.Repeat("a", maxBodyBytes+1)))
				return req, nil
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantErrSub: "request body too large",
			wantLevel:  "",
			wantReads:  -1,
		},
		{
			name: "truncated body returns 400 with warn log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				body := &spyReadCloser{err: io.ErrUnexpectedEOF}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body)
				return req, body
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "read request body",
			wantLevel:  "WARN",
			wantReads:  1,
		},
		{
			name: "net read timeout returns 408 with warn log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				body := &spyReadCloser{err: timeoutNetError{}}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body)
				return req, body
			},
			wantStatus: http.StatusRequestTimeout,
			wantErrSub: "request timed out",
			wantLevel:  "WARN",
			wantReads:  1,
		},
		{
			name: "os deadline exceeded returns 408 with warn log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				body := &spyReadCloser{err: os.ErrDeadlineExceeded}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body)
				return req, body
			},
			wantStatus: http.StatusRequestTimeout,
			wantErrSub: "request timed out",
			wantLevel:  "WARN",
			wantReads:  1,
		},
		{
			name: "request context deadline exceeded during read returns 408 with warn log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				body := &spyReadCloser{err: io.ErrUnexpectedEOF}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body).WithContext(ctx)
				return req, body
			},
			wantStatus: http.StatusRequestTimeout,
			wantErrSub: "request timed out",
			wantLevel:  "WARN",
			wantReads:  1,
		},
		{
			name: "body read after close returns 500 with error log",
			setupReq: func() (*http.Request, *spyReadCloser) {
				body := &spyReadCloser{err: http.ErrBodyReadAfterClose}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/links", body)
				return req, body
			},
			wantStatus: http.StatusInternalServerError,
			wantErrSub: "internal error",
			wantLevel:  "ERROR",
			wantReads:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			req, spy := tt.setupReq()
			rec := httptest.NewRecorder()

			body, ok := readSmallRequestBody(logger, rec, req)
			if ok || body != nil {
				t.Fatalf("readSmallRequestBody() = (%q, %v), want (nil, false)", body, ok)
			}
			if tt.wantReads >= 0 && spy.reads != tt.wantReads {
				t.Errorf("body reads = %d, want %d", spy.reads, tt.wantReads)
			}

			if tt.wantStatus == 0 {
				if rec.Flushed || rec.Body.Len() != 0 {
					t.Errorf("unexpected response written on canceled request: status=%d body=%q", rec.Code, rec.Body.String())
				}
			} else {
				if rec.Code != tt.wantStatus {
					t.Errorf("status = %d, want %d, body = %q", rec.Code, tt.wantStatus, rec.Body.String())
				}
				if tt.wantErrSub != "" && !strings.Contains(rec.Body.String(), tt.wantErrSub) {
					t.Errorf("body = %q, want substring %q", rec.Body.String(), tt.wantErrSub)
				}
			}

			records := parseJSONLogs(t, logBuf.Bytes())
			var warnOrError []logRecord
			for _, rec := range records {
				if rec.Level == "WARN" || rec.Level == "ERROR" {
					warnOrError = append(warnOrError, rec)
				}
			}
			if tt.wantLevel == "" {
				if len(warnOrError) != 0 {
					t.Errorf("unexpected warn/error logs = %+v, want none", warnOrError)
				}
				return
			}
			if len(warnOrError) != 1 || warnOrError[0].Level != tt.wantLevel {
				t.Errorf("warn/error logs = %+v, want single %s log", warnOrError, tt.wantLevel)
			}
		})
	}
}

/*
TestReadSmallRequestBodyMalformedChunkedUsesWarn 验证真实 HTTP 连接收到畸形 chunked 请求体时，
不会像客户端断流一样取消 context，也不会把客户端坏请求打成服务端 ERROR 告警。
*/
func TestReadSmallRequestBodyMalformedChunkedUsesWarn(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	done := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		readSmallRequestBody(logger, w, r)
	}))
	defer ts.Close()

	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	rawReq := "POST /api/v1/links HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\nZZZZ\r\n"
	if _, err := io.WriteString(conn, rawReq); err != nil {
		t.Fatalf("write malformed chunked request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	<-done

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %q", resp.StatusCode, http.StatusBadRequest, respBody)
	}
	if !strings.Contains(string(respBody), "read request body") {
		t.Errorf("body = %q, want substring %q", respBody, "read request body")
	}

	records := parseJSONLogs(t, logBuf.Bytes())
	if len(records) != 1 || records[0].Level != "WARN" {
		t.Fatalf("logs = %+v, want single WARN log", records)
	}
}

func TestDecodeStrictAndFieldErrorMapping(t *testing.T) {
	type multiTimeRequest struct {
		URL              string     `json:"url"`
		ExpiresAt        *time.Time `json:"expires_at"`
		StartsAt         *time.Time `json:"starts_at"`
		DefaultExpiresAt int        `json:"default_expires_at"`
	}

	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name:    "expires_at bad time string",
			body:    `{"url":"https://example.com","expires_at":"tomorrow"}`,
			wantMsg: "invalid expires_at",
		},
		{
			name:    "expires_at number",
			body:    `{"url":"https://example.com","expires_at":1}`,
			wantMsg: "invalid expires_at",
		},
		{
			name:    "expires_at bool",
			body:    `{"url":"https://example.com","expires_at":true}`,
			wantMsg: "invalid expires_at",
		},
		{
			name:    "expires_at array",
			body:    `{"url":"https://example.com","expires_at":[1]}`,
			wantMsg: "invalid expires_at",
		},
		{
			name:    "expires_at object",
			body:    `{"url":"https://example.com","expires_at":{}}`,
			wantMsg: "invalid expires_at",
		},
		{
			name:    "other time field is not misreported as expires_at",
			body:    `{"url":"https://example.com","starts_at":"tomorrow"}`,
			wantMsg: "invalid JSON body",
		},
		{
			name:    "substring field name is not misreported as expires_at",
			body:    `{"url":"https://example.com","default_expires_at":"not-a-number"}`,
			wantMsg: "invalid JSON body",
		},
		{
			name:    "unknown field returns exact field name",
			body:    `{"url":"https://example.com","expire_at":"2030-01-01T00:00:00Z"}`,
			wantMsg: `unknown field "expire_at"`,
		},
		{
			name:    "unknown field containing expires_at still reports unknown field",
			body:    `{"url":"https://example.com","expires_at_ms":1}`,
			wantMsg: `unknown field "expires_at_ms"`,
		},
		{
			name:    "wrong case field rejected as unknown field",
			body:    `{"URL":"https://example.com"}`,
			wantMsg: `unknown field "URL"`,
		},
		{
			name:    "duplicate key rejected",
			body:    `{"url":"https://a.example.com","url":"https://b.example.com"}`,
			wantMsg: "invalid JSON body",
		},
		{
			name:    "trailing second value rejected",
			body:    `{"url":"https://example.com"}{"url":"https://example.com"}`,
			wantMsg: "invalid JSON body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req multiTimeRequest
			err := decodeStrict([]byte(tt.body), &req)
			if err == nil {
				t.Fatalf("decodeStrict(%s) = nil, want error", tt.body)
			}
			if got := expiresAtDecodeMessage(err); got != tt.wantMsg {
				t.Errorf("expiresAtDecodeMessage(%v) = %q, want %q", err, got, tt.wantMsg)
			}
		})
	}
}

func TestIsReadBodyTimeout(t *testing.T) {
	if isReadBodyTimeout(context.Background(), errors.New("generic")) {
		t.Error("generic error should not be classified as timeout")
	}
	if !isReadBodyTimeout(context.Background(), context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded error should be classified as timeout")
	}
	if !isReadBodyTimeout(context.Background(), os.ErrDeadlineExceeded) {
		t.Error("os.ErrDeadlineExceeded error should be classified as timeout")
	}
	if !isReadBodyTimeout(context.Background(), timeoutNetError{}) {
		t.Error("net.Error with Timeout()=true should be classified as timeout")
	}
}
