# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

TraceForge is a distributed observability playground built with OpenTelemetry, Go, Jaeger, and Prometheus.

## Expected Stack

- **Language:** Go
- **Observability:** OpenTelemetry SDK for instrumentation
- **Tracing backend:** Jaeger
- **Metrics backend:** Prometheus

## Anticipated Commands

Once the project is scaffolded, standard Go commands apply:

```sh
go build ./...       # build all packages
go test ./...        # run all tests
go test ./pkg/... -run TestName  # run a single test
go vet ./...         # lint
```

If a `docker-compose.yml` is added for Jaeger/Prometheus, use `docker compose up` to start the observability backends.
