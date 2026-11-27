package httpx

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"shortlink/internal/logx"
)

const httpTracerName = "shortlink/httpx"

func startRequestSpan(r *http.Request, route string) (*http.Request, trace.Span) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	method := methodLabel(r.Method)
	spanName, httpRoute := httpSpanNameAndRoute(method, route)

	attrs := make([]attribute.KeyValue, 0, 3)
	attrs = append(attrs, attribute.String("http.request.method", method))
	if httpRoute != "" {
		attrs = append(attrs, attribute.String("http.route", httpRoute))
	}
	if reqID := RequestIDFrom(ctx); reqID != "" {
		attrs = append(attrs, attribute.String("shortlink.request_id", reqID))
	}

	ctx, span := otel.Tracer(httpTracerName).Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	)
	return r.WithContext(ctx), span
}

func finishRequestSpan(span trace.Span, status, bytesWritten int, snap logx.RequestEventSnapshot) {
	attrs := make([]attribute.KeyValue, 0, 6)
	attrs = append(attrs,
		attribute.Int("http.response.status_code", status),
		attribute.Int("http.response.body.size", bytesWritten),
	)
	if snap.TenantID > 0 {
		attrs = append(attrs, attribute.Int64("shortlink.tenant_id", snap.TenantID))
	}
	if snap.ActorKind != "" {
		attrs = append(attrs, attribute.String("shortlink.actor.kind", snap.ActorKind))
	}
	if snap.Cache != "" {
		attrs = append(attrs, attribute.String("shortlink.cache.result", snap.Cache))
	}
	if snap.RateLimited {
		attrs = append(attrs, attribute.Bool("shortlink.rate_limited", true))
	}
	span.SetAttributes(attrs...)

	if status >= http.StatusInternalServerError {
		desc := snap.ErrorMsg
		if desc == "" {
			desc = http.StatusText(status)
		}
		span.SetStatus(codes.Error, desc)
	}
	span.End()
}

func httpSpanNameAndRoute(method, route string) (spanName, httpRoute string) {
	if route == "" || route == routeUnmatched {
		return method, ""
	}
	if _, pat, ok := strings.Cut(route, " "); ok && pat != "" {
		httpRoute = pat
	} else {
		httpRoute = route
	}
	return method + " " + httpRoute, httpRoute
}
