// Package handlers implements the gateway HTTP handlers.
//
// # Trace Propagation
//
// Every inbound request is already wrapped in a server span by otelgin.
// Child spans are created with tracer.Start(ctx, …). When calling downstream
// services, http.NewRequestWithContext(ctx, …) binds the span context, and
// otelhttp.Transport injects it as a "traceparent" header. The downstream
// service's otelgin extracts the header and creates a child span — producing
// one trace tree spanning all three processes, visible in Jaeger.
//
// # Logs ↔ Traces Correlation
//
// Every slog.XxxContext(ctx, …) call goes through the logging.NewLogger handler.
// When ctx contains an active span (which it does inside any handler after
// tracer.Start), the handler injects trace_id and span_id into the log record.
// The same IDs appear in Jaeger's span detail. In Grafana Explore (Loki),
// filter by trace_id to see all logs for a single request.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("gateway-service")

// Handler holds shared state for all route handlers.
type Handler struct {
	userServiceURL  string
	orderServiceURL string
	httpClient      *http.Client

	requestsTotal    metric.Int64Counter
	requestDuration  metric.Float64Histogram
	upstreamErrors   metric.Int64Counter
}

// New constructs a Handler and initialises its OTel metric instruments.
func New(userServiceURL, orderServiceURL string) *Handler {
	meter := otel.Meter("gateway-service")

	reqTotal, _ := meter.Int64Counter(
		"gateway.requests.total",
		metric.WithDescription("Total HTTP requests handled by the gateway"),
		metric.WithUnit("{request}"),
	)

	reqDuration, _ := meter.Float64Histogram(
		"gateway.request.duration_ms",
		metric.WithDescription("Gateway request processing latency in milliseconds"),
		metric.WithUnit("ms"),
	)

	upstreamErr, _ := meter.Int64Counter(
		"gateway.upstream.errors.total",
		metric.WithDescription("Total upstream call failures"),
		metric.WithUnit("{error}"),
	)

	return &Handler{
		userServiceURL:  userServiceURL,
		orderServiceURL: orderServiceURL,
		// otelhttp.NewTransport injects "traceparent" into every outbound request.
		httpClient: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   20 * time.Second,
		},
		requestsTotal:   reqTotal,
		requestDuration: reqDuration,
		upstreamErrors:  upstreamErr,
	}
}

// Health is a liveness probe.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "gateway-service",
		"version": "2.0.0",
	})
}

// GetUser handles GET /users/:id.
// Demonstrates error spans (id=999), business logic spans, and upstream calls.
func (h *Handler) GetUser(c *gin.Context) {
	start := time.Now()
	userID := c.Param("id")
	ctx := c.Request.Context()

	ctx, span := tracer.Start(ctx, "gateway.get_user",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("user.id", userID),
			attribute.String("http.route", "/users/:id"),
			attribute.String("http.method", "GET"),
		),
	)
	defer span.End()

	h.requestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", "/users/:id"),
		attribute.String("method", "GET"),
	))

	// Simulated error: user 999 always returns not-found.
	if userID == "999" {
		err := fmt.Errorf("user %s not found (simulated)", userID)
		span.RecordError(err, trace.WithAttributes(attribute.String("error.type", "UserNotFound")))
		span.SetStatus(codes.Error, "user not found")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", "gateway")))
		slog.WarnContext(ctx, "user not found (simulated error path)",
			"user_id", userID,
		)
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "not_found")))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	user, err := h.callUserService(ctx, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream user-service call failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", "user-service")))
		slog.ErrorContext(ctx, "user-service call failed", "error", err, "user_id", userID)
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "upstream_error")))
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch user"})
		return
	}

	if name, ok := user["name"].(string); ok {
		span.SetAttributes(attribute.String("user.name", name))
	}
	if tier, ok := user["tier"].(string); ok {
		span.SetAttributes(attribute.String("user.tier", tier))
	}

	slog.InfoContext(ctx, "user fetched", "user_id", userID)
	h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "ok")))
	c.JSON(http.StatusOK, user)
}

