// Package telemetry bootstraps the OpenTelemetry SDK for a service.
//
// # Three Signal Pillars
//
// TracerProvider — factory for Tracer instances. Spans flow via OTLP gRPC to
// the Collector, which fans them out to Jaeger.
//
// MeterProvider — factory for Meter instances (counters, histograms). Metrics
// are exposed as a Prometheus scrape endpoint on /metrics.
//
// LoggerProvider — factory for Logger instances. Log records flow via OTLP
// gRPC to the Collector, which exports them to Loki. Every record
// automatically carries the trace_id and span_id of the active span.
//
// TextMapPropagator — serialises the active SpanContext into HTTP headers
// (W3C "traceparent") so trace context travels across process boundaries.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Provider holds all SDK providers for graceful shutdown on process exit.
type Provider struct {
	trace  *sdktrace.TracerProvider
	meter  *sdkmetric.MeterProvider
	logger *sdklog.LoggerProvider
}

// Init initialises trace, metric, and log providers; wires up global OTel
// singletons; and returns a Provider whose Shutdown must be called on exit.
func Init(ctx context.Context, serviceName, otlpEndpoint string) (*Provider, error) {
	// ── Resource ────────────────────────────────────────────────────────────────
	// A Resource describes the entity producing telemetry (this process).
	// service.name appears as the service label in Jaeger, Grafana, and Loki.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("2.0.0"),
			attribute.String("deployment.environment", "development"),
			attribute.String("project", "traceforge"),
		),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	// ── gRPC connection to OTel Collector ───────────────────────────────────────
	// All signal exporters (traces, logs) share one gRPC connection. The
	// Collector decouples services from backends: swap Jaeger for Tempo, or
	// Loki for another log backend, without changing a single line of service code.
	conn, err := grpc.NewClient(
		otlpEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial collector: %w", err)
	}

	// ── Trace exporter ──────────────────────────────────────────────────────────
	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		// AlwaysSample = 100% in dev. Use TraceIDRatioBased(0.1) in production.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// ── Metric exporter (Prometheus pull model) ─────────────────────────────────
	// prometheus.New registers a reader with the default prometheus registry.
	// OTel instruments are exposed at the /metrics endpoint via promhttp.Handler.
	promExp, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
	)

	// ── Log exporter ────────────────────────────────────────────────────────────
	// Log records flow: service → OTLP gRPC → Collector → Loki.
	// The SDK automatically attaches the active span's trace_id and span_id
	// to every log record, enabling log-trace correlation in Grafana Explore.
	logExp, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("create log exporter: %w", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)

	// ── Register globals ────────────────────────────────────────────────────────
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)

	// ── Propagator ──────────────────────────────────────────────────────────────
	// W3C TraceContext propagates trace_id/span_id in the "traceparent" header.
	// W3C Baggage carries arbitrary key-value metadata across services.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{trace: tp, meter: mp, logger: lp}, nil
}

// Shutdown flushes all buffered spans, metrics, and log records, then releases
// resources. Call with a timeout context on process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	if err := p.trace.Shutdown(ctx); err != nil {
		return fmt.Errorf("trace provider shutdown: %w", err)
	}
	if err := p.meter.Shutdown(ctx); err != nil {
		return fmt.Errorf("meter provider shutdown: %w", err)
	}
	if err := p.logger.Shutdown(ctx); err != nil {
		return fmt.Errorf("log provider shutdown: %w", err)
	}
	return nil
}
