-- A dispatch claim is written before any enabled-mode GitHub request. It is
-- immutable and unique per effect: a process crash after claiming but before
-- recording a response is conservative uncertainty, never a blind repost.
CREATE TABLE publication_dispatch_claims (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    effect_id TEXT NOT NULL UNIQUE REFERENCES publication_effects(id) ON DELETE RESTRICT,
    attempt_number INTEGER NOT NULL CHECK (attempt_number = 1),
    request_sha256 TEXT NOT NULL CHECK (
        length(request_sha256) = 64
        AND request_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    claimed_at_us INTEGER NOT NULL CHECK (claimed_at_us >= 0),
    UNIQUE (effect_id, attempt_number)
);

CREATE TRIGGER trg_publication_dispatch_claims_immutable_update
BEFORE UPDATE ON publication_dispatch_claims
BEGIN
    SELECT RAISE(ABORT, 'publication dispatch claim is immutable');
END;

CREATE TRIGGER trg_publication_dispatch_claims_immutable_delete
BEFORE DELETE ON publication_dispatch_claims
BEGIN
    SELECT RAISE(ABORT, 'publication dispatch claim is retained');
END;
