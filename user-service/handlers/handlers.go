// Package handlers implements the user-service HTTP handlers.
//
// When the gateway calls GET /internal/users/:id, the request carries a
// "traceparent" header. otelgin extracts it and creates a child span under
// the gateway's trace — so all spans share one trace_id visible in Jaeger.
//
// Demo scenarios:
//   - user_id "2"   → simulates a slow response (cache miss scenario)
//   - user_id "999" → simulates a not-found error span (shown red in Jaeger)
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
	Tier  string `json:"tier"` // "standard" | "premium"
}

// store mirrors the rows seeded in init.sql.
var store = map[string]User{
	"1": {ID: "1", Name: "Alice Johnson", Email: "alice@example.com", Tier: "premium"},
	"2": {ID: "2", Name: "Bob Smith", Email: "bob@example.com", Tier: "standard"},
	"3": {ID: "3", Name: "Charlie Brown", Email: "charlie@example.com", Tier: "standard"},
	"4": {ID: "4", Name: "Diana Prince", Email: "diana@example.com", Tier: "premium"},
	"5": {ID: "5", Name: "Evan Zhao", Email: "evan@example.com", Tier: "standard"},
}

// Handler groups route handlers and OTel instruments.
type Handler struct {
	lookups      metric.Int64Counter
	latency      metric.Float64Histogram
	notFoundRate metric.Int64Counter
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

	notFound, _ := meter.Int64Counter(
		"user_service.not_found.total",
		metric.WithDescription("Total user-not-found responses"),
		metric.WithUnit("{request}"),
	)

	return &Handler{lookups: lookups, latency: latency, notFoundRate: notFound}
}

// Health is a liveness probe.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "user-service"})
}

// GetUser handles GET /internal/users/:id.
func (h *Handler) GetUser(c *gin.Context) {
	start := time.Now()
	userID := c.Param("id")
	ctx := c.Request.Context()

	// This span is a child of the gateway's span because otelgin extracted
	// the "traceparent" header before dispatching to this handler.
	ctx, span := tracer.Start(ctx, "user_service.get_user",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("user.id", userID),
			attribute.String("http.route", "/internal/users/:id"),
		),
	)
	defer span.End()

	h.lookups.Add(ctx, 1, metric.WithAttributes(attribute.String("user.id", userID)))

	// ── Simulated error: user 999 not found ─────────────────────────────────────
	if userID == "999" {
		err := fmt.Errorf("user %s does not exist", userID)
		span.RecordError(err, trace.WithAttributes(attribute.String("error.type", "UserNotFound")))
		span.SetStatus(codes.Error, "user not found")
		slog.WarnContext(ctx, "user not found", "user_id", userID)
		h.notFoundRate.Add(ctx, 1)
		h.latency.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "not_found")))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// ── Simulated slow response: user 2 has a cache miss ────────────────────────
	// Produces a visibly wide span in Jaeger's waterfall view.
	if userID == "2" {
		ctx, slowSpan := tracer.Start(ctx, "user_service.cache_miss_simulation",
			trace.WithAttributes(
				attribute.String("cache.key", "user:"+userID),
				attribute.String("cache.result", "miss"),
				attribute.Int("cache.miss_delay_ms", 200),
			),
		)
		slog.InfoContext(ctx, "cache miss — fetching from data store", "user_id", userID)
		time.Sleep(200 * time.Millisecond)
		slowSpan.AddEvent("data store fetch complete")
		slowSpan.End()
	}

	user, ok := store[userID]
	if !ok {
		span.SetStatus(codes.Error, "user not found")
		h.notFoundRate.Add(ctx, 1)
		h.latency.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "not_found")))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	span.SetAttributes(
		attribute.String("user.name", user.Name),
		attribute.String("user.email", user.Email),
		attribute.String("user.tier", user.Tier),
	)

	slog.InfoContext(ctx, "user fetched",
		"user_id", userID,
		"user_name", user.Name,
		"user_tier", user.Tier,
	)

	h.latency.Record(ctx, ms(start), metric.WithAttributes(
		attribute.String("result", "hit"),
		attribute.String("user.tier", user.Tier),
	))

	c.JSON(http.StatusOK, user)
}

// ListUsers handles GET /internal/users.
func (h *Handler) ListUsers(c *gin.Context) {
	ctx := c.Request.Context()

	_, span := tracer.Start(ctx, "user_service.list_users",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.Int("user.count", len(store))),
	)
	defer span.End()

	users := make([]User, 0, len(store))
	for _, u := range store {
		users = append(users, u)
	}

	span.AddEvent("users listed")
	c.JSON(http.StatusOK, gin.H{"users": users, "count": len(users)})
}

func ms(start time.Time) float64 {
	return float64(time.Since(start).Milliseconds())
}
