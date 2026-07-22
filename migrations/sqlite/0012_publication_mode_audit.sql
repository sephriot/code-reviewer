-- Publication mode is a deployment safety gate. Every startup-driven change
-- is durable so operators can reconstruct why an effect was authorized.
CREATE TABLE publication_mode_changes (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    previous_mode TEXT NOT NULL CHECK (previous_mode IN ('disabled', 'simulated', 'enabled')),
    new_mode TEXT NOT NULL CHECK (new_mode IN ('disabled', 'simulated', 'enabled')),
    source TEXT NOT NULL CHECK (source = 'runtime_config'),
    changed_at_us INTEGER NOT NULL CHECK (changed_at_us >= 0),
    CHECK (previous_mode <> new_mode)
);

CREATE INDEX idx_publication_mode_changes_changed
    ON publication_mode_changes(changed_at_us, id);
