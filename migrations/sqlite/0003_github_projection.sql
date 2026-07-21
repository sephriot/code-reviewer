ALTER TABLE repositories ADD COLUMN github_id INTEGER
    CHECK (github_id IS NULL OR github_id > 0);

CREATE UNIQUE INDEX idx_repositories_github_id
    ON repositories(github_id)
    WHERE github_id IS NOT NULL;

CREATE UNIQUE INDEX idx_repositories_id_github_id
    ON repositories(id, github_id);

CREATE UNIQUE INDEX idx_repositories_id_github_node_id
    ON repositories(id, github_node_id);

CREATE TABLE connections (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    provider TEXT NOT NULL CHECK (provider = 'github'),
    mode TEXT NOT NULL CHECK (mode IN ('local_user', 'github_app')),
    auth_kind TEXT NOT NULL CHECK (auth_kind IN (
        'github_cli',
        'fine_grained_pat',
        'github_app_user',
        'github_app_installation'
    )),
    api_base_url TEXT NOT NULL CHECK (
        (
            api_base_url GLOB 'https://*'
            OR (
                substr(api_base_url, 1, 16) = 'http://localhost'
                AND substr(api_base_url, 17, 1) IN ('', ':', '/')
            )
            OR (
                substr(api_base_url, 1, 16) = 'http://127.0.0.1'
                AND substr(api_base_url, 17, 1) IN ('', ':', '/')
            )
            OR (
                substr(api_base_url, 1, 12) = 'http://[::1]'
                AND substr(api_base_url, 13, 1) IN ('', ':', '/')
            )
        )
        AND instr(api_base_url, ' ') = 0
    ),
    account_login TEXT NOT NULL COLLATE NOCASE CHECK (length(account_login) > 0),
    account_node_id TEXT,
    account_database_id INTEGER CHECK (
        account_database_id IS NULL OR account_database_id > 0
    ),
    app_id INTEGER CHECK (app_id IS NULL OR app_id > 0),
    installation_id INTEGER CHECK (installation_id IS NULL OR installation_id > 0),
    credential_ref_kind TEXT NOT NULL CHECK (credential_ref_kind IN (
        'github_cli',
        'environment',
        'file',
        'keychain',
        'secret_manager'
    )),
    credential_locator TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'unverified',
        'active',
        'degraded',
        'invalid',
        'revoked'
    )),
    permissions_json BLOB NOT NULL DEFAULT '{}' CHECK (
        json_valid(permissions_json)
        AND json_type(permissions_json) = 'object'
    ),
    last_checked_at_us INTEGER CHECK (last_checked_at_us IS NULL OR last_checked_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    CHECK (
        (mode = 'local_user' AND auth_kind IN (
            'github_cli', 'fine_grained_pat', 'github_app_user'
        ) AND app_id IS NULL AND installation_id IS NULL)
        OR
        (mode = 'github_app' AND auth_kind = 'github_app_installation'
         AND app_id IS NOT NULL AND installation_id IS NOT NULL)
    ),
    CHECK (
        state <> 'active'
        OR account_database_id IS NOT NULL
    ),
    CHECK (
        (credential_ref_kind = 'github_cli' AND credential_locator = 'github-cli')
        OR
        (credential_ref_kind = 'environment'
         AND substr(credential_locator, 1, 4) = 'env:'
         AND length(credential_locator) > 4)
        OR
        (credential_ref_kind = 'file'
         AND substr(credential_locator, 1, 5) = 'file:'
         AND length(credential_locator) > 5)
        OR
        (credential_ref_kind = 'keychain'
         AND substr(credential_locator, 1, 9) = 'keychain:'
         AND length(credential_locator) > 9)
        OR
        (credential_ref_kind = 'secret_manager'
         AND substr(credential_locator, 1, 15) = 'secret-manager:'
         AND length(credential_locator) > 15)
    ),
    UNIQUE (provider, api_base_url, account_login, mode, installation_id),
    UNIQUE (id, provider)
);

CREATE INDEX idx_connections_state
    ON connections(state, account_login, id);

