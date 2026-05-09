FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install git (needed by go mod tidy for some VCS modules)
RUN apk add --no-cache git ca-certificates

# Copy sources and let go mod tidy resolve all dependencies.
# In production you would commit go.sum and use COPY go.sum ./ before
# COPY . . to benefit from Docker layer caching.
COPY . .

RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway-service .

# ── Runtime image ─────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /gateway-service .

EXPOSE 8080

CMD ["./gateway-service"]
