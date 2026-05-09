// order-service creates orders in PostgreSQL.
// It demonstrates DB instrumentation: manual OTel spans around SQL queries.
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

	"order-service/handlers"
	"order-service/internal/db"
	"order-service/internal/telemetry"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	serviceName := env("SERVICE_NAME", "order-service")
	port := env("SERVICE_PORT", "8082")
	otlpEndpoint := env("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

	prov, err := telemetry.Init(ctx, serviceName, otlpEndpoint)
	if err != nil {
		slog.Error("failed to initialise telemetry", "error", err)
		os.Exit(1)
	}

	// ── Database connection ─────────────────────────────────────────────────
	database, err := db.New(ctx, db.Config{
		Host:     env("DB_HOST", "localhost"),
		Port:     env("DB_PORT", "5432"),
		Name:     env("DB_NAME", "otelpoc"),
		User:     env("DB_USER", "postgres"),
		Password: env("DB_PASSWORD", "postgres"),
	})
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// ── Gin router ──────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(otelgin.Middleware(serviceName))

	h := handlers.New(database)

	r.GET("/health", h.Health)
	r.POST("/internal/orders", h.CreateOrder)
	r.GET("/internal/orders", h.GetOrdersByUser)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second, // longer for slow-query simulation
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		slog.Info("order-service listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	if err := prov.Shutdown(shutCtx); err != nil {
		slog.Error("telemetry shutdown error", "error", err)
	}

	slog.Info("order-service stopped")
}

func env(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