CREATE UNIQUE INDEX idx_connections_account_database_id
    ON connections(provider, api_base_url, account_database_id, mode)
    WHERE account_database_id IS NOT NULL;

CREATE UNIQUE INDEX idx_connections_account_node_id
    ON connections(provider, api_base_url, account_node_id, mode)
    WHERE account_node_id IS NOT NULL;

CREATE UNIQUE INDEX idx_connections_installation_id
    ON connections(provider, api_base_url, installation_id)
    WHERE installation_id IS NOT NULL;

CREATE TABLE reconciliation_generations (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN (
        'review_requested_search',
        'authored_search',
        'watched_repository',
        'installation'
    )),
    scope_key TEXT NOT NULL CHECK (length(scope_key) > 0),
    query_partition TEXT NOT NULL DEFAULT 'all' CHECK (length(query_partition) > 0),
    generation_number INTEGER NOT NULL CHECK (generation_number > 0),
    mode TEXT NOT NULL CHECK (mode = 'shadow_read_only'),
    state TEXT NOT NULL CHECK (state IN (
        'running',
        'complete',
        'partial',
        'capped',
        'rate_limited',
        'failed'
    )),
    pages_expected INTEGER CHECK (pages_expected IS NULL OR pages_expected >= 0),
    pages_received INTEGER NOT NULL DEFAULT 0 CHECK (pages_received >= 0),
    provider_incomplete_results INTEGER NOT NULL DEFAULT 0 CHECK (
        provider_incomplete_results IN (0, 1)
    ),
    provider_total INTEGER CHECK (provider_total IS NULL OR provider_total >= 0),
    result_count INTEGER NOT NULL DEFAULT 0 CHECK (result_count >= 0),
    coverage_sha256 TEXT CHECK (
        coverage_sha256 IS NULL
        OR (
            length(coverage_sha256) = 64
            AND coverage_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    error_class TEXT,
    error_message TEXT,
    started_at_us INTEGER NOT NULL CHECK (started_at_us >= 0),
    finished_at_us INTEGER CHECK (
        finished_at_us IS NULL OR finished_at_us >= started_at_us
    ),
    CHECK (
        (state = 'running'
         AND finished_at_us IS NULL
         AND coverage_sha256 IS NULL
         AND error_class IS NULL
         AND error_message IS NULL)
        OR
        (state = 'complete'
         AND finished_at_us IS NOT NULL
         AND pages_expected IS NOT NULL
         AND pages_received = pages_expected
         AND provider_incomplete_results = 0
         AND coverage_sha256 IS NOT NULL
         AND error_class IS NULL
         AND error_message IS NULL)
        OR
        (state IN ('partial', 'capped', 'rate_limited', 'failed')
         AND finished_at_us IS NOT NULL
         AND coverage_sha256 IS NULL)
    ),
    UNIQUE (
        connection_id,
        scope_kind,
        scope_key,
        query_partition,
        generation_number
    ),
    UNIQUE (
        id,
        connection_id,
        scope_kind,
        scope_key,
        query_partition
    ),
    UNIQUE (id, connection_id)
);

CREATE INDEX idx_reconciliation_generations_scope
    ON reconciliation_generations(
        connection_id,
        scope_kind,
        scope_key,
        query_partition,
        generation_number DESC
    );

CREATE INDEX idx_reconciliation_generations_state
    ON reconciliation_generations(state, started_at_us, id);

CREATE TABLE reconciliation_checkpoints (
    connection_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    query_partition TEXT NOT NULL DEFAULT 'all',
    last_attempt_generation_id TEXT NOT NULL,
    last_complete_generation_id TEXT,
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= 0),
    PRIMARY KEY (connection_id, scope_kind, scope_key, query_partition),
    FOREIGN KEY (
        last_attempt_generation_id,
        connection_id,
        scope_kind,
        scope_key,
        query_partition
    ) REFERENCES reconciliation_generations(
        id,
        connection_id,
        scope_kind,
        scope_key,
        query_partition
    ) ON DELETE RESTRICT,
    FOREIGN KEY (
        last_complete_generation_id,
        connection_id,
        scope_kind,
        scope_key,
        query_partition
    ) REFERENCES reconciliation_generations(
        id,
        connection_id,
        scope_kind,
        scope_key,
        query_partition
    ) ON DELETE RESTRICT
);

