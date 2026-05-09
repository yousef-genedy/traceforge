// Package handlers implements the user-service HTTP handlers.
//
// # How This Service Fits Into the Trace
//
// When the gateway calls GET /internal/users/:id, the request carries
// a "traceparent" header. otelgin extracts it and creates a child span
// under the gateway's trace. Any spans created here become grandchildren
// of the original request span — all visible as one tree in Jaeger.
//
// The in-memory store mirrors the users seeded in init.sql (IDs 1-3).
// User ID "2" triggers a simulated slow response to demonstrate how
// slow spans appear as wide bars in Jaeger's timeline view.
// User ID "999" triggers an error span (shown in red in Jaeger).
package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("user-service")

// User represents a user record.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// in-memory user store — mirrors the rows in init.sql
var store = map[string]User{
	"1": {ID: "1", Name: "Alice Johnson", Email: "alice@example.com"},
	"2": {ID: "2", Name: "Bob Smith", Email: "bob@example.com"},
	"3": {ID: "3", Name: "Charlie Brown", Email: "charlie@example.com"},
}

// Handler groups route handlers and their OTel instruments.
type Handler struct {
	lookups metric.Int64Counter
	latency metric.Float64Histogram
}

// New creates a Handler and registers its OTel metric instruments.
func New() *Handler {
	meter := otel.Meter("user-service")

	lookups, _ := meter.Int64Counter(
		"user_service.lookups.total",
		metric.WithDescription("Total user lookup requests"),
		metric.WithUnit("{request}"),
	)

	latency, _ := meter.Float64Histogram(
		"user_service.lookup.duration_ms",
		metric.WithDescription("User lookup latency in milliseconds"),
		metric.WithUnit("ms"),
	)

	return &Handler{lookups: lookups, latency: latency}
}

// Health is a liveness probe.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "user-service"})
}

// GetUser handles GET /internal/users/:id.
func (h *Handler) GetUser(c *gin.Context) {
	start := time.Now()
	userID := c.Param("id")

	// This span is a child of the gateway's span because otelgin already
	// extracted the traceparent header and set it in c.Request.Context().
	ctx, span := tracer.Start(c.Request.Context(), "user_service.get_user",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("user.id", userID),
		),
	)
	defer span.End()

	h.lookups.Add(ctx, 1, metric.WithAttributes(attribute.String("user.id", userID)))

	// ── Simulated error ──────────────────────────────────────────────────────
	// User "999" always returns a not-found error.
	// In Jaeger this span will be marked red with the error details.
	if userID == "999" {
		err := fmt.Errorf("user %s does not exist", userID)
		span.RecordError(err,
			trace.WithAttributes(attribute.String("error.type", "UserNotFound")),
		)
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "user not found", "user_id", userID)
		h.latency.Record(ctx, float64(time.Since(start).Milliseconds()))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// ── Simulated slow response ──────────────────────────────────────────────
	// User "2" triggers an artificial delay, creating a wide span in Jaeger.
	// This mimics a slow downstream (cache miss, external API, etc.).
	if userID == "2" {
		ctx, slowSpan := tracer.Start(ctx, "user_service.slow_lookup_simulation",
			trace.WithAttributes(
				attribute.String("simulation.reason", "cache miss"),
				attribute.Int("simulation.delay_ms", 250),
			),
		)
		slog.InfoContext(ctx, "simulating slow lookup", "user_id", userID)
		time.Sleep(250 * time.Millisecond)
		slowSpan.AddEvent("cache miss resolved, returning from database")
		slowSpan.End()
	}

	user, ok := store[userID]
	if !ok {
		span.SetStatus(codes.Error, "user not found")
		h.latency.Record(ctx, float64(time.Since(start).Milliseconds()))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	span.SetAttributes(
		attribute.String("user.name", user.Name),
		attribute.String("user.email", user.Email),
	)

	h.latency.Record(ctx, float64(time.Since(start).Milliseconds()),
		metric.WithAttributes(attribute.String("result", "hit")),
	)

	c.JSON(http.StatusOK, user)
}
