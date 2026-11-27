/*
Package tracex 是全仓唯一持有 OpenTelemetry SDK 与 OTLP 导出器的初始化包（ADR 0032）。

仅由进程组合根（cmd/redirector、cmd/console）调用；其余业务与底座包只依赖
go.opentelemetry.io/otel 与 trace API，不引入 sdk 或 exporter，由 make arch-check 强制检查。
*/
package tracex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const exportTimeout = 5 * time.Second

// Options 控制进程级 OpenTelemetry TracerProvider 的装配参数。
type Options struct {
	ServiceName string
	Endpoint    string
	SampleRate  float64
	Logger      *slog.Logger
	// Exporter 仅用于测试注入内存导出器（如 tracetest.NewInMemoryExporter），生产留空使用 OTLP/HTTP。
	Exporter sdktrace.SpanExporter
}

/*
Init 根据配置初始化全局 OpenTelemetry TracerProvider 与 W3C TraceContext 传播器。

当 Endpoint 为空且未注入测试 Exporter 时直接返回 (noop, false, nil)：
不创建 BatchSpanProcessor、不起后台协程、不连网络，也不改写全局默认 Noop TracerProvider。
*/
func Init(ctx context.Context, opts Options) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if strings.TrimSpace(opts.Endpoint) == "" && opts.Exporter == nil {
		return noop, false, nil
	}
	if strings.TrimSpace(opts.ServiceName) == "" {
		return nil, false, errors.New("tracex: service name is required")
	}

	exp := opts.Exporter
	if exp == nil {
		endpointURL, err := normalizeTracesEndpointURL(opts.Endpoint)
		if err != nil {
			return nil, false, err
		}
		exp, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpointURL),
			otlptracehttp.WithTimeout(exportTimeout),
		)
		if err != nil {
			return nil, false, fmt.Errorf("tracex: create otlp http exporter: %w", err)
		}
	}

	// 使用 NewSchemaless 避免 resource.Default() 与不同 semconv 版本的 SchemaURL 冲突报错。
	res := resource.NewSchemaless(semconv.ServiceName(opts.ServiceName))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(buildSampler(opts.SampleRate)),
	)

	if opts.Logger != nil {
		logger := opts.Logger
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			logger.Warn("otel tracing error", "err", err)
		}))
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, true, nil
}

func normalizeTracesEndpointURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("tracex: invalid OTLP endpoint URL %q", raw)
	}
	if !strings.HasSuffix(u.Path, "/v1/traces") {
		u.Path = path.Join(u.Path, "/v1/traces")
	}
	return u.String(), nil
}

func buildSampler(rate float64) sdktrace.Sampler {
	var root sdktrace.Sampler
	switch {
	case rate <= 0:
		root = sdktrace.NeverSample()
	case rate >= 1:
		root = sdktrace.AlwaysSample()
	default:
		root = sdktrace.TraceIDRatioBased(rate)
	}
	return sdktrace.ParentBased(root)
}