CREATE TABLE connection_repositories (
    connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    github_repository_id INTEGER NOT NULL CHECK (github_repository_id > 0),
    github_node_id TEXT NOT NULL CHECK (length(github_node_id) > 0),
    installation_id INTEGER CHECK (installation_id IS NULL OR installation_id > 0),
    access_state TEXT NOT NULL CHECK (access_state IN (
        'active', 'inaccessible', 'removed'
    )),
    permissions_json BLOB NOT NULL DEFAULT '{}' CHECK (
        json_valid(permissions_json)
        AND json_type(permissions_json) = 'object'
    ),
    last_seen_generation_id TEXT,
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    PRIMARY KEY (connection_id, repository_id),
    UNIQUE (connection_id, github_repository_id),
    UNIQUE (connection_id, github_node_id),
    FOREIGN KEY (repository_id, github_repository_id)
        REFERENCES repositories(id, github_id) ON DELETE RESTRICT,
    FOREIGN KEY (repository_id, github_node_id)
        REFERENCES repositories(id, github_node_id) ON DELETE RESTRICT,
    FOREIGN KEY (last_seen_generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_connection_repositories_access
    ON connection_repositories(connection_id, access_state, repository_id);

CREATE TABLE pull_request_observations (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT,
    head_sha TEXT NOT NULL CHECK (
        length(head_sha) = 40
        AND head_sha NOT GLOB '*[^0-9a-f]*'
    ),
    base_sha TEXT NOT NULL CHECK (
        length(base_sha) = 40
        AND base_sha NOT GLOB '*[^0-9a-f]*'
    ),
    source_kind TEXT NOT NULL CHECK (source_kind IN (
        'reconciliation', 'webhook', 'direct_refresh'
    )),
    source_generation_id TEXT,
    source_priority INTEGER NOT NULL CHECK (
        (source_kind = 'reconciliation' AND source_priority = 10)
        OR (source_kind = 'webhook' AND source_priority = 20)
        OR (source_kind = 'direct_refresh' AND source_priority = 30)
    ),
    facts_format_version INTEGER NOT NULL CHECK (facts_format_version = 1),
    facts_sha256 TEXT NOT NULL CHECK (
        length(facts_sha256) = 64
        AND facts_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    title TEXT NOT NULL,
    author_login TEXT NOT NULL COLLATE NOCASE CHECK (length(author_login) > 0),
    author_database_id INTEGER NOT NULL CHECK (author_database_id > 0),
    body_sha256 TEXT NOT NULL CHECK (
        length(body_sha256) = 64
        AND body_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    labels_json BLOB NOT NULL CHECK (
        json_valid(labels_json)
        AND json_type(labels_json) = 'array'
    ),
    is_draft INTEGER NOT NULL CHECK (is_draft IN (0, 1)),
    base_ref TEXT NOT NULL CHECK (length(base_ref) > 0),
    requested_reviewers_json BLOB NOT NULL CHECK (
        json_valid(requested_reviewers_json)
        AND json_type(requested_reviewers_json) = 'array'
    ),
    relationship_set_json BLOB NOT NULL CHECK (
        json_valid(relationship_set_json)
        AND json_type(relationship_set_json) = 'array'
    ),
    github_state TEXT NOT NULL CHECK (github_state IN (
        'open', 'closed', 'merged'
    )),
    github_updated_at_us INTEGER NOT NULL CHECK (github_updated_at_us >= 0),
    observed_at_us INTEGER NOT NULL CHECK (observed_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (source_kind = 'reconciliation' AND source_generation_id IS NOT NULL)
        OR
        (source_kind IN ('webhook', 'direct_refresh') AND source_generation_id IS NULL)
    ),
    UNIQUE (pull_request_id, facts_format_version, facts_sha256),
    UNIQUE (id, pull_request_id),
    UNIQUE (id, pull_request_id, connection_id),
    UNIQUE (id, pull_request_id, connection_id, revision_id),
    UNIQUE (id, connection_id),
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (source_generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_pull_request_observations_current
    ON pull_request_observations(
        pull_request_id,
        github_updated_at_us DESC,
        source_priority DESC,
        observed_at_us DESC,
        id
    );

CREATE INDEX idx_pull_request_observations_generation
    ON pull_request_observations(source_generation_id, pull_request_id)
    WHERE source_generation_id IS NOT NULL;

CREATE TABLE reconciliation_generation_items (
    generation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    recorded_at_us INTEGER NOT NULL CHECK (recorded_at_us >= 0),
    PRIMARY KEY (generation_id, pull_request_id),
    FOREIGN KEY (generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT,
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(
            id, pull_request_id, connection_id
        ) ON DELETE RESTRICT
);

CREATE INDEX idx_reconciliation_generation_items_observation
    ON reconciliation_generation_items(observation_id, generation_id);

CREATE TABLE pr_relationships (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    relationship_kind TEXT NOT NULL CHECK (relationship_kind IN (
        'review_requested', 'authored_by_me', 'watched'
    )),
    subject_database_id INTEGER NOT NULL CHECK (subject_database_id > 0),
    subject_login TEXT NOT NULL COLLATE NOCASE CHECK (length(subject_login) > 0),
    source_kind TEXT NOT NULL CHECK (source_kind IN (
        'reconciliation', 'webhook', 'direct_refresh', 'configuration'
    )),
    started_observation_id TEXT NOT NULL,
    started_generation_id TEXT,
    active_from_us INTEGER NOT NULL CHECK (active_from_us >= 0),
    active_until_us INTEGER CHECK (
        active_until_us IS NULL OR active_until_us >= active_from_us
    ),
    ended_by_observation_id TEXT,
    ended_by_generation_id TEXT,
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    CHECK (
        (source_kind = 'reconciliation' AND started_generation_id IS NOT NULL)
        OR
        (source_kind IN ('webhook', 'direct_refresh', 'configuration')
         AND started_generation_id IS NULL)
    ),
    CHECK (
        (active_until_us IS NULL
         AND ended_by_observation_id IS NULL
         AND ended_by_generation_id IS NULL)
        OR
        (active_until_us IS NOT NULL
         AND (ended_by_observation_id IS NOT NULL OR ended_by_generation_id IS NOT NULL))
    ),
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (started_observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id) ON DELETE RESTRICT,
    FOREIGN KEY (ended_by_observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id) ON DELETE RESTRICT,
    FOREIGN KEY (started_generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT,
    FOREIGN KEY (ended_by_generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX idx_pr_relationships_active
    ON pr_relationships(
        connection_id,
        pull_request_id,
        relationship_kind,
        subject_database_id
    )
    WHERE active_until_us IS NULL;

CREATE INDEX idx_pr_relationships_pull_request_time
    ON pr_relationships(pull_request_id, active_from_us DESC, id);

CREATE TABLE pull_request_projection_state (
    pull_request_id TEXT PRIMARY KEY NOT NULL,
    repository_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    current_revision_id TEXT,
    current_observation_id TEXT NOT NULL,
    last_complete_generation_id TEXT,
    freshness TEXT NOT NULL CHECK (freshness IN ('unknown', 'fresh', 'stale')),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= 0),
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (current_revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (current_observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(
            id, pull_request_id, connection_id
        ) ON DELETE RESTRICT,
    FOREIGN KEY (last_complete_generation_id, connection_id)
        REFERENCES reconciliation_generations(id, connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_pull_request_projection_freshness
    ON pull_request_projection_state(freshness, updated_at_us, pull_request_id);

CREATE TRIGGER trg_reconciliation_generation_identity_immutable
BEFORE UPDATE ON reconciliation_generations
WHEN OLD.id IS NOT NEW.id
  OR OLD.connection_id IS NOT NEW.connection_id
  OR OLD.scope_kind IS NOT NEW.scope_kind
  OR OLD.scope_key IS NOT NEW.scope_key
  OR OLD.query_partition IS NOT NEW.query_partition
  OR OLD.generation_number IS NOT NEW.generation_number
  OR OLD.mode IS NOT NEW.mode
BEGIN
    SELECT RAISE(ABORT, 'reconciliation generation identity is immutable');
END;

CREATE TRIGGER trg_reconciliation_generation_terminal_immutable
BEFORE UPDATE ON reconciliation_generations
WHEN OLD.state <> 'running'
BEGIN
    SELECT RAISE(ABORT, 'terminal reconciliation generation is immutable');
END;

CREATE TRIGGER trg_reconciliation_generation_no_delete
BEFORE DELETE ON reconciliation_generations
BEGIN
    SELECT RAISE(ABORT, 'reconciliation generation is immutable');
END;

CREATE TRIGGER trg_checkpoint_complete_generation_insert
BEFORE INSERT ON reconciliation_checkpoints
WHEN NEW.last_complete_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.last_complete_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND (
        SELECT COUNT(*)
        FROM reconciliation_generation_items AS item
        WHERE item.generation_id = generation.id
      ) = generation.result_count
 )
BEGIN
    SELECT RAISE(ABORT, 'checkpoint requires complete reconciliation generation');
END;

CREATE TRIGGER trg_checkpoint_complete_generation_update
BEFORE UPDATE OF last_complete_generation_id ON reconciliation_checkpoints
WHEN NEW.last_complete_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.last_complete_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND (
        SELECT COUNT(*)
        FROM reconciliation_generation_items AS item
        WHERE item.generation_id = generation.id
      ) = generation.result_count
 )
BEGIN
    SELECT RAISE(ABORT, 'checkpoint requires complete reconciliation generation');
END;

CREATE TRIGGER trg_checkpoint_generation_order_insert
BEFORE INSERT ON reconciliation_checkpoints
WHEN NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS attempted
    LEFT JOIN reconciliation_generations AS completed
      ON completed.id = NEW.last_complete_generation_id
    WHERE attempted.id = NEW.last_attempt_generation_id
      AND attempted.connection_id = NEW.connection_id
      AND (
        (NEW.last_complete_generation_id IS NULL
         AND attempted.state <> 'complete')
        OR
        (NEW.last_complete_generation_id IS NOT NULL
         AND completed.state = 'complete'
         AND completed.generation_number <= attempted.generation_number
         AND (attempted.state <> 'complete' OR completed.id = attempted.id))
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'checkpoint generation order is invalid');
END;

CREATE TRIGGER trg_checkpoint_generation_order_update
BEFORE UPDATE ON reconciliation_checkpoints
WHEN OLD.connection_id IS NOT NEW.connection_id
  OR OLD.scope_kind IS NOT NEW.scope_kind
  OR OLD.scope_key IS NOT NEW.scope_key
  OR OLD.query_partition IS NOT NEW.query_partition
  OR NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS attempted
    JOIN reconciliation_generations AS previous_attempt
      ON previous_attempt.id = OLD.last_attempt_generation_id
    LEFT JOIN reconciliation_generations AS completed
      ON completed.id = NEW.last_complete_generation_id
    LEFT JOIN reconciliation_generations AS previous_complete
      ON previous_complete.id = OLD.last_complete_generation_id
    WHERE attempted.id = NEW.last_attempt_generation_id
      AND attempted.connection_id = NEW.connection_id
      AND attempted.generation_number >= previous_attempt.generation_number
      AND (
        (NEW.last_complete_generation_id IS NULL
         AND OLD.last_complete_generation_id IS NULL
         AND attempted.state <> 'complete')
        OR
        (NEW.last_complete_generation_id IS NOT NULL
         AND completed.state = 'complete'
         AND completed.generation_number <= attempted.generation_number
         AND (previous_complete.id IS NULL
              OR completed.generation_number >= previous_complete.generation_number)
         AND (attempted.state <> 'complete' OR completed.id = attempted.id))
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'checkpoint cannot change scope or move backward');
END;

CREATE TRIGGER trg_generation_item_positive_insert
BEFORE INSERT ON reconciliation_generation_items
WHEN NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state IN ('complete', 'partial', 'capped', 'rate_limited')
 )
 OR EXISTS (
    SELECT 1
    FROM reconciliation_checkpoints AS checkpoint
    WHERE checkpoint.last_attempt_generation_id = NEW.generation_id
 )
BEGIN
    SELECT RAISE(ABORT, 'generation membership is not open for positive results');
END;

CREATE TRIGGER trg_generation_item_immutable_update
BEFORE UPDATE ON reconciliation_generation_items
BEGIN
    SELECT RAISE(ABORT, 'reconciliation generation membership is immutable');
END;

CREATE TRIGGER trg_generation_item_immutable_delete
BEFORE DELETE ON reconciliation_generation_items
BEGIN
    SELECT RAISE(ABORT, 'reconciliation generation membership is immutable');
END;

CREATE TRIGGER trg_observation_positive_generation
BEFORE INSERT ON pull_request_observations
WHEN NEW.source_kind = 'reconciliation'
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.source_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state IN ('complete', 'partial', 'capped', 'rate_limited')
 )
BEGIN
    SELECT RAISE(ABORT, 'observation requires a finalized positive reconciliation generation');
END;

CREATE TRIGGER trg_observation_revision_identity_insert
BEFORE INSERT ON pull_request_observations
WHEN NEW.revision_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM revisions AS revision
    WHERE revision.id = NEW.revision_id
      AND revision.pull_request_id = NEW.pull_request_id
      AND revision.identity_kind = 'canonical_diff'
      AND revision.head_sha = NEW.head_sha
      AND revision.base_sha = NEW.base_sha
 )
BEGIN
    SELECT RAISE(ABORT, 'observation revision identity does not match canonical revision');
END;

CREATE TRIGGER trg_observation_immutable_update
BEFORE UPDATE ON pull_request_observations
BEGIN
    SELECT RAISE(ABORT, 'pull request observation is immutable');
END;

CREATE TRIGGER trg_observation_immutable_delete
BEFORE DELETE ON pull_request_observations
BEGIN
    SELECT RAISE(ABORT, 'pull request observation is immutable');
END;

CREATE TRIGGER trg_relationship_positive_start_generation
BEFORE INSERT ON pr_relationships
WHEN NEW.started_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.started_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state IN ('complete', 'partial', 'capped', 'rate_limited')
      AND EXISTS (
        SELECT 1
        FROM reconciliation_generation_items AS item
        WHERE item.generation_id = generation.id
          AND item.pull_request_id = NEW.pull_request_id
          AND item.observation_id = NEW.started_observation_id
      )
      AND (
        (NEW.relationship_kind = 'review_requested'
         AND generation.scope_kind = 'review_requested_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'authored_by_me'
         AND generation.scope_kind = 'authored_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'watched'
         AND generation.scope_kind = 'watched_repository')
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'relationship requires a finalized positive reconciliation generation');
END;

CREATE TRIGGER trg_relationship_subject_matches_connection
BEFORE INSERT ON pr_relationships
WHEN NOT EXISTS (
    SELECT 1
    FROM connections AS connection
    WHERE connection.id = NEW.connection_id
      AND connection.account_database_id = NEW.subject_database_id
)
BEGIN
    SELECT RAISE(ABORT, 'relationship subject does not match connection account');
END;

CREATE TRIGGER trg_relationship_complete_end_generation_insert
BEFORE INSERT ON pr_relationships
WHEN NEW.ended_by_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.ended_by_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND (
        (NEW.relationship_kind = 'review_requested'
         AND generation.scope_kind = 'review_requested_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'authored_by_me'
         AND generation.scope_kind = 'authored_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'watched'
         AND generation.scope_kind = 'watched_repository')
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'relationship end requires complete reconciliation generation');
END;

CREATE TRIGGER trg_relationship_complete_end_generation_update
BEFORE UPDATE OF ended_by_generation_id ON pr_relationships
WHEN NEW.ended_by_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.ended_by_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
      AND (
        (NEW.relationship_kind = 'review_requested'
         AND generation.scope_kind = 'review_requested_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'authored_by_me'
         AND generation.scope_kind = 'authored_search'
         AND generation.scope_key = NEW.subject_login COLLATE NOCASE)
        OR
        (NEW.relationship_kind = 'watched'
         AND generation.scope_kind = 'watched_repository')
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'relationship end requires complete reconciliation generation');
END;

CREATE TRIGGER trg_relationship_single_end_transition
BEFORE UPDATE ON pr_relationships
WHEN OLD.active_until_us IS NOT NULL
  OR NEW.active_until_us IS NULL
  OR OLD.id IS NOT NEW.id
  OR OLD.connection_id IS NOT NEW.connection_id
  OR OLD.repository_id IS NOT NEW.repository_id
  OR OLD.pull_request_id IS NOT NEW.pull_request_id
  OR OLD.relationship_kind IS NOT NEW.relationship_kind
  OR OLD.subject_database_id IS NOT NEW.subject_database_id
  OR OLD.subject_login IS NOT NEW.subject_login
  OR OLD.source_kind IS NOT NEW.source_kind
  OR OLD.started_observation_id IS NOT NEW.started_observation_id
  OR OLD.started_generation_id IS NOT NEW.started_generation_id
  OR OLD.active_from_us IS NOT NEW.active_from_us
  OR OLD.created_at_us IS NOT NEW.created_at_us
  OR NEW.updated_at_us < OLD.updated_at_us
BEGIN
    SELECT RAISE(ABORT, 'relationship permits one active-to-ended transition');
END;

CREATE TRIGGER trg_relationship_immutable_delete
BEFORE DELETE ON pr_relationships
BEGIN
    SELECT RAISE(ABORT, 'pull request relationship history is immutable');
END;

CREATE TRIGGER trg_revision_immutable_update
BEFORE UPDATE ON revisions
BEGIN
    SELECT RAISE(ABORT, 'revision is immutable');
END;

CREATE TRIGGER trg_revision_immutable_delete
BEFORE DELETE ON revisions
BEGIN
    SELECT RAISE(ABORT, 'revision is immutable');
END;

CREATE TRIGGER trg_connection_repository_installation_insert
BEFORE INSERT ON connection_repositories
WHEN NOT EXISTS (
    SELECT 1
    FROM connections AS connection
    WHERE connection.id = NEW.connection_id
      AND (
        (connection.mode = 'local_user' AND NEW.installation_id IS NULL)
        OR
        (connection.mode = 'github_app'
         AND NEW.installation_id = connection.installation_id)
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'repository installation does not match connection');
END;

CREATE TRIGGER trg_connection_repository_installation_update
BEFORE UPDATE OF connection_id, installation_id ON connection_repositories
WHEN NOT EXISTS (
    SELECT 1
    FROM connections AS connection
    WHERE connection.id = NEW.connection_id
      AND (
        (connection.mode = 'local_user' AND NEW.installation_id IS NULL)
        OR
        (connection.mode = 'github_app'
         AND NEW.installation_id = connection.installation_id)
      )
 )
BEGIN
    SELECT RAISE(ABORT, 'repository installation does not match connection');
END;

CREATE TRIGGER trg_connection_repository_identity_immutable
BEFORE UPDATE ON connection_repositories
WHEN OLD.connection_id IS NOT NEW.connection_id
  OR OLD.repository_id IS NOT NEW.repository_id
  OR OLD.github_repository_id IS NOT NEW.github_repository_id
  OR OLD.github_node_id IS NOT NEW.github_node_id
  OR OLD.installation_id IS NOT NEW.installation_id
  OR OLD.created_at_us IS NOT NEW.created_at_us
  OR NEW.updated_at_us < OLD.updated_at_us
BEGIN
    SELECT RAISE(ABORT, 'connection repository identity is immutable');
END;

CREATE TRIGGER trg_projection_requires_canonical_revision_insert
BEFORE INSERT ON pull_request_projection_state
WHEN NEW.current_revision_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM revisions AS revision
    WHERE revision.id = NEW.current_revision_id
      AND revision.pull_request_id = NEW.pull_request_id
      AND revision.identity_kind = 'canonical_diff'
 )
BEGIN
    SELECT RAISE(ABORT, 'current projection revision must be canonical');
END;

CREATE TRIGGER trg_projection_requires_canonical_revision_update
BEFORE UPDATE OF current_revision_id ON pull_request_projection_state
WHEN NEW.current_revision_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM revisions AS revision
    WHERE revision.id = NEW.current_revision_id
      AND revision.pull_request_id = NEW.pull_request_id
      AND revision.identity_kind = 'canonical_diff'
 )
BEGIN
    SELECT RAISE(ABORT, 'current projection revision must be canonical');
END;

CREATE TRIGGER trg_projection_observation_revision_insert
BEFORE INSERT ON pull_request_projection_state
WHEN NOT EXISTS (
    SELECT 1
    FROM pull_request_observations AS observation
    WHERE observation.id = NEW.current_observation_id
      AND observation.pull_request_id = NEW.pull_request_id
      AND observation.connection_id = NEW.connection_id
      AND observation.revision_id IS NEW.current_revision_id
 )
BEGIN
    SELECT RAISE(ABORT, 'projection revision must match current observation');
END;

CREATE TRIGGER trg_projection_observation_revision_update
BEFORE UPDATE OF current_revision_id, current_observation_id
ON pull_request_projection_state
WHEN NOT EXISTS (
    SELECT 1
    FROM pull_request_observations AS observation
    WHERE observation.id = NEW.current_observation_id
      AND observation.pull_request_id = NEW.pull_request_id
      AND observation.connection_id = NEW.connection_id
      AND observation.revision_id IS NEW.current_revision_id
 )
BEGIN
    SELECT RAISE(ABORT, 'projection revision must match current observation');
END;

CREATE TRIGGER trg_projection_complete_generation_insert
BEFORE INSERT ON pull_request_projection_state
WHEN NEW.last_complete_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.last_complete_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
 )
BEGIN
    SELECT RAISE(ABORT, 'projection requires complete reconciliation generation');
END;

CREATE TRIGGER trg_projection_complete_generation_update
BEFORE UPDATE OF last_complete_generation_id ON pull_request_projection_state
WHEN NEW.last_complete_generation_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM reconciliation_generations AS generation
    WHERE generation.id = NEW.last_complete_generation_id
      AND generation.connection_id = NEW.connection_id
      AND generation.state = 'complete'
 )
BEGIN
    SELECT RAISE(ABORT, 'projection requires complete reconciliation generation');
END;

CREATE TRIGGER trg_projection_observation_monotonic
BEFORE UPDATE OF current_observation_id ON pull_request_projection_state
WHEN EXISTS (
    SELECT 1
    FROM pull_request_observations AS previous
    JOIN pull_request_observations AS candidate
      ON candidate.id = NEW.current_observation_id
    WHERE previous.id = OLD.current_observation_id
      AND (
        candidate.github_updated_at_us < previous.github_updated_at_us
        OR (
            candidate.github_updated_at_us = previous.github_updated_at_us
            AND candidate.source_priority < previous.source_priority
        )
        OR (
            candidate.github_updated_at_us = previous.github_updated_at_us
            AND candidate.source_priority = previous.source_priority
            AND candidate.observed_at_us < previous.observed_at_us
        )
        OR (
            candidate.github_updated_at_us = previous.github_updated_at_us
            AND candidate.source_priority = previous.source_priority
            AND candidate.observed_at_us = previous.observed_at_us
            AND candidate.id <= previous.id
        )
      )
 )
 OR NEW.updated_at_us < OLD.updated_at_us
BEGIN
    SELECT RAISE(ABORT, 'current observation cannot move backward');
END;

CREATE TRIGGER trg_projection_identity_immutable
BEFORE UPDATE ON pull_request_projection_state
WHEN OLD.pull_request_id IS NOT NEW.pull_request_id
  OR OLD.repository_id IS NOT NEW.repository_id
  OR OLD.connection_id IS NOT NEW.connection_id
BEGIN
    SELECT RAISE(ABORT, 'pull request projection identity is immutable');
END;
