// Package handlers implements the order-service HTTP handlers.
//
// # Full Span Tree for POST /internal/orders
//
//	gateway.create_order
//	  └─ gateway.validate_user
//	  └─ http.client POST order-service /internal/orders
//	       └─ order_service.create_order          ← this file
//	            ├─ order_service.validate_order
//	            ├─ order_service.check_inventory   ← simulated stock check
//	            ├─ order_service.process_payment   ← simulated payment gate
//	            └─ db.orders.insert                ← leaf, hits PostgreSQL
//
// Demo scenarios to try:
//   - product_name "out-of-stock"  → inventory check span errors (422)
//   - product_name "slow-product"  → db span sleeps 600 ms (visible in Jaeger)
//   - amount > 1000.00             → payment processing span errors (402)
//   - user_id 0                    → validate_order span errors (400)
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
	ordersFailed    metric.Int64Counter
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

	failed, _ := meter.Int64Counter(
		"order_service.orders.failed.total",
		metric.WithDescription("Total orders that failed at any stage"),
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
		ordersFailed:    failed,
		processDuration: duration,
	}
}

// Health is a liveness probe.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "order-service"})
}

// CreateOrder handles POST /internal/orders.
//
// Body: {"user_id":1,"product_name":"widget","amount":29.99}
func (h *Handler) CreateOrder(c *gin.Context) {
	start := time.Now()
	ctx := c.Request.Context()

	ctx, span := tracer.Start(ctx, "order_service.create_order",
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
		h.ordersFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "bad_request")))
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}

	span.SetAttributes(
		attribute.Int64("order.user_id", req.UserID),
		attribute.String("order.product_name", req.ProductName),
		attribute.Float64("order.amount", req.Amount),
	)

	// ── Stage 1: Input Validation ────────────────────────────────────────────────
	if err := h.validateOrder(ctx, req.UserID, req.ProductName, req.Amount); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "validation failed")
		h.ordersFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "validation_error")))
		h.recordDuration(ctx, start, "validation_error")
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// ── Stage 2: Inventory Check ────────────────────────────────────────────────
	if err := h.checkInventory(ctx, req.ProductName); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "inventory check failed")
		h.ordersFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "out_of_stock")))
		slog.WarnContext(ctx, "order rejected: out of stock",
			"product_name", req.ProductName,
			"user_id", req.UserID,
		)
		h.recordDuration(ctx, start, "out_of_stock")
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	// ── Stage 3: Payment Processing ─────────────────────────────────────────────
	if err := h.processPayment(ctx, req.UserID, req.Amount); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "payment failed")
		h.ordersFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "payment_failed")))
		slog.WarnContext(ctx, "order rejected: payment declined",
			"user_id", req.UserID,
			"amount", req.Amount,
		)
		h.recordDuration(ctx, start, "payment_failed")
		c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error()})
		return
	}

	// ── Stage 4: Database Write ─────────────────────────────────────────────────
	order, err := h.db.CreateOrder(ctx, db.Order{
		UserID:      req.UserID,
		ProductName: req.ProductName,
		Amount:      req.Amount,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "database write failed")
		slog.ErrorContext(ctx, "failed to create order in DB",
			"error", err,
			"user_id", req.UserID,
			"product_name", req.ProductName,
		)
		h.ordersFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "db_error")))
		h.recordDuration(ctx, start, "db_error")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create order"})
		return
	}

	span.SetAttributes(
		attribute.Int64("order.id", order.ID),
		attribute.String("order.status", order.Status),
	)
	span.AddEvent("order persisted", trace.WithAttributes(
		attribute.Int64("order.id", order.ID),
		attribute.String("order.status", order.Status),
	))

	h.ordersCreated.Add(ctx, 1, metric.WithAttributes(
		attribute.String("product_name", order.ProductName),
		attribute.String("status", order.Status),
	))
	h.recordDuration(ctx, start, "ok")

	slog.InfoContext(ctx, "order created",
		"order_id", order.ID,
		"user_id", order.UserID,
		"product_name", order.ProductName,
		"amount", order.Amount,
		"status", order.Status,
	)

	c.JSON(http.StatusCreated, order)
}

// GetOrdersByUser handles GET /internal/orders?user_id=1.
func (h *Handler) GetOrdersByUser(c *gin.Context) {
	ctx := c.Request.Context()

	ctx, span := tracer.Start(ctx, "order_service.get_orders_by_user",
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
		slog.ErrorContext(ctx, "failed to list orders", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch orders"})
		return
	}

	span.SetAttributes(attribute.Int("orders.count", len(orders)))
	slog.InfoContext(ctx, "orders fetched", "user_id", userID, "count", len(orders))
	c.JSON(http.StatusOK, gin.H{"orders": orders, "count": len(orders)})
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

// checkInventory simulates a stock-level check.
//
// In a real system this would query an inventory DB. Here it's a deterministic
// simulation that produces a visible child span in Jaeger.
//
// "out-of-stock" always returns an error; all other products pass.
func (h *Handler) checkInventory(ctx context.Context, productName string) error {
	ctx, span := tracer.Start(ctx, "order_service.check_inventory",
		trace.WithAttributes(
			attribute.String("inventory.product_name", productName),
			attribute.String("inventory.check_method", "simulated"),
		),
	)
	defer span.End()

	// Simulate a brief network round-trip to an inventory service.
	time.Sleep(20 * time.Millisecond)

	if productName == "out-of-stock" {
		err := fmt.Errorf("product %q is out of stock", productName)
		span.RecordError(err, trace.WithAttributes(
			attribute.String("inventory.status", "out_of_stock"),
			attribute.Int("inventory.available_units", 0),
		))
		span.SetStatus(codes.Error, "out of stock")
		return err
	}

	availableUnits := 42 // simulated stock level
	span.SetAttributes(
		attribute.String("inventory.status", "available"),
		attribute.Int("inventory.available_units", availableUnits),
	)
	span.AddEvent("inventory check passed")
	return nil
}

// processPayment simulates payment gateway authorisation.
//
// Amounts > 1000.00 are declined (simulated high-value transaction block).
// A 30 ms delay models the round-trip to the payment provider.
func (h *Handler) processPayment(ctx context.Context, userID int64, amount float64) error {
	ctx, span := tracer.Start(ctx, "order_service.process_payment",
		trace.WithAttributes(
			attribute.Int64("payment.user_id", userID),
			attribute.Float64("payment.amount", amount),
			attribute.String("payment.provider", "stripe-sim"),
			attribute.String("payment.currency", "USD"),
		),
	)
	defer span.End()

	// Simulate round-trip to payment provider.
	time.Sleep(30 * time.Millisecond)

	if amount > 1000.00 {
		err := fmt.Errorf("payment declined: amount %.2f exceeds transaction limit", amount)
		span.RecordError(err, trace.WithAttributes(
			attribute.String("payment.status", "declined"),
			attribute.String("payment.decline_reason", "amount_exceeds_limit"),
		))
		span.SetStatus(codes.Error, "payment declined")
		return err
	}

	span.SetAttributes(
		attribute.String("payment.status", "authorised"),
		attribute.String("payment.auth_code", fmt.Sprintf("AUTH-%d-%d", userID, time.Now().UnixMilli()%100000)),
	)
	span.AddEvent("payment authorised")
	return nil
}

func (h *Handler) recordDuration(ctx context.Context, start time.Time, result string) {
	h.processDuration.Record(ctx,
		float64(time.Since(start).Milliseconds()),
		metric.WithAttributes(attribute.String("result", result)),
	)
}
