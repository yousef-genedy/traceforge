# OpenTelemetry Distributed Tracing — PoC

A complete, runnable proof-of-concept demonstrating distributed tracing, metrics
collection, and the OpenTelemetry Collector pipeline in a Go microservices system.

---

## Architecture

```
                          ┌───────────────────┐
                          │    HTTP Client     │
                          │  (curl / browser)  │
                          └────────┬──────────┘
                                   │ HTTP
                                   ▼
                    ┌──────────────────────────────┐
                    │     gateway-service :8080     │
                    │                              │
                    │  GET /users/:id              │
                    │  POST /orders                │
                    │  GET /health                 │
                    │  GET /metrics                │
                    └────────┬─────────┬───────────┘
                             │         │
                   HTTP+     │         │  HTTP+
                traceparent  │         │  traceparent
                   header    │         │  header
                             ▼         ▼
            ┌────────────────────┐  ┌───────────────────────┐
            │  user-service      │  │  order-service :8082  │
            │  :8081             │  │                       │
            │                   │  │  POST /internal/orders │
            │  GET /internal/    │  │  GET  /internal/orders │
            │    users/:id       │  │                       │
            │                   │  └──────────┬────────────┘
            │  in-memory store   │             │ SQL
            └────────────────────┘             ▼
                                      ┌─────────────────┐
                                      │   PostgreSQL     │
                                      │   :5432          │
                                      │                  │
                                      │   users table    │
                                      │   orders table   │
                                      └─────────────────┘

Telemetry pipeline (all services → collector → backends):

  ┌─────────────────────────────────────────────────────────────┐
  │             OpenTelemetry Collector :4317                    │
  │                                                             │
  │   OTLP receiver                                             │
  │       │                                                     │
  │       ▼                                                     │
  │   batch processor                                           │
  │       │                                                     │
  │       ├──► Jaeger exporter  ──► Jaeger UI :16686           │
  │       │    (otlp/jaeger)                                    │
  │       │                                                     │
  │       └──► Prometheus exporter :8889                        │
  │                                    ▲                        │
  └────────────────────────────────────┼────────────────────────┘
                                       │ scrape
                              ┌────────┴─────────┐
                              │   Prometheus :9090 │
                              │                   │
                              │ also scrapes each │
                              │ service /metrics  │
                              └───────────────────┘
```

---

## How Distributed Tracing Works

### 1. Trace Context Propagation

When a client calls `GET /users/1`:

1. **Gateway** receives the request. `otelgin` middleware creates a **root span**
   with a fresh **trace ID** (e.g. `a3ce929d0e0e4736…`).

2. **Gateway handler** creates child spans for its internal logic
   (`gateway.get_user`, `gateway.validate_user`, etc.).

3. **Gateway HTTP client** calls `user-service`. Because it uses
   `otelhttp.NewTransport`, the outgoing HTTP request automatically gets a
   `traceparent` header:

   ```
   traceparent: 00-a3ce929d0e0e47360e0e4736a3ce929d-b7bce9deadbeef01-01
                ^^  ^^^^^^^^^^^^^^^^ trace-id ^^^^  ^^ span-id ^^     flags
   ```

4. **User-service** receives the request. Its `otelgin` middleware reads the
   `traceparent` header and extracts the trace ID. Any new span it creates
   uses that same trace ID and parents itself to the gateway's span.

5. All spans — from all three services — are exported via OTLP to the
   **Collector**, then forwarded to **Jaeger**, where they appear as one
   waterfall with a single root.

### 2. Span Hierarchy (complete order flow)

```
HTTP POST /orders                               [gateway-service]
  └─ gateway.create_order
       ├─ gateway.validate_user
       │    └─ http.client.user-service.get_user
       │         └─ GET /internal/users/1       [user-service]
       │              └─ user_service.get_user
       │
       └─ http.client.order-service.create_order
            └─ POST /internal/orders            [order-service]
                 └─ order_service.create_order
                      ├─ order_service.validate_order
                      └─ db.orders.insert       → PostgreSQL
```

Each indented span is a child of the one above. Colours in Jaeger:
- **Blue** — successful spans
- **Red** — spans with `status = ERROR` (error simulations)
- **Wide** — slow spans (slow-product simulation)

---

## Quick Start

### Prerequisites

- Docker ≥ 24 and Docker Compose ≥ 2.20
- 2 GB free RAM

### Run the stack

```bash
cd otel-poc
docker compose up --build
```

First startup takes a few minutes because Go downloads module dependencies
inside each container. Subsequent builds are faster.

Wait until you see log lines like:
```
gateway-service  | {"level":"INFO","msg":"gateway-service listening","port":"8080"}
user-service     | {"level":"INFO","msg":"user-service listening","port":"8081"}
order-service    | {"level":"INFO","msg":"order-service listening","port":"8082"}
```

### Tear down

```bash
docker compose down -v   # -v also removes the postgres volume
```

---

## Endpoints

| Service | URL | Description |
|---------|-----|-------------|
| Gateway | `http://localhost:8080/health` | Liveness check |
| Gateway | `http://localhost:8080/users/:id` | Get user (calls user-service) |
| Gateway | `http://localhost:8080/orders` | Create order (calls order-service + DB) |
| Gateway | `http://localhost:8080/metrics` | Prometheus metrics |
| User | `http://localhost:8081/metrics` | Prometheus metrics |
| Order | `http://localhost:8082/metrics` | Prometheus metrics |
| Jaeger | `http://localhost:16686` | Trace explorer UI |
| Prometheus | `http://localhost:9090` | Metrics query UI |

---

## Sample curl Requests

### 1. Health checks

```bash
curl http://localhost:8080/health
curl http://localhost:8081/health
curl http://localhost:8082/health
```

