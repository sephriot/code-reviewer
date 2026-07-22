-- An enabled delivery with an uncertain outcome must never be retried by the
-- control plane. An operator records one immutable terminal resolution after
-- inspecting GitHub or deliberately abandoning the effect.
CREATE TABLE publication_uncertainty_resolutions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    effect_id TEXT NOT NULL UNIQUE REFERENCES publication_effects(id) ON DELETE RESTRICT,
    attempt_id TEXT NOT NULL UNIQUE REFERENCES publication_attempts(id) ON DELETE RESTRICT,
    resolution TEXT NOT NULL CHECK (resolution IN ('externally_completed', 'abandoned')),
    actor_id TEXT NOT NULL CHECK (length(actor_id) > 0),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (length(idempotency_key) > 0),
    reason TEXT NOT NULL DEFAULT '',
    resolved_at_us INTEGER NOT NULL CHECK (resolved_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= resolved_at_us)
);

CREATE INDEX idx_publication_uncertainty_resolutions_resolved
    ON publication_uncertainty_resolutions(resolved_at_us, id);

CREATE TRIGGER trg_publication_uncertainty_resolutions_immutable_update
BEFORE UPDATE ON publication_uncertainty_resolutions
BEGIN
    SELECT RAISE(ABORT, 'publication uncertainty resolution is immutable');
END;

CREATE TRIGGER trg_publication_uncertainty_resolutions_immutable_delete
BEFORE DELETE ON publication_uncertainty_resolutions
BEGIN
    SELECT RAISE(ABORT, 'publication uncertainty resolution is retained');
END;