// CreateOrder handles POST /orders.
// Body: {"user_id":1,"product_name":"widget","amount":29.99}
//
// Demo scenarios:
//   - product_name "slow-product"  → triggers 600 ms DB slow-query simulation
//   - product_name "out-of-stock"  → triggers inventory error span
//   - amount > 1000.00             → triggers payment declined span
//   - user_id 999                  → user validation fails (user not found)
func (h *Handler) CreateOrder(c *gin.Context) {
	start := time.Now()
	ctx := c.Request.Context()

	ctx, span := tracer.Start(ctx, "gateway.create_order",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("http.route", "/orders"),
			attribute.String("http.method", "POST"),
		),
	)
	defer span.End()

	h.requestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", "/orders"),
		attribute.String("method", "POST"),
	))

	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	userID := fmt.Sprintf("%v", body["user_id"])
	if amt, ok := body["amount"].(float64); ok {
		span.SetAttributes(attribute.Float64("order.amount", amt))
	}
	if pn, ok := body["product_name"].(string); ok {
		span.SetAttributes(attribute.String("order.product_name", pn))
	}
	span.SetAttributes(attribute.String("order.user_id", userID))

	// Validate user exists before forwarding.
	if err := h.validateUser(ctx, userID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user validation failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", "user-service")))
		slog.WarnContext(ctx, "order rejected: invalid user", "user_id", userID, "error", err)
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "invalid_user")))
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid user: %v", err)})
		return
	}

	order, err := h.callOrderService(ctx, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream order-service call failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", "order-service")))
		slog.ErrorContext(ctx, "order-service call failed", "error", err, "user_id", userID)
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "upstream_error")))
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create order"})
		return
	}

	orderID := fmt.Sprintf("%v", order["id"])
	span.AddEvent("order created", trace.WithAttributes(
		attribute.String("order.id", orderID),
		attribute.String("order.status", fmt.Sprintf("%v", order["status"])),
	))

	slog.InfoContext(ctx, "order created successfully",
		"order_id", orderID,
		"user_id", userID,
	)
	h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "ok")))
	c.JSON(http.StatusCreated, order)
}

// GetOrdersByUser handles GET /orders?user_id=1.
func (h *Handler) GetOrdersByUser(c *gin.Context) {
	start := time.Now()
	ctx := c.Request.Context()
	userID := c.Query("user_id")

	ctx, span := tracer.Start(ctx, "gateway.get_orders_by_user",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("http.route", "/orders"),
			attribute.String("http.method", "GET"),
			attribute.String("query.user_id", userID),
		),
	)
	defer span.End()

	h.requestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", "/orders"),
		attribute.String("method", "GET"),
	))

	if userID == "" {
		span.SetStatus(codes.Error, "missing user_id query param")
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id query param required"})
		return
	}

	url := fmt.Sprintf("%s/internal/orders?user_id=%s", h.orderServiceURL, userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to build request")
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "error")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "order-service call failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", "order-service")))
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "upstream_error")))
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch orders"})
		return
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "read_error")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read response"})
		return
	}

	h.requestDuration.Record(ctx, ms(start), metric.WithAttributes(attribute.String("result", "ok")))
	c.Data(resp.StatusCode, "application/json", data)
}

// validateUser calls user-service to confirm the user exists.
func (h *Handler) validateUser(ctx context.Context, userID string) error {
	ctx, span := tracer.Start(ctx, "gateway.validate_user",
		trace.WithAttributes(attribute.String("user.id", userID)),
	)
	defer span.End()

	_, err := h.callUserService(ctx, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found or service unavailable")
		return err
	}
	span.AddEvent("user validated")
	return nil
}

// callUserService makes an instrumented HTTP GET to user-service.
func (h *Handler) callUserService(ctx context.Context, userID string) (map[string]any, error) {
	url := fmt.Sprintf("%s/internal/users/%s", h.userServiceURL, userID)

	ctx, span := tracer.Start(ctx, "http.client GET user-service /internal/users/:id",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", "GET"),
			attribute.String("http.url", url),
			attribute.String("peer.service", "user-service"),
			attribute.String("user.id", userID),
		),
	)
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "http request failed")
		return nil, fmt.Errorf("http call failed: %w", err)
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, fmt.Sprintf("user-service returned HTTP %d", resp.StatusCode))
		return nil, fmt.Errorf("user-service returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// callOrderService makes an instrumented HTTP POST to order-service.
func (h *Handler) callOrderService(ctx context.Context, body map[string]any) (map[string]any, error) {
	url := h.orderServiceURL + "/internal/orders"

	ctx, span := tracer.Start(ctx, "http.client POST order-service /internal/orders",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", "POST"),
			attribute.String("http.url", url),
			attribute.String("peer.service", "order-service"),
		),
	)
	defer span.End()

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "http request failed")
		return nil, fmt.Errorf("http call failed: %w", err)
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusCreated {
		errBody, _ := io.ReadAll(resp.Body)
		span.SetStatus(codes.Error, fmt.Sprintf("order-service returned HTTP %d", resp.StatusCode))
		return nil, fmt.Errorf("order-service returned HTTP %d: %s", resp.StatusCode, errBody)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

func ms(start time.Time) float64 {
	return float64(time.Since(start).Milliseconds())
}
