# Cerebrum

> OpenWolf's learning memory. Updated automatically as the AI learns from interactions.
> Do not edit manually unless correcting an error.
> Last updated: 2026-05-28

## User Preferences

<!-- How the user likes things done. Code style, tools, patterns, communication. -->

## Key Learnings

- **Project:** traceforge
- **Description:** Production-oriented cloud-native observability platform (traces+metrics+logs) across 3 Go microservices. All three OTel signal pillars flowing through a central Collector.
- **Log correlation pattern:** `logging.NewLogger(serviceName)` creates a fanHandler with (a) JSON stdout enriched with trace_id/span_id via `traceHandler` wrapper, and (b) `otelslog.NewHandler` bridge to OTel global LoggerProvider. Must call AFTER `telemetry.Init`.
- **Service init order in main.go:** (1) bootstrap slog, (2) telemetry.Init (sets global LoggerProvider), (3) slog.SetDefault(logging.NewLogger). This ordering is mandatory.
- **Collector logs pipeline (current):** uses `loki` exporter with `default_labels_enabled.job: true` — maps `service.name` to the Loki `job` label. Loki runs on :3100, receives OTLP logs from Collector via HTTP push.
- **Loki config:** grafana/loki:3.1.0 in single-binary mode, schema v13 + tsdb, filesystem storage, unordered_writes enabled for restart resilience. Config at loki/config.yaml.
- **Trace↔Log correlation in Grafana:** Loki datasource has derivedFields regex `"trace_id":"([a-f0-9]{32})"` → links to Jaeger. Jaeger datasource tracesToLogsV2 uses LogQL `{job="<service>"} |= <traceId>` to jump from span to logs.
- **Demo scenarios:** `slow-product`=DB slow query (600ms), `out-of-stock`=inventory error (422), amount>1000=payment declined (402), user_id 999=not found error (404), user_id 2=cache miss simulation (200ms).
- **go mod tidy in Dockerfile:** All three services use `go mod tidy` during Docker build. Only go.mod module name + go version needed; tidy adds require block automatically.

## Do-Not-Repeat

<!-- Mistakes made and corrected. Each entry prevents the same mistake recurring. -->
<!-- Format: [YYYY-MM-DD] Description of what went wrong and what to do instead. -->

- [2026-05-28] **Never use `go mod tidy` inside Dockerfile.** Empty go.mod + `go mod tidy` at Docker build time fails when the build network can't reach GOPROXY. Instead: run `go mod tidy` locally once to populate go.mod require block and go.sum, then use `COPY go.mod go.sum ./` + `RUN go mod download` in the Dockerfile. Dependencies are resolved from the committed go.sum — no internet access needed at build time.

## Decision Log

<!-- Significant technical decisions with rationale. Why X was chosen over Y. -->
