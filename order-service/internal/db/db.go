// Package db wraps database/sql with manual OpenTelemetry instrumentation.
//
// # Why manual spans instead of an ORM or otelsql wrapper?
//
// Manual instrumentation makes the concepts explicit:
//  1. We create a CLIENT span for each SQL operation.
//  2. We attach db.system, db.name, db.statement attributes (semantic conventions).
//  3. Any error is recorded on the span with RecordError + SetStatus.
//  4. The span's duration in Jaeger directly shows how long the query took.
//
// In a production codebase you would use github.com/XSAM/otelsql which wraps
// database/sql automatically, but the manual approach here is more didactic.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver registration side-effect
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("order-service/db")

// Order is the domain object written to and read from PostgreSQL.
type Order struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	ProductName string    `json:"product_name"`
	Amount      float64   `json:"amount"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// DB wraps a sql.DB connection pool with OTel instrumentation.
type DB struct {
	conn         *sql.DB
	dbName       string
	queryCounter metric.Int64Counter
	queryLatency metric.Float64Histogram
}

// Config holds PostgreSQL connection parameters.
type Config struct {
	Host     string
	Port     string
	Name     string
	User     string
	Password string
}

// New opens a connection pool and verifies connectivity with a ping.
func New(ctx context.Context, cfg Config) (*DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s sslmode=disable",
		cfg.Host, cfg.Port, cfg.Name, cfg.User, cfg.Password,
	)

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Ping with context so the startup probe is traced if called with a span ctx.
	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)

	meter := otel.Meter("order-service/db")

	qcount, _ := meter.Int64Counter(
		"db.queries.total",
		metric.WithDescription("Total SQL queries executed"),
		metric.WithUnit("{query}"),
	)

	qlatency, _ := meter.Float64Histogram(
		"db.query.duration_ms",
		metric.WithDescription("SQL query latency in milliseconds"),
		metric.WithUnit("ms"),
	)

	slog.InfoContext(ctx, "database connected", "host", cfg.Host, "db", cfg.Name)

	return &DB{
		conn:         conn,
		dbName:       cfg.Name,
		queryCounter: qcount,
		queryLatency: qlatency,
	}, nil
}

// CreateOrder inserts a new order row and returns the full record.
//
// # DB Span Attributes (OpenTelemetry Semantic Conventions)
//
// We set the standard database span attributes defined at:
// https://opentelemetry.io/docs/specs/semconv/database/
//
//   - db.system        — which database technology ("postgresql")
//   - db.name          — the database name
//   - db.operation     — the SQL verb ("INSERT")
//   - db.sql.table     — the target table
//   - db.statement     — the parameterised SQL (never interpolated values)
func (d *DB) CreateOrder(ctx context.Context, o Order) (*Order, error) {
	start := time.Now()

	// SpanKindClient marks this as an outbound call to an external system.
	// In Jaeger the span will appear as a leaf node (no children expected).
	ctx, span := tracer.Start(ctx, "db.orders.insert",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.name", d.dbName),
			attribute.String("db.operation", "INSERT"),
			attribute.String("db.sql.table", "orders"),
			// Never include user-supplied values in db.statement to avoid leaking PII.
			attribute.String("db.statement",
				"INSERT INTO orders (user_id, product_name, amount, status) VALUES ($1, $2, $3, $4) RETURNING id, created_at"),
		),
	)
	defer span.End()

	d.queryCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("db.operation", "INSERT"),
		attribute.String("db.sql.table", "orders"),
	))

	// ── Slow query simulation ─────────────────────────────────────────────
	// Products named "slow-product" trigger a 600 ms delay, producing a
	// visibly wide span in Jaeger. This demonstrates how you would detect
	// N+1 queries or unindexed lookups in a real system.
	if o.ProductName == "slow-product" {
		span.AddEvent("slow_query_simulation_start", trace.WithAttributes(
			attribute.String("reason", "unindexed full-table scan (simulated)"),
		))
		time.Sleep(600 * time.Millisecond)
		span.AddEvent("slow_query_simulation_end")
	}

	query := `
		INSERT INTO orders (user_id, product_name, amount, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id, created_at`

	row := d.conn.QueryRowContext(ctx, query, o.UserID, o.ProductName, o.Amount)
	if err := row.Scan(&o.ID, &o.CreatedAt); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "INSERT failed")
		d.queryLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("result", "error")),
		)
		return nil, fmt.Errorf("insert order: %w", err)
	}

	o.Status = "pending"
	span.SetAttributes(attribute.Int64("order.id", o.ID))

	d.queryLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
		metric.WithAttributes(attribute.String("result", "ok")),
	)

	return &o, nil
}

// ListOrdersByUser retrieves all orders for a given user_id.
// Demonstrates a SELECT span with SpanKindClient.
func (d *DB) ListOrdersByUser(ctx context.Context, userID int64) ([]Order, error) {
	start := time.Now()

	ctx, span := tracer.Start(ctx, "db.orders.list_by_user",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.name", d.dbName),
			attribute.String("db.operation", "SELECT"),
			attribute.String("db.sql.table", "orders"),
			attribute.String("db.statement", "SELECT * FROM orders WHERE user_id = $1"),
			attribute.Int64("query.user_id", userID),
		),
	)
	defer span.End()

	d.queryCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("db.operation", "SELECT"),
	))

	rows, err := d.conn.QueryContext(ctx,
		`SELECT id, user_id, product_name, amount, status, created_at
		 FROM orders WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "SELECT failed")
		d.queryLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("result", "error")),
		)
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.ProductName, &o.Amount, &o.Status, &o.CreatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan order: %w", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "rows iteration error")
		return nil, fmt.Errorf("rows error: %w", err)
	}

	span.SetAttributes(attribute.Int("db.rows_returned", len(orders)))

	d.queryLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
		metric.WithAttributes(attribute.String("result", "ok")),
	)

	return orders, nil
}

// Close releases the connection pool.
func (d *DB) Close() error {
	return d.conn.Close()
}