### 2. Get a user (happy path — trace spans gateway + user-service)

```bash
curl http://localhost:8080/users/1
curl http://localhost:8080/users/2   # triggers slow-lookup simulation
curl http://localhost:8080/users/3
```

### 3. Get a user — error span

```bash
curl http://localhost:8080/users/999
# Returns 404, produces a red error span in Jaeger
```

### 4. Create an order (full distributed trace — all 3 services + DB)

```bash
curl -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "widget", "amount": 49.99}'
```

### 5. Create a slow order (triggers 600 ms simulated slow DB query)

```bash
curl -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 2, "product_name": "slow-product", "amount": 9.99}'
```

Inspect in Jaeger: the `db.orders.insert` span will be ~600 ms wide.

### 6. Simulate an error through the full stack

```bash
curl -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 999, "product_name": "widget", "amount": 10.00}'
# user validation fails, error spans visible in gateway + user-service
```

### 7. List orders for a user (direct to order-service for exploration)

```bash
curl "http://localhost:8082/internal/orders?user_id=1"
```

---

## Inspecting Traces in Jaeger

1. Open **http://localhost:16686**
2. Select a service from the dropdown:
   - `gateway-service`
   - `user-service`
   - `order-service`
3. Click **Find Traces**
4. Click any trace row to see the full waterfall

### What to look for

| Feature | What to do |
|---------|-----------|
| Distributed trace (all 3 services) | `POST /orders`, then open in Jaeger |
| Parent→child spans | Expand the trace tree |
| Error spans (red) | Use `user_id=999` or `product_name` mismatch |
| Slow span | Use `product_name=slow-product` |
| DB spans | Expand the order-service subtree |
| Span attributes | Click any span row |
| Span events | Expand "Logs" section of a span |

---

## Inspecting Metrics in Prometheus

1. Open **http://localhost:9090**
2. Use the expression browser

### Useful queries

```promql
# Request rate to the gateway
rate(gateway_requests_total[1m])

# 95th percentile order processing latency
histogram_quantile(0.95,
  rate(order_service_order_processing_duration_ms_bucket[5m]))

# DB query latency by operation
histogram_quantile(0.99,
  rate(db_query_duration_ms_bucket[5m]))

# Upstream error rate
rate(gateway_upstream_errors_total[1m])

# User lookup cache hit breakdown
rate(user_service_lookups_total[1m])

# OTel collector spans received
rate(otelcol_receiver_accepted_spans[1m])
```

Check **Status → Targets** to verify all scrape targets are UP.

---

## Key OpenTelemetry Concepts Demonstrated

### TracerProvider & MeterProvider

Each service calls `telemetry.Init()` which creates:
- A `TracerProvider` connected to the OTLP exporter (→ Collector → Jaeger)
- A `MeterProvider` using the Prometheus exporter (→ `/metrics` endpoint)

Both are registered globally so library code (otelgin, otelhttp) picks them up.

### Instrumentation Libraries

| Library | What it instruments |
|---------|-------------------|
| `otelgin` | Automatic server span per HTTP request |
| `otelhttp.Transport` | Automatic client span + header injection per outbound HTTP call |
| Manual `tracer.Start()` | Business logic, DB calls, validation |

### Semantic Conventions

DB spans follow the [OpenTelemetry database semantic conventions](https://opentelemetry.io/docs/specs/semconv/database/):
- `db.system = postgresql`
- `db.name = otelpoc`
- `db.operation = INSERT | SELECT`
- `db.sql.table = orders`
- `db.statement` — parameterised query (never interpolated values)

### Sampler

All services use `AlwaysSample()` for 100% trace capture. In production:
```go
// Sample 10% of traces
sdktrace.WithSampler(sdktrace.TraceIDRatioBased(0.1))
```

### Graceful Shutdown

Each service's `main.go` calls `prov.Shutdown(ctx)` on SIGTERM. This flushes
any spans still buffered in the `BatchSpanProcessor`. Without this, the last
few seconds of spans before a deploy would be silently dropped.

---

## Project Structure

```
otel-poc/
├── docker-compose.yml          # full stack definition
├── init.sql                    # DB schema + seed data
├── collector/
│   └── config.yaml             # OTLP→Jaeger + Prometheus pipeline
├── prometheus/
│   └── prometheus.yml          # scrape targets
├── gateway-service/
│   ├── main.go                 # server bootstrap + graceful shutdown
│   ├── handlers/handlers.go    # HTTP handlers + OTel spans
│   ├── internal/telemetry/     # TracerProvider + MeterProvider init
│   ├── go.mod
│   └── Dockerfile
├── user-service/
│   ├── main.go
│   ├── handlers/handlers.go    # slow + error span simulations
│   ├── internal/telemetry/
│   ├── go.mod
│   └── Dockerfile
└── order-service/
    ├── main.go
    ├── handlers/handlers.go    # business logic spans
    ├── internal/
    │   ├── telemetry/
    │   └── db/db.go            # manual DB span instrumentation
    ├── go.mod
    └── Dockerfile
```

---

## Extending the PoC

| Goal | How |
|------|-----|
| Add a new service | Copy user-service structure, register in docker-compose |
| Add a new metric | `meter.Int64Counter(…)` or `meter.Float64Histogram(…)` |
| Add a span event | `span.AddEvent("name", trace.WithAttributes(…))` |
| Add baggage | `baggage.New(…)` + `otel.GetTextMapPropagator().Inject(…)` |
| Use a different backend | Change collector exporters — service code is unchanged |
| Enable gRPC tracing | Use `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc` |
| Add log correlation | Extract `trace.SpanFromContext(ctx).SpanContext()` and add to log fields |
