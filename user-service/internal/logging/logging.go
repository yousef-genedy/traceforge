// Package logging provides a production-style slog logger with two outputs:
//
//  1. JSON to stdout — includes trace_id and span_id fields extracted from
//     the active OTel span in the context. Useful for `docker compose logs`.
//
//  2. OTLP log export — records flow through the global LoggerProvider
//     (set by telemetry.Init) to the OTel Collector, which exports them
//     to Loki. The SDK automatically correlates logs to traces.
//
// Call NewLogger after telemetry.Init so the global LoggerProvider is set.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps any slog.Handler and enriches every log record with
// trace_id, span_id, and trace_flags from the active OTel span in ctx.
// When there is no active span the record is passed through unchanged.
type traceHandler struct {
	inner slog.Handler
}

func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
			slog.String("trace_flags", sc.TraceFlags().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}

// fanHandler dispatches each log record to every inner handler.
type fanHandler []slog.Handler

func (m fanHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m fanHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (m fanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m fanHandler) WithGroup(name string) slog.Handler {
	out := make(fanHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// NewLogger returns a slog.Logger that writes to two sinks simultaneously:
//   - stdout (JSON, with trace_id/span_id injected from context)
//   - OTel global LoggerProvider → Collector → Loki
//
// serviceName is used as the OTel instrumentation scope name.
func NewLogger(serviceName string) *slog.Logger {
	stdoutHandler := &traceHandler{
		inner: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     slog.LevelInfo,
			AddSource: false,
		}),
	}
	otelHandler := otelslog.NewHandler(serviceName)
	return slog.New(fanHandler{stdoutHandler, otelHandler})
}
