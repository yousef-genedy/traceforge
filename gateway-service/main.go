// gateway-service is the single public entry point for the TraceForge platform.
//
// It demonstrates all three OTel signal pillars:
//   - Traces  → OTel Collector → Jaeger
//   - Metrics → Prometheus scrape endpoint → Prometheus → Grafana
//   - Logs    → OTel Collector → Loki (with trace_id/span_id correlation)
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
	"gateway-service/internal/logging"
	"gateway-service/internal/telemetry"
)

func main() {
	// Bootstrap logger before OTel is available (startup errors need somewhere to go).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	serviceName := env("SERVICE_NAME", "gateway-service")
	port := env("SERVICE_PORT", "8080")
	otlpEndpoint := env("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

	// ── Initialise OpenTelemetry (traces + metrics + logs) ──────────────────────
	prov, err := telemetry.Init(ctx, serviceName, otlpEndpoint)
	if err != nil {
		slog.Error("failed to initialise telemetry", "error", err)
		os.Exit(1)
	}

	// Replace bootstrap logger with the production logger that:
	//   • emits JSON to stdout with trace_id/span_id injected from context
	//   • exports records via OTLP → Collector → Loki
	slog.SetDefault(logging.NewLogger(serviceName))
	slog.Info("telemetry initialised", "service", serviceName, "otlp_endpoint", otlpEndpoint)

	// ── Gin router ───────────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// otelgin creates a server span per request and extracts inbound traceparent.
	r.Use(otelgin.Middleware(serviceName))

	h := handlers.New(
		env("USER_SERVICE_URL", "http://localhost:8081"),
		env("ORDER_SERVICE_URL", "http://localhost:8082"),
	)

	r.GET("/health", h.Health)
	r.GET("/users/:id", h.GetUser)
	r.POST("/orders", h.CreateOrder)
	r.GET("/orders", h.GetOrdersByUser)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// ── HTTP server ──────────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("gateway-service listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")

	// ── Graceful shutdown ────────────────────────────────────────────────────────
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	// Flush buffered spans, metrics, and log records before exit.
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
