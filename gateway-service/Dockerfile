FROM golang:1.25-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git ca-certificates

# Copy module files first so Docker can cache the dependency layer.
# Dependencies are only re-downloaded when go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy sources and build.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -p 1 -ldflags="-s -w" -o /gateway-service .

FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /gateway-service .

EXPOSE 8080

CMD ["./gateway-service"]
