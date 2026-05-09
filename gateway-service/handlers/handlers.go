// Package handlers implements the gateway HTTP handlers.
//
// # Distributed Tracing in the Gateway
//
// Every incoming request is already wrapped in a span by otelgin middleware.
// Handler code obtains the active span's context via c.Request.Context().
// Child spans are created with tracer.Start(ctx, "span-name"), which
// automatically parents them to the current span in ctx.
//
// When the gateway calls user-service or order-service:
//   - http.NewRequestWithContext(ctx, …) binds the span context to the request
//   - otelhttp.Transport injects that context as "traceparent" HTTP headers
//   - The downstream service's otelgin extracts it and creates a child span
//
// Result: one trace ID spans all three services, visible as a single tree
// in Jaeger.
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

// tracer is a package-level singleton. The name ("gateway-service") shows up
// as the instrumentation library name in Jaeger, separate from the span name.
var tracer = otel.Tracer("gateway-service")

// Handler holds shared state for all route handlers.
type Handler struct {
	userServiceURL  string
	orderServiceURL string
	httpClient      *http.Client

	// Custom metrics — these appear in Prometheus / Jaeger metrics view.
	requestsTotal  metric.Int64Counter
	upstreamErrors metric.Int64Counter
}

// New constructs a Handler and initialises its OTel instruments.
func New(userServiceURL, orderServiceURL string) *Handler {
	meter := otel.Meter("gateway-service")

	reqTotal, _ := meter.Int64Counter(
		"gateway.requests.total",
		metric.WithDescription("Total HTTP requests handled by the gateway"),
		metric.WithUnit("{request}"),
	)

	upstreamErr, _ := meter.Int64Counter(
		"gateway.upstream.errors.total",
		metric.WithDescription("Total upstream call failures"),
		metric.WithUnit("{error}"),
	)

	return &Handler{
		userServiceURL:  userServiceURL,
		orderServiceURL: orderServiceURL,
		// otelhttp.NewTransport wraps the default transport so that every
		// outbound request automatically carries "traceparent" headers.
		httpClient: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   15 * time.Second,
		},
		requestsTotal:  reqTotal,
		upstreamErrors: upstreamErr,
	}
}

// Health is a simple liveness probe — no tracing needed.
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "gateway-service",
	})
}

// GetUser handles GET /users/:id.
// It fans out to user-service and returns the user record.
// If id == "999" a simulated error span is produced.
func (h *Handler) GetUser(c *gin.Context) {
	userID := c.Param("id")

	// Create a child span beneath the otelgin root span.
	// SpanKind=Server because this is a server-side handler span.
	ctx, span := tracer.Start(c.Request.Context(), "gateway.get_user",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("user.id", userID),
			attribute.String("http.route", "/users/:id"),
		),
	)
	defer span.End()

	h.requestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint", "/users/:id"),
		attribute.String("method", "GET"),
	))

	// Simulated error path — user ID "999" always fails.
	// This demonstrates error spans in Jaeger (shown in red).
	if userID == "999" {
		err := fmt.Errorf("user %s not found (simulated error)", userID)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("service", "gateway")))
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	user, err := h.callUserService(ctx, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream user-service call failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("service", "user-service")))
		slog.ErrorContext(ctx, "user-service call failed", "error", err, "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch user"})
		return
	}

	// Enrich span with the returned user data.
	if name, ok := user["name"].(string); ok {
		span.SetAttributes(attribute.String("user.name", name))
	}

	c.JSON(http.StatusOK, user)
}

// CreateOrder handles POST /orders.
// Body: {"user_id": 1, "product_name": "widget", "amount": 29.99}
// It calls order-service which writes to PostgreSQL.
// Use product_name "slow-product" to trigger the slow-query simulation.
func (h *Handler) CreateOrder(c *gin.Context) {
	ctx, span := tracer.Start(c.Request.Context(), "gateway.create_order",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("http.route", "/orders")),
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

	// Annotate the span with business-relevant attributes.
	if uid, ok := body["user_id"]; ok {
		span.SetAttributes(attribute.String("order.user_id", fmt.Sprintf("%v", uid)))
	}
	if amt, ok := body["amount"].(float64); ok {
		span.SetAttributes(attribute.Float64("order.amount", amt))
	}
	if pn, ok := body["product_name"].(string); ok {
		span.SetAttributes(attribute.String("order.product_name", pn))
	}

	// Business logic span: validate that the user exists before creating order.
	// This creates an additional child span to show "business logic" wrapping.
	userID := fmt.Sprintf("%v", body["user_id"])
	if err := h.validateUser(ctx, userID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user validation failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("service", "user-service")))
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid user: %v", err)})
		return
	}

	order, err := h.callOrderService(ctx, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream order-service call failed")
		h.upstreamErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("service", "order-service")))
		slog.ErrorContext(ctx, "order-service call failed", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create order"})
		return
	}

	span.AddEvent("order created successfully", trace.WithAttributes(
		attribute.String("order.id", fmt.Sprintf("%v", order["id"])),
	))

	c.JSON(http.StatusCreated, order)
}

// validateUser is a "business logic span" wrapping a call to user-service.
// It demonstrates how you can wrap arbitrary logic in a named span.
func (h *Handler) validateUser(ctx context.Context, userID string) error {
	ctx, span := tracer.Start(ctx, "gateway.validate_user",
		trace.WithAttributes(attribute.String("user.id", userID)),
	)
	defer span.End()

	_, err := h.callUserService(ctx, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "user not found or unavailable")
		return err
	}
	span.AddEvent("user validated")
	return nil
}

// callUserService makes an instrumented HTTP GET to user-service.
// otelhttp.Transport (set on h.httpClient) automatically injects
// the traceparent header so user-service continues the same trace.
func (h *Handler) callUserService(ctx context.Context, userID string) (map[string]any, error) {
	ctx, span := tracer.Start(ctx, "http.client.user-service.get_user",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", "GET"),
			attribute.String("http.url", h.userServiceURL+"/internal/users/"+userID),
			attribute.String("peer.service", "user-service"),
		),
	)
	defer span.End()

	url := fmt.Sprintf("%s/internal/users/%s", h.userServiceURL, userID)
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

	span.SetAttributes(
		attribute.Int("http.status_code", resp.StatusCode),
	)

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, "non-200 response from user-service")
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
	ctx, span := tracer.Start(ctx, "http.client.order-service.create_order",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.method", "POST"),
			attribute.String("http.url", h.orderServiceURL+"/internal/orders"),
			attribute.String("peer.service", "order-service"),
		),
	)
	defer span.End()

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.orderServiceURL+"/internal/orders",
		bytes.NewReader(payload),
	)
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
		span.SetStatus(codes.Error, "non-201 response from order-service")
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
