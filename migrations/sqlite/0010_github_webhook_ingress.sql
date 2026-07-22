-- GitHub webhook ingress retains only verified delivery metadata. Raw payloads
-- are deliberately not persisted: provider payloads can contain code, user,
-- and repository data not needed for idempotency or later reconciliation.

CREATE TABLE github_webhook_deliveries (
    delivery_id TEXT PRIMARY KEY NOT NULL CHECK (
        length(delivery_id) = 36
        AND delivery_id GLOB '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
    ),
    event_type TEXT NOT NULL CHECK (event_type IN ('ping', 'pull_request', 'pull_request_review')),
    action TEXT NOT NULL CHECK (length(action) BETWEEN 1 AND 64),
    repository_github_id INTEGER NOT NULL CHECK (repository_github_id > 0),
    pull_request_number INTEGER CHECK (pull_request_number IS NULL OR pull_request_number > 0),
    payload_sha256 TEXT NOT NULL CHECK (
        length(payload_sha256) = 64
        AND payload_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    payload_bytes INTEGER NOT NULL CHECK (payload_bytes BETWEEN 1 AND 1048576),
    received_at_us INTEGER NOT NULL CHECK (received_at_us >= 0),
    CHECK (
        (event_type = 'ping' AND pull_request_number IS NULL)
        OR
        (event_type IN ('pull_request', 'pull_request_review') AND pull_request_number IS NOT NULL)
    )
);

CREATE INDEX idx_github_webhook_deliveries_received
    ON github_webhook_deliveries(received_at_us, delivery_id);

CREATE TRIGGER trg_github_webhook_deliveries_immutable
BEFORE UPDATE ON github_webhook_deliveries
BEGIN
    SELECT RAISE(ABORT, 'github webhook delivery metadata is immutable');
END;

CREATE TRIGGER trg_github_webhook_deliveries_no_delete
BEFORE DELETE ON github_webhook_deliveries
BEGIN
    SELECT RAISE(ABORT, 'github webhook delivery metadata is retained');
END;
