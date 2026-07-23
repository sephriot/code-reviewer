CREATE TABLE cutover_checkpoints (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    publication_mode TEXT NOT NULL,
    domain_event_sequence INTEGER NOT NULL,
    created_at_us INTEGER NOT NULL
);
