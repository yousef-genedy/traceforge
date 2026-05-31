# TraceForge — Deep-Dive Learning Guide

This guide walks you through every component in the stack, explains why each piece exists, and gives you concrete exercises to build real intuition for distributed observability.

---

## Table of Contents

1. [What This Project Teaches](#what-this-project-teaches)
2. [Architecture Overview](#architecture-overview)
3. [Component Reference](#component-reference)
   - [PostgreSQL](#postgresql)
   - [OpenTelemetry Collector](#opentelemetry-collector)
   - [Jaeger](#jaeger)
   - [Prometheus](#prometheus)
   - [Grafana](#grafana)
   - [Loki](#loki)
   - [gateway-service](#gateway-service)
   - [user-service](#user-service)
   - [order-service](#order-service)
4. [How to Run the Stack](#how-to-run-the-stack)
5. [How Distributed Tracing Actually Works](#how-distributed-tracing-actually-works)
6. [Guided Exercises](#guided-exercises)
7. [Testing Strategy](#testing-strategy)
8. [Key Files and Where to Read First](#key-files-and-where-to-read-first)
9. [Extending This Project](#extending-this-project)

---

## What This Project Teaches

| Concept | Where it's demonstrated |
|---------|------------------------|
| Initialising the OTel SDK (TracerProvider + MeterProvider + LoggerProvider) | `*/internal/telemetry/telemetry.go` |
| Automatic HTTP server instrumentation | `otelgin.Middleware(...)` in every `main.go` |
| Automatic HTTP client instrumentation + header injection | `otelhttp.NewTransport(...)` in `gateway-service/handlers` |
| Manual child spans for business logic | `tracer.Start(ctx, "span-name")` throughout |
| Span attributes, events, and error recording | Every handler |
| Database span instrumentation (OTel semantic conventions) | `order-service/internal/db/db.go` |
| Custom metrics (counters, histograms) | Every `handlers/` package |
| Structured logging with trace_id/span_id correlation | `*/internal/logging/logging.go` |
| OTel log bridge (slog → OTel LoggerProvider → Collector → Loki) | `logging.NewLogger(serviceName)` |
| The OTel Collector pipeline (receive → process → export, 3 signals) | `collector/config.yaml` |
| Prometheus scraping + PromQL | `prometheus/prometheus.yml` |
| Grafana dashboards (RED method: rate, errors, duration) | `grafana/provisioning/dashboards/` |
| Log exploration with LogQL in Grafana Explore | Loki datasource in Grafana |
| Trace-to-log and log-to-trace correlation | Jaeger `tracesToLogsV2` + Loki derived fields |
| Graceful shutdown that flushes buffered spans | Every `main.go` |
| W3C TraceContext propagation across process boundaries | `gateway-service/handlers/handlers.go` |

---

## Architecture Overview

```
HTTP Client (curl)
        │
        ▼
  gateway-service :8080          ← public entry point
        │                │
        │ HTTP +          │ HTTP +
        │ traceparent     │ traceparent
        ▼                ▼
  user-service     order-service
  :8081            :8082
  (in-memory)          │
                       │ SQL
                       ▼
                   PostgreSQL :5432

Telemetry pipeline (every service pushes all three signals via OTLP gRPC to port 4317):

  gateway-service ─┐
  user-service    ─┤──► OTel Collector :4317
  order-service   ─┘         │
                              ├── traces pipeline  ──► Jaeger :16686
                              ├── metrics pipeline ──► Prometheus :8889 (scrape endpoint)
                              │                              ▲
                              │                              │ scrape /metrics every 10 s
                              │                     Prometheus :9090
                              │                              ▲
                              │                              │ queries
                              │                     Grafana :3000 (dashboards + Explore)
                              └── logs pipeline ──► Loki :3100
                                                             ▲
                                                             │ queries
                                                    Grafana :3000 (Explore)
```

The Collector is the central telemetry hub. Services never talk to Jaeger, Prometheus, or Loki directly — they send everything to the Collector via OTLP and let it fan out. Adding a new backend (e.g. Grafana Tempo) means changing only `collector/config.yaml` with zero code changes in the services.

---

## Component Reference

### PostgreSQL

**File:** `db/init.sql`, `docker-compose.yml`

PostgreSQL stores orders. It is initialised from `db/init.sql` when the container first starts, which creates two tables and seeds five users:

| ID | Name | Tier |
|----|------|------|
| 1 | Alice Johnson | premium |
| 2 | Bob Smith | standard |
| 3 | Charlie Brown | standard |
| 4 | Diana Prince | premium |
| 5 | Evan Rogers | standard |

The `orders` table has a foreign-key constraint on `user_id` referencing `users(id)`. Attempting to insert an order with a nonexistent `user_id` produces a real DB error, which the order-service captures as an error span.

**Key design decisions:**
- `user-service` mirrors these five users in memory so you can explore both in-memory and DB instrumentation patterns.
- The DB is only reachable from within the Docker network. The order-service is the only service with `DB_*` environment variables set.

---

### OpenTelemetry Collector

**File:** `collector/config.yaml`

The Collector is a vendor-agnostic process that receives, processes, and exports telemetry. Think of it as a router for all three observability signals.

```
otlp receiver (gRPC :4317, HTTP :4318)
        │
        ▼
  memory_limiter processor   ← drops data if RAM usage spikes (512 MiB limit)
        │
        ▼
  resource processor         ← stamps deployment.environment=development, project=traceforge
        │
        ▼
  batch processor            ← buffers 1024 items or 2 s, whichever comes first
        │
        ├──► debug exporter         (logs a summary to collector stdout)
        ├──► otlp/jaeger exporter   (forwards traces to Jaeger via OTLP gRPC)
        ├──► prometheus exporter    (exposes /metrics on :8889 for Prometheus to scrape)
        └──► loki exporter          (pushes log records to Loki HTTP push API :3100)
```

Three independent pipelines share the same receiver and processor chain:

| Pipeline | Receiver | Processors | Exporter |
|----------|----------|------------|---------|
| `traces` | otlp | memory_limiter, resource, batch | debug, otlp/jaeger |
| `metrics` | otlp | memory_limiter, resource, batch | debug, prometheus |
| `logs` | otlp | memory_limiter, resource, batch | debug, loki |

**Processors explained:**
- `memory_limiter`: Prevents the Collector from OOM-crashing under traffic spikes. Checks RSS every second and back-pressures the pipeline if it exceeds `limit_mib: 512`.
- `resource`: Injects cross-cutting attributes (`deployment.environment`, `project`) that aren't set per-service. Every trace, metric, and log record gets these.
- `batch`: Groups telemetry items into batches before exporting. Without this, every span would be a separate gRPC call — expensive at scale.

**Loki exporter specifics:**
- `default_labels_enabled.job: true` maps each service's `service.name` attribute to the Loki `job` label. This is the idiomatic Loki label for "which service produced this log".
- `default_labels_enabled.level: true` maps OTel log severity to the Loki `level` label.
- The result: every log stream in Loki is identified by `{job="<service-name>", level="<severity>"}`.

**Ports exposed to host:**
| Port | Purpose |
|------|---------|
| 4317 | OTLP gRPC (services → collector) |
| 4318 | OTLP HTTP (alternative protocol) |
| 8889 | Prometheus scrape endpoint |
| 13133 | Health check |

---

### Jaeger

**File:** `docker-compose.yml`

Jaeger stores and visualises traces. This stack uses the `all-in-one` image — fine for development.

`SPAN_STORAGE_TYPE=memory` means traces are stored in RAM and lost when the container restarts.

**What you see in the Jaeger UI:**
- Each row in the trace list is one request (identified by a unique trace ID).
- Clicking a trace opens the **waterfall view**: a timeline of all spans across all services.
- Span colour: blue = OK, red = error (`span.SetStatus(codes.Error, ...)`).
- Span width corresponds to duration — wide bars = slow operations.
- Clicking any span shows its **Tags** (attributes), **Process** (service.name, version), and **Logs** (span events).
- The **Logs** button on a span jumps to Grafana Explore pre-filtered to that service + trace ID.

---

### Prometheus

**File:** `prometheus/prometheus.yml`

Prometheus is a **pull-based** metrics system. Every 10 seconds it HTTP-GETs `/metrics` from each target and stores the result as a time series.

Scrape targets:

| Job | Target | What it exposes |
|-----|--------|----------------|
| `gateway-service` | `gateway-service:8080/metrics` | Request counters, upstream error counters, latency histograms |
| `user-service` | `user-service:8081/metrics` | Lookup counters and latency histograms |
| `order-service` | `order-service:8082/metrics` | Order counters, failure counters, processing duration, DB metrics |
| `otel-collector` | `otel-collector:8889/metrics` | Collector internal metrics (spans received, export errors) |
| `prometheus` | `localhost:9090/metrics` | Prometheus self-monitoring |

The `/metrics` endpoint on each service is served by `promhttp.Handler()`, which reads from the default Prometheus registry. OTel instruments register themselves into that same registry when you use `prometheus.New()` as the MeterProvider reader.

---

### Grafana

**Files:** `grafana/provisioning/datasources/datasources.yml`, `grafana/provisioning/dashboards/`

Grafana is the unified UI for all three signal types. It is pre-provisioned with three datasources and one dashboard.

**Datasources provisioned automatically:**

| Name | UID | Type | URL | Default |
|------|-----|------|-----|---------|
| Prometheus | `prometheus` | prometheus | http://prometheus:9090 | yes |
| Loki | `loki` | loki | http://loki:3100 | no |
| Jaeger | `jaeger` | jaeger | http://jaeger:16686 | no |

**Trace-to-log wiring:** The Jaeger datasource is configured with `tracesToLogsV2` pointing at the Loki datasource. When you view a span in Jaeger and click **Logs**, Grafana opens an Explore view pre-filtered with:
```logql
{job="<service-name>"} |= `<traceId>`
```

**Log-to-trace wiring:** The Loki datasource has a **derived field** that matches `"trace_id":"<32hexchars>"` in every log line and creates a clickable **View in Jaeger** link directly to the trace waterfall.

**Pre-provisioned dashboard — TraceForge Platform Overview:**
- Auto-refreshes every 30 seconds
- Row 1 "Service Health": `up{job=~"gateway-service|order-service|user-service"}`
- Row 2 "Throughput": request rate by endpoint, orders created per second
- Row 3 "Errors": upstream errors by service, failed orders by reason
- Row 4 "Latency": gateway P50/P95/P99, DB query P50/P95
- Row 5 "Business Metrics": total orders, lookups, current error rate, P95 latency

---

### Loki

**File:** `loki/config.yaml`

Loki is a log aggregation system designed to be cost-effective at scale. Unlike Elasticsearch, it indexes only labels (not the full log body), so storage stays lightweight.

**How logs get into Loki in this stack:**
```
Go service
  └─ slog.InfoContext(ctx, "order created", "order_id", 42)
       └─ logging.fanHandler
            ├─ JSON handler → stdout (docker compose logs)
            └─ otelslog bridge → OTel LoggerProvider
                  └─ OTLP gRPC → Collector
                        └─ loki exporter → Loki :3100
```

**Every log line includes:**
- `trace_id` — extracted from the active OTel span context
- `span_id` — extracted from the active OTel span context
- `service.name` — becomes the `job` Loki label
- All business attributes passed to `slog.InfoContext`

**Querying in Grafana Explore:**

| LogQL pattern | What it finds |
|--------------|---------------|
| `{job="order-service"}` | All logs from order-service |
| `{job=~".+"} \| json \| level="error"` | Error logs across all services |
| `{job=~".+"} \|= "4bf92f3577b34da6a3ce929d0e0e4736"` | All logs for one trace ID |
| `{job="order-service"} \|= "payment declined"` | Payment failure logs |
| `{job="order-service"} \| json \| order_id="42"` | Logs for a specific order |

**Configuration highlights (`loki/config.yaml`):**
- Single-binary mode: all Loki components in one process
- `schema v13` + `tsdb` store: the recommended index format for Loki 3.x
- `unordered_writes: true`: accepts slightly out-of-order log records without rejecting them — useful when services restart and replay buffered records
- Filesystem storage under `/loki` mapped to the `loki_data` Docker volume

---

### gateway-service

**Files:** `gateway-service/main.go`, `gateway-service/handlers/handlers.go`, `gateway-service/internal/telemetry/telemetry.go`, `gateway-service/internal/logging/logging.go`

The gateway is the single public entry point. It receives HTTP requests, fans them out to internal services, and returns a combined response.

**Routes:**

| Method | Path | What it does |
|--------|------|-------------|
| GET | `/health` | Liveness probe — no tracing |
| GET | `/users/:id` | Proxies to user-service |
| POST | `/orders` | Validates user, then creates order via order-service |
| GET | `/orders?user_id=N` | Fetches orders for a user via order-service |
| GET | `/metrics` | Prometheus metrics endpoint |

**How OTel is initialised (`telemetry.Init`):**

1. Creates a `resource.Resource` describing this process (`service.name`, `service.version`, host, PID).
2. Dials the Collector via gRPC and creates OTLP trace and log exporters.
3. Wraps the trace exporter in a `BatchSpanProcessor` and creates a `TracerProvider`.
4. Creates a Prometheus metrics exporter and a `MeterProvider`.
5. Creates a `BatchLogProcessor` + `LoggerProvider`, registers it as the global logger provider.
6. Registers all three providers as globals.
7. Sets the W3C TraceContext propagator as the global propagator.

**Logger initialisation order in `main.go` (order matters):**
```go
// 1. Bootstrap logger (for startup errors before OTel is available)
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

// 2. Init OTel — this sets the global LoggerProvider
prov, err := telemetry.Init(ctx, serviceName, otlpEndpoint)

// 3. Replace with production logger that uses the OTel bridge
slog.SetDefault(logging.NewLogger(serviceName))
```

If you call `logging.NewLogger` before `telemetry.Init`, the `otelslog` bridge attaches to a no-op LoggerProvider and logs never reach Loki.

**How spans propagate outward:**
```
otelgin middleware creates root span
        │
        └─ gateway.get_user (manual span in handler)
                │
                └─ http.client GET user-service /internal/users/:id
                        │
                        │   otelhttp.Transport injects traceparent header
                        │
                        ▼
                   user-service receives request with traceparent
```

**Custom metrics:**
- `gateway.requests.total` — Int64Counter, labelled by endpoint and method
- `gateway.request.duration_ms` — Float64Histogram of request latency
- `gateway.upstream.errors.total` — Int64Counter, labelled by upstream service

---

### user-service

**Files:** `user-service/main.go`, `user-service/handlers/handlers.go`, `user-service/internal/telemetry/telemetry.go`, `user-service/internal/logging/logging.go`

The user-service is an internal service (not reachable directly from outside). It serves user lookups from an in-memory map.

**Routes:**

| Method | Path | What it does |
|--------|------|-------------|
| GET | `/health` | Liveness probe |
| GET | `/internal/users/:id` | Returns user record from in-memory store |
| GET | `/internal/users` | Lists all users |
| GET | `/metrics` | Prometheus metrics endpoint |

**In-memory store** mirrors the five users seeded in `db/init.sql`, with a `Tier` field:
```go
var store = map[string]User{
    "1": {ID: "1", Name: "Alice Johnson",  Email: "alice@example.com",   Tier: "premium"},
    "2": {ID: "2", Name: "Bob Smith",      Email: "bob@example.com",     Tier: "standard"},
    "3": {ID: "3", Name: "Charlie Brown",  Email: "charlie@example.com", Tier: "standard"},
    "4": {ID: "4", Name: "Diana Prince",   Email: "diana@example.com",   Tier: "premium"},
    "5": {ID: "5", Name: "Evan Rogers",    Email: "evan@example.com",    Tier: "standard"},
}
```

**Built-in simulations:**
- `GET /internal/users/2` → adds a 200 ms `time.Sleep` inside a span called `user_service.cache_miss_simulation`. In Jaeger this appears as a visibly wide span, demonstrating a cache miss scenario.
- `GET /internal/users/999` → records an error on the span and returns 404.

**Custom metrics:**
- `user_service.lookups.total` — Int64Counter per lookup
- `user_service.lookup.duration_ms` — Float64Histogram of lookup latency, labelled by result (hit/miss)
- `user_service.not_found.total` — Int64Counter for 404s

---

### order-service

**Files:** `order-service/main.go`, `order-service/handlers/handlers.go`, `order-service/internal/db/db.go`, `order-service/internal/telemetry/telemetry.go`, `order-service/internal/logging/logging.go`

The order-service writes orders to PostgreSQL and demonstrates a multi-stage business pipeline with a span for each stage.

**Routes:**

| Method | Path | What it does |
|--------|------|-------------|
| GET | `/health` | Liveness probe |
| POST | `/internal/orders` | Creates an order through the 4-stage pipeline |
| GET | `/internal/orders?user_id=N` | Lists orders for a user |
| GET | `/metrics` | Prometheus metrics endpoint |

**Full span tree for POST /internal/orders:**

```
order_service.create_order              (handler entry span)
  ├─ order_service.validate_order       (input validation — user_id > 0, amount > 0)
  ├─ order_service.check_inventory      (~20 ms simulated stock check)
  ├─ order_service.process_payment      (~30 ms simulated payment gate)
  └─ db.orders.insert                   (SQL INSERT — SpanKindClient)
```

Each stage creates a child span so you can see in the Jaeger waterfall exactly which stage is slow or failing. Stages run sequentially: the pipeline short-circuits on the first error.

**DB instrumentation (`db/db.go`):**

Manual spans follow the [OTel database semantic conventions](https://opentelemetry.io/docs/specs/semconv/database/). Key attributes set on every DB span:

```
db.system     = "postgresql"
db.name       = "otelpoc"
db.operation  = "INSERT" | "SELECT"
db.sql.table  = "orders"
db.statement  = "<parameterised SQL, never interpolated values>"
```

The `db.statement` intentionally uses `$1`, `$2` placeholders — never string-interpolated values — to avoid leaking PII into trace data.

**Built-in simulations (all trigger different failure modes):**

| Input | Stage that fails | HTTP status | What you see in Jaeger |
|-------|-----------------|-------------|----------------------|
| `user_id: 0` | `validate_order` | 400 | `validate_order` span red |
| `product_name: "out-of-stock"` | `check_inventory` | 422 | `check_inventory` span red |
| `amount > 1000` | `process_payment` | 402 | `process_payment` span red |
| `product_name: "slow-product"` | `db.orders.insert` | 200 | `db.orders.insert` span ~600 ms wide |
| `user_id: 999` | gateway validation | 404 | gateway spans red, order never created |

**Custom metrics:**
- `order_service.orders.created.total` — Int64Counter, labelled by product name
- `order_service.orders.failed.total` — Int64Counter, labelled by failure reason (validation_error / out_of_stock / payment_failed / db_error)
- `order_service.order_processing.duration_ms` — Float64Histogram, labelled by result
- `db.queries.total` — Int64Counter per SQL query, labelled by operation
- `db.query.duration_ms` — Float64Histogram of SQL query latency

---

## How to Run the Stack

### Prerequisites

- Docker ≥ 24
- Docker Compose ≥ 2.20 (`docker compose` subcommand, not the old `docker-compose`)
- ~3 GB free RAM (Loki + Grafana add ~200 MB over the original stack)

### Start everything

```bash
# From the repo root
docker compose up --build
```

Dependencies (`go.mod` + `go.sum`) are committed, so `go mod download` in Docker uses the local cache — no network resolution needed after the first pull. Subsequent builds reuse the Docker layer.

Wait for all services to be healthy:

```bash
docker compose ps
```

All entries should show `(healthy)` before you start generating traffic.

### Open the UIs

| UI | URL | Credentials |
|----|-----|-------------|
| Grafana (dashboards + logs) | http://localhost:3000 | admin / admin |
| Jaeger trace explorer | http://localhost:16686 | — |
| Prometheus query browser | http://localhost:9090 | — |
| Loki (API / ready check) | http://localhost:3100/ready | — |

### Tear down

```bash
docker compose down       # keeps all volumes (Postgres data, Loki data)
docker compose down -v    # removes all volumes (fresh state on next start)
```

---

## How Distributed Tracing Actually Works

### The W3C traceparent header

When the gateway calls user-service, it attaches this HTTP header:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
             ^^  ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^  ^^^^^^^^^^^^^^^^  ^^
             ver  trace-id (128-bit hex, 32 chars)  span-id (16 ch)  flags
```

- `trace-id` is generated once at the root of the request and never changes.
- `span-id` identifies the *current* span. The receiving service creates a new span ID but keeps the same trace ID, marking the sender's span ID as its parent.
- `flags = 01` means "sampling decision = record this trace".

### The propagation lifecycle

```
1. curl → gateway
   otelgin creates root span:
     traceId  = abc123
     spanId   = span001
     parentId = (none)

2. gateway → user-service
   otelhttp.Transport injects header:
     traceparent: 00-abc123-span001-01
   Creates a CLIENT span:
     traceId  = abc123
     spanId   = span002
     parentId = span001

3. user-service receives request
   otelgin reads the header, creates a SERVER span:
     traceId  = abc123   ← same trace ID!
     spanId   = span003
     parentId = span002  ← child of gateway's client span

4. All spans (span001–span003) export to Collector → Jaeger.
   Jaeger reassembles them into one tree using the shared traceId.
```

### How logs join the same trace

Every `slog.InfoContext(ctx, ...)` call goes through `logging.fanHandler`. When `ctx` contains an active span, the `traceHandler` wrapper extracts `trace_id` and `span_id` from `trace.SpanFromContext(ctx).SpanContext()` and adds them to the log record.

The same trace ID that identifies the span tree in Jaeger appears in every log line written during that request:

```json
{
  "time": "2024-01-15T12:00:00Z",
  "level": "INFO",
  "msg": "order created",
  "order_id": 42,
  "trace_id": "abc123",
  "span_id": "span003"
}
```

This is the foundation of log-trace correlation: one ID links the distributed trace waterfall to every structured log line that fired during the same request.

---

## Guided Exercises

Work through these in order. Each exercise points to specific code to read, the curl command to run, and what to look for.

---

### Exercise 1 — Your first 9-span distributed trace

**Goal:** See a complete trace spanning all three services, including all four order stages.

**Run:**
```bash
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "widget", "amount": 49.99}' | jq .
```

**Inspect in Jaeger:**
1. Open http://localhost:16686
2. Select service: `gateway-service` → Find Traces
3. Click the trace for `POST /orders`
4. Expand all spans. You should see the full waterfall:
   ```
   POST /orders                                   [gateway-service]
     └─ gateway.create_order
          ├─ gateway.validate_user
          │    └─ http.client GET user-service /internal/users/1
          │         └─ GET /internal/users/1      [user-service]
          │              └─ user_service.get_user
          └─ http.client POST order-service /internal/orders
               └─ POST /internal/orders           [order-service]
                    └─ order_service.create_order
                         ├─ order_service.validate_order
                         ├─ order_service.check_inventory
                         ├─ order_service.process_payment
                         └─ db.orders.insert
   ```

**What to notice:**
- The entire waterfall shares one Trace ID (shown at the top of the Jaeger page).
- Three different service names appear in different colours.
- Click `db.orders.insert` and look at its Tags tab — you'll see `db.system`, `db.statement`, `db.operation` (OTel semantic conventions for databases).
- Click `order_service.process_payment` and look for the `payment.auth_code` attribute on successful payment.

**Code to read:** `gateway-service/handlers/handlers.go` — the `validateUser` function creates a span wrapping an outbound call. This is the pattern for representing "business logic that calls downstream" as a named unit in a trace.

---

### Exercise 2 — Error spans (three different failure modes)

**Goal:** See how different stages of a pipeline appear as error spans in Jaeger.

**Run all three:**
```bash
# Payment declined (amount > $1000) — process_payment span goes red
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "gadget", "amount": 1500.00}' | jq .

# Out of stock — check_inventory span goes red
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "out-of-stock", "amount": 9.99}' | jq .

# User not found — gateway and user-service spans go red; order stage never reached
curl -s http://localhost:8080/users/999 | jq .
```

**Inspect in Jaeger:**
1. Find Traces for `gateway-service` — look for traces with red spans
2. For each, click the trace and click the red span
3. In the Tags tab, look for `otel.status_code = ERROR`
4. Notice how error location differs: payment vs inventory vs user-service

**What to notice:**
- Errors at different pipeline stages produce different red span locations in the waterfall.
- The "user not found" trace is red all the way up to the gateway root span because the error propagates through the parent chain.
- The "payment declined" and "out of stock" errors only mark the specific stage span (and its parents) red — the DB is never hit.

**Code to read:** `order-service/handlers/handlers.go` — look at `checkInventory` and `processPayment`. Both call `span.RecordError(err)` + `span.SetStatus(codes.Error, ...)`. Compare with `user-service/handlers/handlers.go:GetUser` for the 404 pattern.

---

### Exercise 3 — Slow spans

**Goal:** See how latency appears in the Jaeger waterfall.

**Run:**
```bash
# Slow user lookup (200 ms cache miss simulation)
curl -s http://localhost:8080/users/2 | jq .

# Slow DB write (600 ms slow-product simulation)
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "product_name": "slow-product", "amount": 9.99}' | jq .
```

**Inspect in Jaeger:**
- For the `GET /users/2` trace: find the `user_service.cache_miss_simulation` span — it should be ~200 ms wide.
- For the `POST /orders` trace: find `db.orders.insert` — it should be ~600 ms wide. Click its **Logs** tab (span events) to see `slow_query_simulation_start` and `slow_query_simulation_end` markers.

**Why this matters:** In a real system, wide leaf spans indicate slow queries, cache misses, or synchronous calls to slow external APIs. The waterfall width is your first performance signal.

**Code to read:** `order-service/internal/db/db.go` — the simulation is a `time.Sleep` bracketed by `span.AddEvent()` calls. Events appear as point-in-time markers on the Jaeger span timeline.

---

### Exercise 4 — Span attributes as searchable metadata

**Goal:** Search for traces by a business attribute.

In Jaeger's search form, under **Tags**, enter:
```
order.product_name=widget
```

You should see only the traces where a widget was ordered.

**Why this matters:** Span attributes turn traces into a searchable index over your system's business activity. In production you'd filter by `user.id`, `payment.status`, `http.status_code`, etc.

**Code to read:** `gateway-service/handlers/handlers.go` — attributes are added with `span.SetAttributes(attribute.String("order.product_name", req.ProductName))`.

---

### Exercise 5 — Metrics in Prometheus

**Goal:** Query the custom metrics produced by the services.

Open http://localhost:9090 → **Graph** tab. Try these PromQL queries:

```promql
# Gateway request rate by endpoint
sum by (endpoint) (rate(gateway_requests_total[5m]))

# P95 gateway latency
histogram_quantile(0.95, sum by (le) (rate(gateway_request_duration_ms_bucket[5m])))

# Failed orders by reason (out_of_stock, payment_failed, validation_error, db_error)
sum by (reason) (rate(order_service_orders_failed_total[5m]))

# P95 order processing duration
histogram_quantile(0.95, sum by (le) (rate(order_service_order_processing_duration_ms_bucket[5m])))

# DB query rate by SQL operation
rate(db_queries_total[1m])

# Error rate as a percentage
rate(gateway_upstream_errors_total[5m]) / rate(gateway_requests_total[5m]) * 100

# Collector's own metrics — spans received per second
rate(otelcol_receiver_accepted_spans_total[1m])
```

Go to **Status → Targets** and confirm all five scrape targets are `UP`.

**Why `otelcol_receiver_accepted_spans_total` matters:** If spans stop appearing in Jaeger but this counter is still rising, the problem is between the Collector and Jaeger (network, config), not between your service and the Collector.

---

### Exercise 6 — Grafana dashboards (RED method)

**Goal:** Read the pre-provisioned dashboard and understand the RED method.

1. Open http://localhost:3000 (admin / admin)
2. **Dashboards → TraceForge → TraceForge — Platform Overview**

Generate some mixed traffic first:
```bash
for i in $(seq 1 20); do
  # Normal order
  curl -s -X POST http://localhost:8080/orders \
    -H 'Content-Type: application/json' \
    -d "{\"user_id\":$((RANDOM % 5 + 1)),\"product_name\":\"widget\",\"amount\":$((RANDOM % 100 + 10)).99}" > /dev/null
  # Occasional errors
  curl -s -X POST http://localhost:8080/orders \
    -H 'Content-Type: application/json' \
    -d '{"user_id":1,"product_name":"out-of-stock","amount":5.00}' > /dev/null
  curl -s -X POST http://localhost:8080/orders \
    -H 'Content-Type: application/json' \
    -d '{"user_id":2,"product_name":"gadget","amount":1500.00}' > /dev/null
  sleep 0.3
done
```

**What to notice in the dashboard:**
- **Service Health row**: all three services should show green UP
- **Throughput row**: request rate timeseries, split by endpoint
- **Errors row**: two panels side-by-side — upstream errors at the gateway, and failed orders by reason (watch `out_of_stock` and `payment_failed` appear as separate series)
- **Latency row**: P50/P95/P99 for the gateway, and separate DB latency percentiles
- **Business Metrics row**: stat panels show cumulative totals; error rate turns yellow/red above thresholds

**Why the RED method matters:** Rate, Errors, Duration — these three signals are sufficient to know whether a service is healthy and what to investigate first when it's not.

---

### Exercise 7 — Log exploration in Grafana (Loki)

**Goal:** Find the logs for a specific request and understand structured log fields.

1. Make a request and capture the response:
```bash
RESPONSE=$(curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 3, "product_name": "widget", "amount": 75.00}')
echo $RESPONSE | jq .
```

2. Open http://localhost:3000/explore
3. Select the **Loki** datasource
4. Enter this query to see order-service logs:
```logql
{job="order-service"}
```
5. Click a log line to expand it and read the structured fields.

**Now filter to all services:**
```logql
{job=~".+"}
```

**Filter to error logs only:**
```logql
{job=~".+"} | json | level=`error`
```

**What to notice:**
- Every log line is a structured JSON object with `trace_id`, `span_id`, `service.name`, and your business fields.
- The `trace_id` field matches the trace ID in Jaeger — this is the correlation link.
- The `level` label comes from the Loki exporter's `default_labels_enabled.level: true` setting — you can filter streams by severity without parsing the body.

**Code to read:** `order-service/internal/logging/logging.go` — the `traceHandler` wrapper that injects `trace_id` and `span_id` from `trace.SpanFromContext(ctx)`.

---

### Exercise 8 — Log-to-trace correlation (Loki → Jaeger)

**Goal:** Navigate from a log line to the full trace waterfall in one click.

1. Trigger a payment error (easy to find in logs):
```bash
curl -s -X POST http://localhost:8080/orders \
  -H "Content-Type: application/json" \
  -d '{"user_id": 4, "product_name": "expensive", "amount": 9999.00}' | jq .
```

2. Open Grafana Explore → Loki datasource
3. Query for payment errors:
```logql
{job="order-service"} |= "payment declined"
```

4. Click the log line to expand it
5. Look for the **View in Jaeger** link next to the `trace_id` field
6. Click it — Jaeger opens directly to the waterfall for that request

**What to notice:**
- The derived field extracts `trace_id` from the JSON body using the regex `"trace_id":"([a-f0-9]{32})"`.
- The link is `datasourceUid: jaeger` in the Loki datasource config — Grafana resolves this to the Jaeger URL automatically.
- The waterfall shows `order_service.process_payment` in red, confirming the log and the trace describe the same event.

**Code to read:** `grafana/provisioning/datasources/datasources.yml` — the `derivedFields` block under the Loki datasource.

---

### Exercise 9 — Trace-to-log correlation (Jaeger → Loki)

**Goal:** Navigate from a specific span to the logs that fired during that span.

1. Open http://localhost:16686 and find any recent `POST /orders` trace
2. Click the trace to open the waterfall
3. Click the `order_service.create_order` span to expand its detail panel
4. Click the **Logs** button (or link) — this opens Grafana Explore pre-filtered with a LogQL query like:
   ```logql
   {job="order-service"} |= `<traceId>`
   ```
5. The log view shows every log line from order-service that fired during that specific trace

**What to notice:**
- The LogQL query comes from the Jaeger datasource's `tracesToLogsV2.query` template in `datasources.yml`.
- `${__span.tags["service.name"]}` resolves to the span's service name, `${__trace.traceId}` resolves to the trace ID.
- The time range in the Explore view is automatically bounded to ±1 minute around the span's start/end time.

**Why this matters:** In production, when you see an error span in Jaeger, you can jump directly to the exact log lines that fired during that span — without copying the trace ID manually. This cuts mean time to diagnosis significantly.

---

### Exercise 10 — Read the Collector logs

The Collector's `debug` exporter logs a summary of every span, metric, and log record it receives. Run:

```bash
docker compose logs otel-collector | head -100
```

You'll see output listing exported spans with their trace ID, service name, and attributes. This is invaluable when you're not sure whether your instrumentation is emitting data at all.

To watch live:
```bash
docker compose logs -f otel-collector 2>&1 | grep -E "(ScopeLogs|ScopeSpans|ResourceMetrics)"
```

---

### Exercise 11 — Graceful shutdown

**Goal:** Understand why `prov.Shutdown(ctx)` matters.

Open `gateway-service/main.go` and find the shutdown sequence at the bottom of `main()`:

1. `srv.Shutdown(shutCtx)` — stops accepting new HTTP requests, waits for in-flight requests to complete.
2. `prov.Shutdown(shutCtx)` — flushes the `BatchSpanProcessor`, `BatchLogProcessor`, and `PeriodicReader` internal queues to the Collector.

Without step 2, any spans or log records buffered in memory at shutdown time are silently dropped. Run some requests, then:

```bash
docker compose stop gateway-service
docker compose logs gateway-service | tail -5
```

You should see `gateway-service stopped` confirming the shutdown sequence completed cleanly.

---

## Testing Strategy

### Unit tests

Unit-test business logic in isolation using a no-op tracer. The OTel SDK ships `go.opentelemetry.io/otel/trace/noop` for this:

```go
import "go.opentelemetry.io/otel/trace/noop"

func TestValidateOrder(t *testing.T) {
    otel.SetTracerProvider(noop.NewTracerProvider())
    h := &Handler{}
    err := h.validateOrder(context.Background(), 1, "widget", 9.99)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}
```

This keeps unit tests fast and side-effect-free — no SDK bootstrapping, no network calls.

### Integration tests (in-process, with span capture)

Use `go.opentelemetry.io/otel/sdk/trace/tracetest` to capture spans in memory and assert on them:

```go
import (
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    "go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCreateOrder_PaymentDeclined(t *testing.T) {
    sr := tracetest.NewSpanRecorder()
    tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
    otel.SetTracerProvider(tp)

    // inject the handler with a mock DB ...

    // make request with amount > 1000
    // assert HTTP 402

    spans := sr.Ended()
    var paymentSpan sdktrace.ReadOnlySpan
    for _, s := range spans {
        if s.Name() == "order_service.process_payment" {
            paymentSpan = s
        }
    }
    if paymentSpan == nil {
        t.Fatal("process_payment span not found")
    }
    if paymentSpan.Status().Code != codes.Error {
        t.Errorf("expected error status on payment span, got %v", paymentSpan.Status().Code)
    }
}
```

`SpanRecorder` collects all spans ended during the test without exporting anywhere. Assert on span names, attributes, status codes, and events.

### End-to-end tests

```bash
docker compose up --build --wait
go test ./e2e/... -v
docker compose down -v
```

In an e2e test, make a real HTTP call and then query Jaeger's API to verify the trace appeared:

```bash
# Jaeger HTTP API
curl "http://localhost:16686/api/traces?service=gateway-service&limit=1" | jq '.data[0].spans | length'
```

Verify the Loki pipeline in e2e tests:

```bash
# Check Loki received data for order-service
curl -s 'http://localhost:3100/loki/api/v1/labels' | jq '.data'

# Query for recent order-service logs
curl -s -G 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={job="order-service"}' \
  --data-urlencode 'limit=5' | jq '.data.result[0].values[0][1]'
```

### What to assert in tests

| Layer | Assert on |
|-------|-----------|
| Handler | HTTP status code, response body |
| Spans | Span name, `status.code`, presence of specific attributes, events |
| Metrics | Counter increments (use the `metricdata` package for in-process assertions) |
| Logs | Presence of `trace_id` and `span_id` fields in log output |
| Integration | Trace appears in Jaeger within N seconds after request |
| Integration | Log line with trace_id appears in Loki |

### Running Go build checks

```bash
(cd gateway-service && go build ./... && go vet ./...)
(cd user-service   && go build ./... && go vet ./...)
(cd order-service  && go build ./... && go vet ./...)
```

---

## Key Files and Where to Read First

If you're new to the codebase, read files in this order:

1. **`collector/config.yaml`** — understand the three-pipeline data flow before looking at service code.
2. **`gateway-service/internal/telemetry/telemetry.go`** — the SDK bootstrap. All three providers (Tracer, Meter, Logger) are initialised here with comments explaining each.
3. **`gateway-service/internal/logging/logging.go`** — the `fanHandler` pattern: JSON stdout for `docker compose logs`, plus the OTel bridge for Loki. The `traceHandler` wrapper shows exactly how `trace_id` enters every log line.
4. **`gateway-service/main.go`** — see how the bootstrap is called, the mandatory 3-step logger init order, and how graceful shutdown flushes all three signal pipelines.
5. **`gateway-service/handlers/handlers.go`** — the package docstring explains context propagation. The `CreateOrder` flow shows the full pattern: receive request → create span → call downstream with `otelhttp.Transport` → log with context → record metrics.
6. **`user-service/handlers/handlers.go`** — see how a receiving service creates a child span from the incoming `traceparent` header.
7. **`order-service/handlers/handlers.go`** — the file-top comment has the full span tree. The `checkInventory` and `processPayment` functions show how to instrument a multi-stage business pipeline.
8. **`order-service/internal/db/db.go`** — manual DB span instrumentation following OTel semantic conventions.
9. **`grafana/provisioning/datasources/datasources.yml`** — how the three datasources are wired for bidirectional trace↔log correlation.
10. **`loki/config.yaml`** — single-binary Loki configuration: storage, schema, ingestion limits.

---

## Extending This Project

| Goal | Steps |
|------|-------|
| Add a new downstream service | Copy `user-service/`, register in `docker-compose.yml`, add a scrape target in `prometheus/prometheus.yml` |
| Add a new metric instrument | Call `meter.Int64Counter(...)` or `meter.Float64Histogram(...)` after `telemetry.Init` |
| Add a span event | `span.AddEvent("event name", trace.WithAttributes(...))` |
| Add a new business stage to order-service | Create a child span with `tracer.Start(ctx, "order_service.stage_name")`, call `span.End()` with defer |
| Add W3C Baggage (propagate metadata across services) | `baggage.New(...)` + inject/extract via the global propagator |
| Reduce trace volume in production | Replace `sdktrace.AlwaysSample()` with `sdktrace.TraceIDRatioBased(0.01)` for 1% sampling |
| Switch trace backend to Grafana Tempo | Add `otlp/tempo` exporter in `collector/config.yaml`; update Grafana datasource to Tempo; enable trace-to-log correlation in Tempo datasource config |
| Add Alertmanager for metric-based alerts | Add `alertmanager` service to `docker-compose.yml`; add `alerting_rules.yml` to Prometheus config |
| Persist Jaeger traces across restarts | Replace `all-in-one` with a Jaeger Collector + Query + Cassandra/Elasticsearch backend |
| Add sampling decisions visible in Jaeger | Add `probabilistic_sampler` processor to the collector traces pipeline |
| Explore Grafana Tempo as Jaeger replacement | Tempo is purpose-built for OTel, stores traces in object storage, and integrates natively with Grafana |
