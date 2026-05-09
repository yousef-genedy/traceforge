// Package telemetry bootstraps the OpenTelemetry SDK for a service.
//
// # Core Concepts
//
// TracerProvider — factory for Tracer instances. A single provider is shared
// process-wide and registered globally via otel.SetTracerProvider().
//
// MeterProvider — factory for Meter instances (counters, histograms, etc.).
// Registered globally via otel.SetMeterProvider().
//
// TextMapPropagator — serialises/deserialises the current SpanContext into
// HTTP headers so trace context can travel across process boundaries.
// We use the W3C "traceparent" standard which looks like:
//
//	traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//	             ^^  ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^  ^^^^^^^^^^^^^^^^  ^^
//	             ver  trace-id (128 bit hex)            span-id (64 bit)  flags
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Provider holds references to the SDK providers so they can be shut down
// gracefully at process exit (flushing any buffered spans/metrics).
type Provider struct {
	trace *sdktrace.TracerProvider
	meter *sdkmetric.MeterProvider
}

// Init initialises both trace and metric providers, wires up global OTel
// singletons, and returns a Provider whose Shutdown must be called on exit.
func Init(ctx context.Context, serviceName, otlpEndpoint string) (*Provider, error) {
	// ── Resource ──────────────────────────────────────────────────────────────
	// A Resource describes the entity producing telemetry (this process).
	// resource.New merges:
	//   • our explicit attributes (service.name, version, env)
	//   • auto-detected process attributes (pid, executable name)
	//   • host attributes
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("1.0.0"),
			attribute.String("deployment.environment", "development"),
		),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	// ── Trace exporter ────────────────────────────────────────────────────────
	// We send spans to the OTel Collector using the OTLP gRPC protocol.
	// The Collector (not the service) is responsible for forwarding to Jaeger.
	// This decouples services from the backend — swap Jaeger for Tempo by
	// only changing the collector config, without touching service code.
	conn, err := grpc.NewClient(
		otlpEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial collector: %w", err)
	}

	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	// ── TracerProvider ────────────────────────────────────────────────────────
	// BatchSpanProcessor buffers spans in memory and flushes them to the
	// exporter in batches. This is more efficient than sending each span
	// individually (SimpleSpanProcessor is only for debugging).
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		// AlwaysSample captures 100 % of traces. In production, use
		// sdktrace.TraceIDRatioBased(0.1) for 10 % sampling.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// ── Metric exporter (Prometheus) ──────────────────────────────────────────
	// prometheus.New() registers a reader with the default prometheus registry.
	// OTel instruments (counters, histograms) are automatically exposed at
	// the /metrics endpoint served by promhttp.Handler().
	promExp, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
	)

	// ── Register globals ──────────────────────────────────────────────────────
	// Library code (otelgin, otelhttp) calls otel.GetTracerProvider() etc.
	// Setting these globals means libraries pick up our configured providers.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	// ── Propagator ────────────────────────────────────────────────────────────
	// The propagator is what makes distributed tracing "distributed".
	// When an HTTP client makes a request, otelhttp injects the current
	// SpanContext into outgoing headers (traceparent, tracestate, baggage).
	// The receiving service's otelgin middleware extracts those headers and
	// continues the same trace — creating a parent→child relationship.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C TraceContext (traceparent / tracestate)
		propagation.Baggage{},     // W3C Baggage (arbitrary key-value metadata)
	))

	return &Provider{trace: tp, meter: mp}, nil
}

// Shutdown flushes all buffered spans and metrics then releases resources.
// Call this with a timeout context on process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	if err := p.trace.Shutdown(ctx); err != nil {
		return fmt.Errorf("trace provider shutdown: %w", err)
	}
	if err := p.meter.Shutdown(ctx); err != nil {
		return fmt.Errorf("meter provider shutdown: %w", err)
	}
	return nil
}
