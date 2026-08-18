-- +goose Up
CREATE TYPE worker_status AS ENUM (
    'available',
    'draining',
    'stalled',
    'shutting_down'
);

CREATE TYPE session_mode AS ENUM (
    'default',
    'dedicated'
);

CREATE TYPE session_status AS ENUM (
    'pending',
    'running',
    'completed',
    'failed',
    'expired'
);

CREATE TABLE workers (
    id uuid PRIMARY KEY,
    address text NOT NULL,
    -- Plain text plus CHECK is deliberate because new browser values, such as camoufox, are expected.
    browser text NOT NULL CHECK (browser IN ('chromium', 'firefox', 'webkit')),
    playwright_version text NOT NULL,
    max_slots integer NOT NULL CHECK (max_slots > 0),
    status worker_status NOT NULL,
    last_heartbeat timestamptz NOT NULL,
    lifetime_sessions bigint NOT NULL DEFAULT 0 CHECK (lifetime_sessions >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    hash text NOT NULL UNIQUE,
    prefix text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    -- Workers are a live registry and are deleted when removed; sessions are history and use bare worker IDs on purpose.
    worker_id uuid NOT NULL,
    -- Plain text plus CHECK is deliberate because new browser values, such as camoufox, are expected.
    browser text NOT NULL CHECK (browser IN ('chromium', 'firefox', 'webkit')),
    playwright_version text NOT NULL,
    worker_address text NOT NULL,
    mode session_mode NOT NULL,
    status session_status NOT NULL,
    -- API keys are revoked, never deleted (GitHub-style); RESTRICT protects session attribution.
    created_by_key uuid REFERENCES api_keys (id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    last_heartbeat timestamptz NOT NULL,
    keep_alive_ms integer CHECK (keep_alive_ms IS NULL OR keep_alive_ms > 0),
    connect_metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX sessions_worker_id_idx ON sessions (worker_id);
CREATE INDEX sessions_running_worker_idx ON sessions (worker_id)
    WHERE status = 'running';
CREATE INDEX sessions_running_heartbeat_idx ON sessions (last_heartbeat)
    WHERE status IN ('pending', 'running');

-- +goose Down
DROP TABLE sessions;
DROP TABLE api_keys;
DROP TABLE workers;
DROP TYPE session_status;
DROP TYPE session_mode;
DROP TYPE worker_status;
