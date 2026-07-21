CREATE TABLE legacy_sources (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    source_kind TEXT NOT NULL CHECK (source_kind IN ('sqlite')),
    display_name TEXT NOT NULL CHECK (length(display_name) > 0),
    location TEXT NOT NULL CHECK (length(location) > 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us)
);

CREATE TABLE legacy_snapshots (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    source_id TEXT NOT NULL REFERENCES legacy_sources(id) ON DELETE RESTRICT,
    physical_sha256 TEXT NOT NULL CHECK (
        length(physical_sha256) = 64
        AND physical_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    schema_sha256 TEXT NOT NULL CHECK (
        length(schema_sha256) = 64
        AND schema_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    logical_sha256 TEXT NOT NULL CHECK (
        length(logical_sha256) = 64
        AND logical_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    rowset_sha256 TEXT NOT NULL CHECK (
        length(rowset_sha256) = 64
        AND rowset_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    row_format_version INTEGER NOT NULL CHECK (row_format_version = 1),
    source_size_bytes INTEGER NOT NULL CHECK (source_size_bytes >= 0),
    table_count INTEGER NOT NULL CHECK (table_count >= 0),
    row_count INTEGER NOT NULL CHECK (row_count >= 0),
    table_counts_json BLOB NOT NULL CHECK (
        json_valid(table_counts_json)
        AND json_type(table_counts_json) = 'object'
    ),
    state TEXT NOT NULL DEFAULT 'importing' CHECK (
        state IN ('importing', 'complete')
    ),
    captured_at_us INTEGER NOT NULL CHECK (captured_at_us >= 0),
    completed_at_us INTEGER CHECK (
        completed_at_us IS NULL OR completed_at_us >= captured_at_us
    ),
    verified_row_count INTEGER CHECK (
        verified_row_count IS NULL OR verified_row_count >= 0
    ),
    coverage_sha256 TEXT CHECK (
        coverage_sha256 IS NULL
        OR (
            length(coverage_sha256) = 64
            AND coverage_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    CHECK (
        (
            state = 'importing'
            AND completed_at_us IS NULL
            AND verified_row_count IS NULL
            AND coverage_sha256 IS NULL
        )
        OR
        (
            state = 'complete'
            AND completed_at_us IS NOT NULL
            AND verified_row_count = row_count
            AND coverage_sha256 IS NOT NULL
        )
    ),
    UNIQUE (source_id, schema_sha256, logical_sha256),
    UNIQUE (id, source_id)
);

CREATE INDEX idx_legacy_snapshots_source_captured
    ON legacy_snapshots(source_id, captured_at_us DESC, id);

CREATE TABLE repositories (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    github_node_id TEXT,
    full_name TEXT NOT NULL COLLATE NOCASE CHECK (
        length(full_name) > 2
        AND instr(full_name, '/') > 1
    ),
    owner_login TEXT NOT NULL COLLATE NOCASE CHECK (length(owner_login) > 0),
    name TEXT NOT NULL COLLATE NOCASE CHECK (length(name) > 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    UNIQUE (full_name),
    UNIQUE (id, full_name)
);

CREATE UNIQUE INDEX idx_repositories_github_node_id
    ON repositories(github_node_id)
    WHERE github_node_id IS NOT NULL;

CREATE TABLE pull_requests (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    github_id INTEGER CHECK (github_id IS NULL OR github_id > 0),
    number INTEGER NOT NULL CHECK (number > 0),
    title TEXT,
    author_login TEXT,
    html_url TEXT,
    state TEXT NOT NULL DEFAULT 'unknown' CHECK (
        state IN ('unknown', 'open', 'closed', 'merged')
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    UNIQUE (repository_id, number),
    UNIQUE (id, repository_id)
);

CREATE UNIQUE INDEX idx_pull_requests_github_id
    ON pull_requests(github_id)
    WHERE github_id IS NOT NULL;

CREATE INDEX idx_pull_requests_repository_state
    ON pull_requests(repository_id, state, number);

CREATE TABLE revisions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    pull_request_id TEXT NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
    identity_kind TEXT NOT NULL CHECK (
        identity_kind IN ('legacy_sha_pair', 'synthetic_legacy', 'canonical_diff')
    ),
    identity_key TEXT NOT NULL CHECK (length(identity_key) > 0),
    head_sha TEXT,
    base_sha TEXT,
    diff_sha256 TEXT CHECK (
        diff_sha256 IS NULL
        OR (
            length(diff_sha256) = 64
            AND diff_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    is_publishable INTEGER NOT NULL DEFAULT 0 CHECK (is_publishable IN (0, 1)),
    observed_at_us INTEGER NOT NULL CHECK (observed_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (
            identity_kind = 'legacy_sha_pair'
            AND is_publishable = 0
            AND head_sha IS NOT NULL
            AND length(head_sha) = 40
            AND head_sha NOT GLOB '*[^0-9a-f]*'
            AND base_sha IS NOT NULL
            AND length(base_sha) = 40
            AND base_sha NOT GLOB '*[^0-9a-f]*'
        )
        OR
        (identity_kind = 'synthetic_legacy' AND is_publishable = 0)
        OR
        (
            identity_kind = 'canonical_diff'
            AND head_sha IS NOT NULL
            AND length(head_sha) = 40
            AND head_sha NOT GLOB '*[^0-9a-f]*'
            AND base_sha IS NOT NULL
            AND length(base_sha) = 40
            AND base_sha NOT GLOB '*[^0-9a-f]*'
            AND diff_sha256 IS NOT NULL
        )
    ),
    UNIQUE (pull_request_id, identity_kind, identity_key),
    UNIQUE (id, pull_request_id)
);

CREATE INDEX idx_revisions_pull_request_observed
    ON revisions(pull_request_id, observed_at_us DESC, id);

CREATE INDEX idx_revisions_head_sha
    ON revisions(pull_request_id, head_sha)
    WHERE head_sha IS NOT NULL;

CREATE UNIQUE INDEX idx_revisions_legacy_sha_pair
    ON revisions(pull_request_id, head_sha, base_sha)
    WHERE identity_kind = 'legacy_sha_pair';

CREATE UNIQUE INDEX idx_revisions_canonical_diff
    ON revisions(pull_request_id, head_sha, base_sha, diff_sha256)
    WHERE identity_kind = 'canonical_diff';

CREATE TABLE migration_ledger (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    source_id TEXT NOT NULL REFERENCES legacy_sources(id) ON DELETE RESTRICT,
    snapshot_id TEXT NOT NULL,
    source_table TEXT NOT NULL CHECK (length(source_table) > 0),
    source_pk TEXT NOT NULL CHECK (length(source_pk) > 0),
    row_checksum TEXT NOT NULL CHECK (
        length(row_checksum) = 64
        AND row_checksum NOT GLOB '*[^0-9a-f]*'
    ),
    row_format_version INTEGER NOT NULL CHECK (row_format_version = 1),
    raw_json BLOB NOT NULL CHECK (
        json_valid(raw_json)
        AND json_type(raw_json) = 'object'
    ),
    warnings_json BLOB NOT NULL DEFAULT '[]' CHECK (
        json_valid(warnings_json)
        AND json_type(warnings_json) = 'array'
    ),
    repository_id TEXT,
    pull_request_id TEXT,
    revision_id TEXT,
    imported_at_us INTEGER NOT NULL CHECK (imported_at_us >= 0),
    CHECK (pull_request_id IS NULL OR repository_id IS NOT NULL),
    CHECK (revision_id IS NULL OR pull_request_id IS NOT NULL),
    UNIQUE (source_id, source_table, source_pk),
    UNIQUE (id, source_id, source_table, source_pk, row_checksum),
    FOREIGN KEY (snapshot_id, source_id)
        REFERENCES legacy_snapshots(id, source_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT
);

CREATE INDEX idx_migration_ledger_snapshot
    ON migration_ledger(snapshot_id, source_table, source_pk);

CREATE INDEX idx_migration_ledger_repository
    ON migration_ledger(repository_id)
    WHERE repository_id IS NOT NULL;

CREATE INDEX idx_migration_ledger_pull_request
    ON migration_ledger(pull_request_id)
    WHERE pull_request_id IS NOT NULL;

CREATE INDEX idx_migration_ledger_revision
    ON migration_ledger(revision_id)
    WHERE revision_id IS NOT NULL;

CREATE TABLE legacy_snapshot_rows (
    snapshot_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    table_name TEXT NOT NULL CHECK (length(table_name) > 0),
    source_pk TEXT NOT NULL CHECK (length(source_pk) > 0),
    row_checksum TEXT NOT NULL CHECK (
        length(row_checksum) = 64
        AND row_checksum NOT GLOB '*[^0-9a-f]*'
    ),
    ledger_id TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, table_name, source_pk),
    FOREIGN KEY (snapshot_id, source_id)
        REFERENCES legacy_snapshots(id, source_id) ON DELETE RESTRICT,
    FOREIGN KEY (ledger_id, source_id, table_name, source_pk, row_checksum)
        REFERENCES migration_ledger(
            id,
            source_id,
            source_table,
            source_pk,
            row_checksum
        ) ON DELETE RESTRICT
);

CREATE INDEX idx_legacy_snapshot_rows_ledger
    ON legacy_snapshot_rows(ledger_id);
