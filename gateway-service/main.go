// gateway-service is the single public entry point for the OTel PoC.
//
// It demonstrates:
//   - OTel SDK initialisation (TracerProvider + MeterProvider)
//   - otelgin middleware for automatic HTTP server instrumentation
//   - Graceful shutdown that flushes buffered spans before exit
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"gateway-service/handlers"
	"gateway-service/internal/telemetry"
)

func main() {
	// Structured JSON logging — log entries include trace_id/span_id when
	// the context carries an active span (added manually or via middleware).
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	serviceName := env("SERVICE_NAME", "gateway-service")
	port := env("SERVICE_PORT", "8080")
	otlpEndpoint := env("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

	// ── Initialise OpenTelemetry ────────────────────────────────────────────
	// This must happen before any tracer/meter is obtained. It sets up the
	// global TracerProvider, MeterProvider, and TextMapPropagator.
	prov, err := telemetry.Init(ctx, serviceName, otlpEndpoint)
	if err != nil {
		slog.Error("failed to initialise telemetry", "error", err)
		os.Exit(1)
	}

	// ── Gin router ──────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// gin.Recovery() catches panics and returns 500 (avoiding crashes).
	r.Use(gin.Recovery())

	// otelgin.Middleware creates a server span for every incoming HTTP
	// request. The span name is "<METHOD> <route pattern>".
	// It also extracts the "traceparent" header from inbound requests so
	// that a caller's trace context is continued (not forked).
	r.Use(otelgin.Middleware(serviceName))

	h := handlers.New(
		env("USER_SERVICE_URL", "http://localhost:8081"),
		env("ORDER_SERVICE_URL", "http://localhost:8082"),
	)

	r.GET("/health", h.Health)
	r.GET("/users/:id", h.GetUser)
	r.POST("/orders", h.CreateOrder)

	// The prometheus exporter (initialised inside telemetry.Init) registers
	// all OTel metric instruments with the default prometheus registry.
	// promhttp.Handler() exposes them in the standard text/plain format.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// ── HTTP server ─────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("gateway-service listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			cancel()
		}
	}()

	// Block until SIGINT/SIGTERM.
	<-ctx.Done()
	slog.Info("shutdown signal received")

	// ── Graceful shutdown ────────────────────────────────────────────────────
	// Give in-flight requests 10 seconds to complete, then flush OTel buffers.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	// Flush buffered spans — without this, the last few spans may be lost.
	if err := prov.Shutdown(shutCtx); err != nil {
		slog.Error("telemetry shutdown error", "error", err)
	}

	slog.Info("gateway-service stopped")
}

func env(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
