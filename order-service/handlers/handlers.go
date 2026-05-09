// Package handlers implements the order-service HTTP handlers.
//
// # Trace Depth at This Point
//
//	gateway (root span)
//	  └─ gateway.create_order
//	       ├─ gateway.validate_user
//	       │    └─ http.client.user-service.get_user
//	       │         └─ [user-service spans — different process]
//	       └─ http.client.order-service.create_order
//	            └─ [order-service spans — this file]   ← we are here
//	                  └─ db.orders.insert              ← leaf, hits PostgreSQL
//
// All of the above share ONE trace ID and are visible as a single waterfall
// in Jaeger, even though they run in three separate Go processes.
package handlers

import (
	"context"
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

	"order-service/internal/db"
)

var tracer = otel.Tracer("order-service")

// Handler groups route handlers, the DB client, and OTel instruments.
type Handler struct {
	db              *db.DB
	ordersCreated   metric.Int64Counter
	processDuration metric.Float64Histogram
}

// New constructs a Handler with the injected DB dependency.
func New(database *db.DB) *Handler {
	meter := otel.Meter("order-service")

	created, _ := meter.Int64Counter(
		"order_service.orders.created.total",
		metric.WithDescription("Total orders successfully created"),
		metric.WithUnit("{order}"),
	)

	duration, _ := meter.Float64Histogram(
		"order_service.order_processing.duration_ms",
		metric.WithDescription("End-to-end order processing latency in milliseconds"),
		metric.WithUnit("ms"),
	)

	return &Handler{
		db:              database,
		ordersCreated:   created,
		processDuration: duration,
	}
}

// Health is a liveness probe.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "order-service"})
}

// CreateOrder handles POST /internal/orders.
//
// Request body: {"user_id": 1, "product_name": "widget", "amount": 29.99}
//
// Special values for demo purposes:
//   - product_name "slow-product" → triggers 600 ms slow-query simulation
//   - user_id 0                   → triggers a DB constraint error span
func (h *Handler) CreateOrder(c *gin.Context) {
	start := time.Now()

	// The context carries the W3C traceparent propagated from the gateway.
	// otelgin extracted it before dispatching to this handler, so any span
	// we create here is automatically a child of the gateway's span.
	ctx, span := tracer.Start(c.Request.Context(), "order_service.create_order",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	var req struct {
		UserID      int64   `json:"user_id"      binding:"required"`
		ProductName string  `json:"product_name" binding:"required"`
		Amount      float64 `json:"amount"       binding:"required,gt=0"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return
	}

	span.SetAttributes(
		attribute.Int64("order.user_id", req.UserID),
		attribute.String("order.product_name", req.ProductName),
		attribute.Float64("order.amount", req.Amount),
	)

	// ── Business logic span ───────────────────────────────────────────────
	// Wrap validation in its own child span so Jaeger shows it as a discrete
	// step with its own duration — useful for spotting slow validation paths.
	if err := h.validateOrder(ctx, req.UserID, req.ProductName, req.Amount); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "input validation failed")
		h.recordDuration(ctx, start, "validation_error")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// ── Database write ────────────────────────────────────────────────────
	// db.CreateOrder creates its own child span (db.orders.insert).
	// The span tree so far:
	//   order_service.create_order
	//     └─ order_service.validate_order
	//     └─ db.orders.insert              ← next
	order, err := h.db.CreateOrder(ctx, db.Order{
		UserID:      req.UserID,
		ProductName: req.ProductName,
		Amount:      req.Amount,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "database write failed")
		slog.ErrorContext(ctx, "failed to create order", "error", err, "user_id", req.UserID)
		h.recordDuration(ctx, start, "db_error")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create order"})
		return
	}

	span.SetAttributes(attribute.Int64("order.id", order.ID))
	span.AddEvent("order persisted", trace.WithAttributes(
		attribute.Int64("order.id", order.ID),
		attribute.String("order.status", order.Status),
	))

	h.ordersCreated.Add(ctx, 1, metric.WithAttributes(
		attribute.String("product_name", order.ProductName),
	))
	h.recordDuration(ctx, start, "ok")

	slog.InfoContext(ctx, "order created",
		"order_id", order.ID,
		"user_id", order.UserID,
		"amount", order.Amount,
	)

	c.JSON(http.StatusCreated, order)
}

// validateOrder wraps business-rule validation in a child span.
func (h *Handler) validateOrder(ctx context.Context, userID int64, productName string, amount float64) error {
	_, span := tracer.Start(ctx, "order_service.validate_order",
		trace.WithAttributes(
			attribute.Int64("order.user_id", userID),
			attribute.String("order.product_name", productName),
			attribute.Float64("order.amount", amount),
		),
	)
	defer span.End()

	if userID <= 0 {
		err := fmt.Errorf("user_id must be positive, got %d", userID)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid user_id")
		return err
	}
	if productName == "" {
		err := fmt.Errorf("product_name is required")
		span.RecordError(err)
		span.SetStatus(codes.Error, "missing product_name")
		return err
	}
	if amount <= 0 {
		err := fmt.Errorf("amount must be positive, got %.2f", amount)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid amount")
		return err
	}

	span.AddEvent("validation passed")
	return nil
}

// GetOrdersByUser handles GET /internal/orders?user_id=1.
func (h *Handler) GetOrdersByUser(c *gin.Context) {
	ctx, span := tracer.Start(c.Request.Context(), "order_service.get_orders_by_user",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer span.End()

	var userID int64
	if _, err := fmt.Sscan(c.Query("user_id"), &userID); err != nil || userID == 0 {
		span.SetStatus(codes.Error, "missing or invalid user_id query param")
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id query param required"})
		return
	}

	span.SetAttributes(attribute.Int64("query.user_id", userID))

	orders, err := h.db.ListOrdersByUser(ctx, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch orders"})
		return
	}

	span.SetAttributes(attribute.Int("orders.count", len(orders)))
	c.JSON(http.StatusOK, gin.H{"orders": orders, "count": len(orders)})
}

func (h *Handler) recordDuration(ctx context.Context, start time.Time, result string) {
	h.processDuration.Record(ctx,
		float64(time.Since(start).Milliseconds()),
		metric.WithAttributes(attribute.String("result", result)),
	)
}
