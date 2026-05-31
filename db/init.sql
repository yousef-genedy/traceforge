-- ────────────────────────────────────────────────────────────────────────────
-- TraceForge — Database Initialisation
-- Runs automatically when the postgres container first starts.
-- ────────────────────────────────────────────────────────────────────────────

-- ── Users ────────────────────────────────────────────────────────────────────
-- user-service keeps an in-memory mirror of these rows (IDs 1-5).
CREATE TABLE IF NOT EXISTS users (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(100)  NOT NULL,
    email      VARCHAR(150)  UNIQUE NOT NULL,
    tier       VARCHAR(20)   NOT NULL DEFAULT 'standard',
    created_at TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

-- ── Orders ───────────────────────────────────────────────────────────────────
-- Written by order-service. status lifecycle: pending → confirmed → shipped.
CREATE TABLE IF NOT EXISTS orders (
    id           SERIAL PRIMARY KEY,
    user_id      INTEGER       NOT NULL REFERENCES users(id),
    product_name VARCHAR(200)  NOT NULL,
    amount       NUMERIC(10,2) NOT NULL CHECK (amount > 0),
    status       VARCHAR(50)   NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS orders_user_id_idx ON orders(user_id);
CREATE INDEX IF NOT EXISTS orders_status_idx  ON orders(status);
CREATE INDEX IF NOT EXISTS orders_created_idx ON orders(created_at DESC);

-- ── Seed data ─────────────────────────────────────────────────────────────────
-- IDs 1-5 are guaranteed (SERIAL starts at 1, conflict-safe).
-- Must mirror the in-memory store in user-service/handlers/handlers.go.
INSERT INTO users (name, email, tier) VALUES
    ('Alice Johnson', 'alice@example.com', 'premium'),
    ('Bob Smith',     'bob@example.com',   'standard'),
    ('Charlie Brown', 'charlie@example.com','standard'),
    ('Diana Prince',  'diana@example.com', 'premium'),
    ('Evan Zhao',     'evan@example.com',  'standard')
ON CONFLICT (email) DO NOTHING;
