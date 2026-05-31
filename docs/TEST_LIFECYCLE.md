# Test Lifecycle — What Happens on Every Request

This document traces a single HTTP request from the moment it hits the gateway to the moment
it appears in Jaeger and Prometheus, with exact curl commands, expected responses, log output,
span trees, and PromQL queries to verify each step.

---

## Table of Contents

1. [Prerequisites — Verify Stack Is Healthy](#prerequisites)
2. [Scenario A — Happy Path: Get User](#scenario-a--happy-path-get-user)
3. [Scenario B — Slow Span: Get User 2](#scenario-b--slow-span-get-user-2)
4. [Scenario C — Error Span: User Not Found](#scenario-c--error-span-user-not-found)
5. [Scenario D — Full Order Flow (All 3 Services + DB)](#scenario-d--full-order-flow)
6. [Scenario E — Slow DB Query](#scenario-e--slow-db-query)
7. [Scenario F — Order with Invalid User (Error Propagation)](#scenario-f--order-with-invalid-user)
8. [Reading the OTel Collector Logs](#reading-the-otel-collector-logs)
9. [Prometheus Queries Reference](#prometheus-queries-reference)
10. [Lifecycle Summary Diagram](#lifecycle-summary-diagram)

---

## Prerequisites

Before running any scenario, confirm every service is alive:

```bash
curl -s http://localhost:8080/health | jq
curl -s http://localhost:8081/health | jq
curl -s http://localhost:8082/health | jq
```

Expected response from each:
```json
{ "status": "ok", "service": "<service-name>" }
```

Check Prometheus targets — all must show **State: UP**:
```
http://localhost:9090  →  Status → Targets
```

Check Jaeger is reachable:
```
http://localhost:16686
```

---

## Scenario A — Happy Path: Get User

### The Request

```bash
curl -s http://localhost:8080/users/1 | jq
```

Expected response:
```json
{
  "id": "1",
  "name": "Alice Johnson",
  "email": "alice@example.com"
}
```

---

### What Happens — Step by Step

```
1. curl → gateway-service :8080
2. gateway-service → user-service :8081
3. user-service returns user from in-memory store
4. gateway-service returns the response to curl
5. Both services flush spans → OTel Collector → Jaeger
6. Both services update metrics → Prometheus scrapes /metrics
```

#### Step 1: Gateway receives the request

`otelgin` middleware intercepts the request **before** your handler runs. It:
- Checks for an incoming `traceparent` header (none from curl — this is the root)
- Generates a fresh 128-bit **Trace ID** (e.g. `4bf92f3577b34da6a3ce929d`)
- Creates the **root span**: `GET /users/:id`
- Sets `SpanKind = SERVER`

#### Step 2: Gateway handler creates child spans

Inside `handlers.GetUser()`:

```
root span: GET /users/:id                  [otelgin, automatic]
  └─ gateway.get_user                      [manual, tracer.Start()]
       └─ http.client.user-service.get_user [manual, SpanKindClient]
```

Before making the HTTP call to user-service, `otelhttp.NewTransport` injects:
```
traceparent: 00-4bf92f3577b34da6a3ce929d-<span-id>-01
```
into the outgoing request headers.

#### Step 3: User-service receives the request

`otelgin` reads the `traceparent` header and extracts the Trace ID. It creates:
```
GET /internal/users/:id                    [otelgin, automatic, SpanKindServer]
  └─ user_service.get_user                 [manual, tracer.Start()]
```

Both spans share the **same Trace ID** as the gateway spans.

#### Step 4: Spans are exported

Each service's `BatchSpanProcessor` holds spans in memory and flushes them every 1 second
(or when 512 accumulate) via OTLP gRPC to the Collector on port 4317.
The Collector forwards them to Jaeger.

---

### What to Observe in Jaeger

1. Open **http://localhost:16686**
2. Service dropdown → `gateway-service` → **Find Traces**
3. Click the trace for `GET /users/:id`

**Span tree you should see:**

```
GET /users/:id                              gateway-service   ~5 ms total
  └─ gateway.get_user                       gateway-service   ~4 ms
       └─ http.client.user-service.get_user gateway-service   ~3 ms  [CLIENT]
            └─ GET /internal/users/:id      user-service      ~2 ms  [SERVER]
                 └─ user_service.get_user   user-service      ~1 ms
```

**Click `gateway.get_user` → Tags tab. Verify these attributes exist:**

| Attribute | Value |
|-----------|-------|
| `user.id` | `1` |
| `http.route` | `/users/:id` |
| `user.name` | `Alice Johnson` (added after the upstream call returns) |

**Click `http.client.user-service.get_user` → Tags tab:**

| Attribute | Value |
|-----------|-------|
| `http.method` | `GET` |
| `http.url` | `http://user-service:8081/internal/users/1` |
| `peer.service` | `user-service` |
| `http.status_code` | `200` |

**Click `user_service.get_user` → Tags tab:**

| Attribute | Value |
|-----------|-------|
| `user.id` | `1` |
| `user.name` | `Alice Johnson` |
| `user.email` | `alice@example.com` |

**Notice:** The `service.name` tag on each span shows which process produced it — even though they share one Trace ID.

---

### What to Observe in Service Logs

```bash
docker compose logs gateway-service --tail=5
docker compose logs user-service --tail=5
```

Gateway log line (JSON):
```json
{
  "level": "INFO",
  "time": "...",
  "msg": "...",
  "trace_id": "4bf92f3577b34da6a3ce929d",
  "span_id": "00f067aa0ba902b7"
}
```

The `trace_id` in the log matches the Trace ID in Jaeger — this is log-trace correlation. You can paste the `trace_id` into Jaeger's search to jump directly to the trace.

---

### What to Observe in Prometheus

Run this in http://localhost:9090:

```promql
# Confirm a request was recorded
gateway_requests_total{endpoint="/users/:id"}
```

Also check the user-service lookup counter:
```promql
user_service_lookups_total{user_id="1"}
```

And lookup latency (should be fast — no sleep for user 1):
```promql
user_service_lookup_duration_ms_bucket
```

---

## Scenario B — Slow Span: Get User 2

### The Request

```bash
curl -s http://localhost:8080/users/2 | jq
```

Expected response:
```json
{
  "id": "2",
  "name": "Bob Smith",
  "email": "bob@example.com"
}
```

The request succeeds but takes ~250 ms longer than usual.

---

### What Happens Differently

Inside `user-service/handlers/handlers.go`, user ID `"2"` triggers a special path:

```go
if userID == "2" {
    ctx, slowSpan := tracer.Start(ctx, "user_service.slow_lookup_simulation", ...)
    time.Sleep(250 * time.Millisecond)
    slowSpan.AddEvent("cache miss resolved, returning from database")
    slowSpan.End()
}
```

A child span is created, a 250 ms sleep runs inside it, a span **Event** is added, then it ends.

---

### What to Observe in Jaeger

Span tree:
```
GET /users/:id                               gateway-service   ~260 ms total
  └─ gateway.get_user                        gateway-service   ~258 ms
       └─ http.client.user-service.get_user  gateway-service   ~255 ms
            └─ GET /internal/users/:id       user-service      ~252 ms
                 └─ user_service.get_user    user-service      ~251 ms
                      └─ user_service.slow_lookup_simulation   ~250 ms  ← WIDE BAR
```

**The `user_service.slow_lookup_simulation` span is ~250 ms wide** — visually distinct from the instant sibling spans.

**Click that span → Tags tab:**

| Attribute | Value |
|-----------|-------|
| `simulation.reason` | `cache miss` |
| `simulation.delay_ms` | `250` |

**Click → Logs tab (span events):**

| Timestamp | Event |
|-----------|-------|
| +250ms | `cache miss resolved, returning from database` |

**Key insight:** The waterfall makes it immediately obvious that the delay is inside user-service, not the network or the gateway. Without tracing, you'd only see the total 260 ms at the gateway level and have no idea where it came from.

---

### What to Observe in Prometheus

Lookup latency histogram will now have a bucket at >250 ms:

```promql
# Latency distribution for user lookups labelled by result
histogram_quantile(0.99,
  rate(user_service_lookup_duration_ms_bucket[5m]))
```

Run the request 5–10 times to populate the histogram, then compare user 1 vs user 2 percentiles.

---

## Scenario C — Error Span: User Not Found

### The Request

```bash
curl -s http://localhost:8080/users/999 | jq
```

Expected response (HTTP 404):
```json
{ "error": "user not found" }
```

---

### What Happens

In `gateway-service/handlers/handlers.go`:
```go
if userID == "999" {
    err := fmt.Errorf("user %s not found (simulated error)", userID)
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
    h.upstreamErrors.Add(ctx, 1, ...)
    c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
    return
}
```

The gateway short-circuits — it never calls user-service. The error is recorded directly on the gateway span.

---

### What to Observe in Jaeger

Span tree:
```
GET /users/:id              gateway-service   [ERROR — red]
  └─ gateway.get_user       gateway-service   [ERROR — red]
```

Notice there are **only 2 spans** (no user-service spans) because the gateway returned early.

**Click `gateway.get_user` → Tags tab:**

| Attribute | Value |
|-----------|-------|
| `user.id` | `999` |
| `otel.status_code` | `ERROR` |
| `error` | `true` |

**Click → Logs tab (span events):**

| Field | Value |
|-------|-------|
| `event` | `exception` |
| `exception.message` | `user 999 not found (simulated error)` |
| `exception.type` | `*errors.errorString` |
| `exception.stacktrace` | (full Go stack trace) |

`span.RecordError(err)` automatically captures the exception type, message, and stack trace as a span event named `exception` — this is the OpenTelemetry exception semantic convention.

---

### What to Observe in Prometheus

```promql
# Should now show at least 1 error attributed to service "gateway"
gateway_upstream_errors_total{service="gateway"}
```

---

## Scenario D — Full Order Flow

This scenario touches all three services and the PostgreSQL database.

### The Request

```bash
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "widget", "amount": 49.99}' | jq
```

Expected response (HTTP 201):
```json
{
  "id": 1,
  "user_id": 1,
  "product_name": "widget",
  "amount": 49.99,
  "status": "pending",
  "created_at": "2026-05-09T..."
}
```

---

### What Happens — Step by Step

```
1. curl → gateway :8080  POST /orders
2. gateway validates the JSON body
3. gateway calls user-service to validate user 1 exists  (GET /internal/users/1)
4. gateway calls order-service                           (POST /internal/orders)
5. order-service validates the order fields
6. order-service inserts the row into PostgreSQL
7. order-service returns the created order
8. gateway returns HTTP 201 to curl
```

---

### Full Span Tree in Jaeger

```
POST /orders                                  gateway-service   ~15 ms total
  └─ gateway.create_order                     gateway-service   ~14 ms
       ├─ gateway.validate_user               gateway-service   ~5 ms
       │    └─ http.client.user-service.get_user   gateway-service  [CLIENT]
       │         └─ GET /internal/users/:id   user-service      ~3 ms  [SERVER]
       │              └─ user_service.get_user user-service      ~2 ms
       │
       └─ http.client.order-service.create_order  gateway-service  [CLIENT]
            └─ POST /internal/orders          order-service     ~8 ms  [SERVER]
                 └─ order_service.create_order order-service    ~7 ms
                      ├─ order_service.validate_order           ~0 ms
                      └─ db.orders.insert                       ~5 ms  [CLIENT → PostgreSQL]
```

**10 spans across 3 processes, all sharing 1 Trace ID.**

---

### What to Observe in Jaeger — Span by Span

**`gateway.create_order`**
- Tags: `order.user_id`, `order.product_name`, `order.amount`
- Events: `order created successfully` with `order.id` (added after the DB write completes)

**`gateway.validate_user`**
- A pure business-logic span — no network call of its own; it wraps `callUserService()`
- Tags: `user.id = 1`
- Events: `user validated`

**`http.client.order-service.create_order`**
- Tags: `http.method = POST`, `peer.service = order-service`, `http.status_code = 201`

**`order_service.create_order`**
- Tags: `order.user_id = 1`, `order.product_name = widget`, `order.amount = 49.99`
- Tags (added after DB write): `order.id = 1`
- Events: `order persisted` with `order.id` and `order.status = pending`

**`order_service.validate_order`**
- Near-zero duration — validates that user_id > 0, product_name is non-empty, amount > 0
- Events: `validation passed`

**`db.orders.insert`** ← most interesting leaf span
- Tags:

  | Attribute | Value |
  |-----------|-------|
  | `db.system` | `postgresql` |
  | `db.name` | `otelpoc` |
  | `db.operation` | `INSERT` |
  | `db.sql.table` | `orders` |
  | `db.statement` | `INSERT INTO orders (user_id, product_name, amount, status) VALUES ($1, $2, $3, $4) RETURNING id, created_at` |
  | `order.id` | `1` |

  The `db.statement` uses `$1, $2` placeholders — never interpolated values — so no PII leaks into traces.

---

### What to Observe in Prometheus

```promql
# Orders created (labelled by product name)
order_service_orders_created_total{product_name="widget"}

# End-to-end order processing time (P95)
histogram_quantile(0.95,
  rate(order_service_order_processing_duration_ms_bucket[5m]))

# DB INSERT count
db_queries_total{db_operation="INSERT", db_sql_table="orders"}

# DB INSERT latency (P99)
histogram_quantile(0.99,
  rate(db_query_duration_ms_bucket{db_operation="INSERT"}[5m]))
```

---

### Verify the Row in PostgreSQL

Connect with any Postgres client using the connection string `postgresql://postgres:postgres@localhost:5432/otelpoc`, or use psql via Docker:

```bash
docker compose exec postgres psql -U postgres -d otelpoc -c "SELECT * FROM orders;"
```

Expected output:
```
 id | user_id | product_name | amount | status  |         created_at
----+---------+--------------+--------+---------+----------------------------
  1 |       1 | widget       |  49.99 | pending | 2026-05-09 16:04:21.123+00
```

---

## Scenario E — Slow DB Query

### The Request

```bash
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "slow-product", "amount": 9.99}' | jq
```

The request succeeds (HTTP 201) but takes ~600 ms longer than a normal order.

---

### What Happens in the DB Layer

Inside `order-service/internal/db/db.go`, the product name `"slow-product"` triggers:

```go
if o.ProductName == "slow-product" {
    span.AddEvent("slow_query_simulation_start", trace.WithAttributes(
        attribute.String("reason", "unindexed full-table scan (simulated)"),
    ))
    time.Sleep(600 * time.Millisecond)
    span.AddEvent("slow_query_simulation_end")
}
```

The sleep happens **inside** the `db.orders.insert` span, between span creation and the actual SQL call.

---

### What to Observe in Jaeger

```
POST /orders                                  gateway-service   ~620 ms total
  └─ gateway.create_order                     gateway-service   ~618 ms
       ├─ gateway.validate_user               gateway-service   ~5 ms
       │    └─ ...
       └─ http.client.order-service.create_order   gateway-service
            └─ POST /internal/orders          order-service     ~612 ms
                 └─ order_service.create_order order-service    ~611 ms
                      ├─ order_service.validate_order           ~0 ms
                      └─ db.orders.insert                       ~607 ms  ← VERY WIDE
```

**Click `db.orders.insert` → Logs tab (span events):**

| Timestamp | Event | Attributes |
|-----------|-------|-----------|
| T+0ms | `slow_query_simulation_start` | `reason = unindexed full-table scan (simulated)` |
| T+600ms | `slow_query_simulation_end` | — |

**Key insight:** Without tracing, you'd see a ~620 ms gateway response and have no idea if the slowness was in validation, the user lookup, or the DB write. The waterfall shows it is entirely inside `db.orders.insert`, and the span events pinpoint the exact sub-operation.

---

### What to Observe in Prometheus

Run several normal orders and several slow ones, then compare:

```promql
# Latency split by result label (ok vs slow)
histogram_quantile(0.99,
  rate(db_query_duration_ms_bucket[5m]))
```

The P99 bucket will be dominated by the slow inserts. Without the histogram you'd only have a rate — no latency distribution.

---

## Scenario F — Order with Invalid User

### The Request

```bash
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 999, "product_name": "widget", "amount": 10.00}' | jq
```

Expected response (HTTP 400):
```json
{ "error": "invalid user: user-service returned HTTP 404" }
```

---

### What Happens

```
1. gateway.create_order begins
2. gateway.validate_user calls user-service for user 999
3. user-service returns 404 (user not found)
4. gateway records the error on gateway.validate_user and gateway.create_order
5. gateway returns 400 — order is never sent to order-service
```

---

### What to Observe in Jaeger

```
POST /orders                                   gateway-service  [ERROR]
  └─ gateway.create_order                      gateway-service  [ERROR]
       └─ gateway.validate_user                gateway-service  [ERROR]
            └─ http.client.user-service.get_user   gateway-service  [ERROR]
                 └─ GET /internal/users/:id    user-service     [ERROR]
                      └─ user_service.get_user user-service     [ERROR]
```

**All spans are red.** The error starts at `user_service.get_user` and propagates up through every parent span.

**Click `user_service.get_user` → Tags tab:**
- `otel.status_code = ERROR`
- `error.type = UserNotFound`

**Click `gateway.create_order` → Tags tab:**
- `otel.status_code = ERROR`

Notice `http.client.order-service.create_order` does **not** appear in the tree — the gateway exited before making that call, which you can confirm by the absence of that span.

---

### What to Observe in Prometheus

```promql
# Upstream errors at the gateway attributed to user-service
gateway_upstream_errors_total{service="user-service"}
```

---

## Reading the OTel Collector Logs

The Collector's `debug` exporter writes every received span to its stdout. This is the most direct way to confirm your service is actually emitting spans.

```bash
docker compose logs otel-collector | grep -A 5 "gateway.create_order"
```

Sample output:
```
2026-05-09T16:04:21.500Z info  Traces   {"resource spans": 1, "spans": 3}
ScopeName: gateway-service
SpanName : gateway.create_order
TraceID  : 4bf92f3577b34da6a3ce929d0e0e4736
SpanID   : 9a3522d5e3875d3c
ParentSpanID: 00f067aa0ba902b7
Status   : Ok
Attributes:
     -> order.user_id: Str(1)
     -> order.product_name: Str(widget)
     -> order.amount: Double(49.99)
```

Useful log filters:
```bash
# See only trace-related log lines
docker compose logs otel-collector | grep "ScopeName"

# Count spans received per flush
docker compose logs otel-collector | grep "resource spans"

# Watch live as you fire requests
docker compose logs -f otel-collector | grep "SpanName"
```

---

## Prometheus Queries Reference

Open http://localhost:9090 and run these queries after generating traffic with the scenarios above.

### Gateway Metrics

```promql
# Request rate by endpoint
rate(gateway_requests_total[1m])

# Upstream error rate
rate(gateway_upstream_errors_total[1m])
```

### User Service Metrics

```promql
# Lookup rate
rate(user_service_lookups_total[1m])

# Lookup latency P50 / P95 / P99
histogram_quantile(0.50, rate(user_service_lookup_duration_ms_bucket[5m]))
histogram_quantile(0.95, rate(user_service_lookup_duration_ms_bucket[5m]))
histogram_quantile(0.99, rate(user_service_lookup_duration_ms_bucket[5m]))
```

### Order Service Metrics

```promql
# Orders created per minute
rate(order_service_orders_created_total[1m])

# Order processing latency P95
histogram_quantile(0.95,
  rate(order_service_order_processing_duration_ms_bucket[5m]))

# DB query rate by operation
rate(db_queries_total[1m])

# DB query latency P99
histogram_quantile(0.99, rate(db_query_duration_ms_bucket[5m]))
```

### OTel Collector Self-Metrics

```promql
# Spans received per second (confirms services are emitting)
rate(otelcol_receiver_accepted_spans_total[1m])

# Spans exported per second (confirms Jaeger is receiving)
rate(otelcol_exporter_sent_spans_total[1m])

# Export failures (non-zero = problem between Collector and Jaeger)
rate(otelcol_exporter_send_failed_spans_total[1m])
```

### Collector vs Jaeger sanity check

If spans appear in `otelcol_receiver_accepted_spans_total` but NOT in Jaeger, the problem is between the Collector and Jaeger. If `otelcol_receiver_accepted_spans_total` is zero after a request, the problem is between your service and the Collector.

---

## Lifecycle Summary Diagram

```
curl → gateway :8080
│
│   [otelgin] creates root span
│   traceid = abc123, spanid = span001
│
├── [handler] creates child span gateway.create_order
│     traceid = abc123, spanid = span002, parent = span001
│
├── [handler] creates child span gateway.validate_user
│     traceid = abc123, spanid = span003, parent = span002
│
│   [otelhttp.Transport] injects header:
│     traceparent: 00-abc123-span003-01
│
├──► user-service :8081
│     │
│     │   [otelgin] reads header, creates SERVER span
│     │     traceid = abc123, spanid = span004, parent = span003
│     │
│     └── [handler] creates child span user_service.get_user
│           traceid = abc123, spanid = span005, parent = span004
│
│   [otelhttp.Transport] injects header:
│     traceparent: 00-abc123-span002-01
│
├──► order-service :8082
│     │
│     │   [otelgin] reads header, creates SERVER span
│     │     traceid = abc123, spanid = span006, parent = span002
│     │
│     ├── [handler] order_service.create_order
│     │     spanid = span007, parent = span006
│     │
│     ├── [handler] order_service.validate_order
│     │     spanid = span008, parent = span007
│     │
│     └── [db] db.orders.insert
│           spanid = span009, parent = span007, kind = CLIENT
│               │
│               └──► PostgreSQL (no spans — driver is not instrumented)
│
│ BatchSpanProcessor buffers all spans in memory
│ Every ~1s: service → OTel Collector :4317 (OTLP gRPC)
│
├──► OTel Collector
│     ├── memory_limiter (drop if RAM > 256 MiB)
│     ├── batch (group into efficient payloads)
│     ├──► Jaeger :4317 (traces)    → visible at localhost:16686
│     └──► Prometheus :8889 (metrics pull endpoint)
│               ▲
│               └── Prometheus :9090 scrapes every 10 s
│
└── Each service also exposes /metrics directly
    Prometheus scrapes gateway:8080, user:8081, order:8082
```

---

## Quick Checklist for Each Test

| After each curl | Check here | Look for |
|-----------------|-----------|---------|
| Jaeger | http://localhost:16686 | Trace with correct span tree |
| Jaeger span tags | Click any span → Tags | `user.id`, `db.statement`, `http.status_code`, etc. |
| Jaeger span events | Click any span → Logs | `order persisted`, `validation passed`, exception details |
| Prometheus counter | http://localhost:9090 | Counter incremented by 1 |
| Prometheus histogram | http://localhost:9090 | Bucket matching observed latency |
| Service logs | `docker compose logs <svc>` | JSON log with `trace_id` field |
| Collector logs | `docker compose logs otel-collector` | `SpanName` lines matching your request |
| PostgreSQL | `docker compose exec postgres psql ...` | Row in `orders` table (order scenarios only) |
