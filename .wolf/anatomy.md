# anatomy.md

> Auto-maintained by OpenWolf. Last scanned: 2026-05-31T03:47:39.006Z
> Files: 38 tracked | Anatomy hits: 0 | Misses: 0

## ./

- `.DS_Store` (~2186 tok)
- `CLAUDE.md` — OpenWolf (~251 tok)
- `docker-compose.yml` — Docker Compose services (~2326 tok)
- `GUIDE.md` — TraceForge — Deep-Dive Learning Guide (~10736 tok)
- `README.md` — Project documentation (~1430 tok)
- `TEST_LIFECYCLE.md` — Test Lifecycle — What Happens on Every Request (~5890 tok)

## .claude/

- `settings.json` (~441 tok)

## .claude/rules/

- `openwolf.md` (~313 tok)

## collector/

- `config.yaml` — ────────────────────────────────────────────────────────────────────────────── (~1410 tok)

## db/

- `init.sql` — PostgreSQL schema (users + orders tables) and seed data for users 1-5 (~541 tok)

## docs/

- `GUIDE.md` — TraceForge — Deep-Dive Learning Guide (~10739 tok)

## gateway-service/

- `Dockerfile` — Docker container definition (~147 tok)
- `go.mod` — Go module definition (~9 tok)
- `main.go` — gateway-service is the single public entry point for the TraceForge platform. (~922 tok)

## gateway-service/handlers/

- `handlers.go` — implements the gateway HTTP handlers. (~3769 tok)

## gateway-service/internal/logging/

- `logging.go` — provides a production-style slog logger with two outputs: (~800 tok)

## gateway-service/internal/telemetry/

- `telemetry.go` — bootstraps the OpenTelemetry SDK for a service. (~1478 tok)

## grafana/provisioning/dashboards/

- `dashboards.yml` (~82 tok)
- `traceforge-overview.json` (~4723 tok)

## grafana/provisioning/datasources/

- `datasources.yml` (~594 tok)

## graphify-out/

- `.graphify_chunk_01.json` (~7265 tok)
- `.graphify_chunk_02.json` (~6671 tok)

## loki/

- `config.yaml` — ────────────────────────────────────────────────────────────────────────────── (~492 tok)
- `config.yaml` — Single-binary Loki config: filesystem/tsdb schema v13, port 3100, unordered writes (~492 tok)

## order-service/

- `Dockerfile` — Docker container definition (~102 tok)
- `go.mod` — Go module definition (~8 tok)
- `main.go` — order-service creates and manages orders in PostgreSQL. (~772 tok)

## order-service/handlers/

- `handlers.go` — implements the order-service HTTP handlers. (~3027 tok)

## order-service/internal/db/

- `db.go` — wraps database/sql with manual OpenTelemetry instrumentation. (~2200 tok)

## order-service/internal/logging/

- `logging.go` — provides a production-style slog logger with two outputs: (~800 tok)

## order-service/internal/telemetry/

- `telemetry.go` — bootstraps the OpenTelemetry SDK for the order service. (~861 tok)

## prometheus/

- `prometheus.yml` — ────────────────────────────────────────────────────────────────────────────── (~569 tok)

## user-service/

- `Dockerfile` — Docker container definition (~101 tok)
- `go.mod` — Go module definition (~8 tok)
- `main.go` — user-service serves internal user lookup requests. (~608 tok)

## user-service/handlers/

- `handlers.go` — implements the user-service HTTP handlers. (~1504 tok)

## user-service/internal/logging/

- `logging.go` — provides a production-style slog logger with two outputs: (~800 tok)

## user-service/internal/telemetry/

- `telemetry.go` — bootstraps the OpenTelemetry SDK for the user service. (~860 tok)
