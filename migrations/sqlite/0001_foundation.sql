CREATE TABLE domain_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    aggregate_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_version INTEGER NOT NULL CHECK (event_version > 0),
    payload_json BLOB NOT NULL,
    correlation_id TEXT,
    causation_id TEXT,
    occurred_at_us INTEGER NOT NULL
);

CREATE INDEX idx_domain_events_aggregate
    ON domain_events(aggregate_type, aggregate_id, sequence);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    resource_type TEXT,
    resource_id TEXT,
    dedupe_key TEXT,
    payload_json BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'retry_wait', 'succeeded', 'failed', 'cancelled')),
    priority INTEGER NOT NULL DEFAULT 0,
    available_at_us INTEGER NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    lease_owner TEXT,
    lease_expires_at_us INTEGER,
    lease_generation INTEGER NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    last_error_class TEXT,
    last_error_message TEXT,
    result_event_id TEXT REFERENCES domain_events(id),
    created_at_us INTEGER NOT NULL,
    updated_at_us INTEGER NOT NULL
);

CREATE INDEX idx_jobs_claim
    ON jobs(state, available_at_us, priority DESC, id);

CREATE UNIQUE INDEX idx_jobs_active_dedupe
    ON jobs(kind, dedupe_key)
    WHERE dedupe_key IS NOT NULL
      AND state IN ('queued', 'running', 'retry_wait');

CREATE UNIQUE INDEX idx_jobs_result_event
    ON jobs(result_event_id)
    WHERE result_event_id IS NOT NULL;

CREATE TABLE outbox (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES domain_events(id) ON DELETE CASCADE,
    topic TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'delivering', 'delivered', 'retry_wait', 'failed')),
    available_at_us INTEGER NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 10 CHECK (max_attempts > 0),
    lease_owner TEXT,
    lease_expires_at_us INTEGER,
    lease_generation INTEGER NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    last_error TEXT,
    created_at_us INTEGER NOT NULL,
    updated_at_us INTEGER NOT NULL
);

CREATE INDEX idx_outbox_delivery
    ON outbox(state, available_at_us, id);

CREATE TABLE system_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at_us INTEGER NOT NULL
);

INSERT INTO system_state(key, value, updated_at_us)
VALUES ('publication_mode', 'disabled', unixepoch('subsec') * 1000000);
