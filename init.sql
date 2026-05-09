-- ────────────────────────────────────────────────────────────────────────────
-- Database initialisation for the OTel PoC
-- This script runs automatically when the postgres container first starts.
-- ────────────────────────────────────────────────────────────────────────────

-- Users table (user-service keeps an in-memory mirror of these)
CREATE TABLE IF NOT EXISTS users (
    id         SERIAL PRIMARY KEY,
    name       VARCHAR(100) NOT NULL,
    email      VARCHAR(150) UNIQUE NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Orders table (written by order-service)
CREATE TABLE IF NOT EXISTS orders (
    id           SERIAL PRIMARY KEY,
    user_id      INTEGER      NOT NULL REFERENCES users(id),
    product_name VARCHAR(200) NOT NULL,
    amount       NUMERIC(10,2) NOT NULL CHECK (amount > 0),
    status       VARCHAR(50)  NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS orders_user_id_idx ON orders(user_id);
CREATE INDEX IF NOT EXISTS orders_status_idx  ON orders(status);

-- ── Seed data ────────────────────────────────────────────────────────────────
-- These three users are mirrored in the user-service in-memory store.
-- IDs 1-3 are guaranteed because SERIAL starts at 1.
INSERT INTO users (name, email) VALUES
    ('Alice Johnson', 'alice@example.com'),
    ('Bob Smith',     'bob@example.com'),
    ('Charlie Brown', 'charlie@example.com')
ON CONFLICT (email) DO NOTHING;
