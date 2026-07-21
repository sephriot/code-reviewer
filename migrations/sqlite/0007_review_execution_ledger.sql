-- Review execution is an append-only evidence chain. Mutable queue state stays
-- in jobs; profile selection, engine context, output, and normalization records
-- are permanently tied to one canonical revision and its fact observation.
CREATE TABLE review_profiles (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    profile_key TEXT NOT NULL COLLATE NOCASE CHECK (length(profile_key) > 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (profile_key),
    UNIQUE (id, profile_key)
);

CREATE TABLE review_profile_versions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    profile_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    name TEXT NOT NULL CHECK (length(name) > 0),
    description TEXT NOT NULL,
    instructions TEXT NOT NULL CHECK (length(instructions) > 0),
    output_schema_version INTEGER NOT NULL CHECK (output_schema_version = 1),
    settings_json BLOB NOT NULL CHECK (
        json_valid(settings_json)
        AND json_type(settings_json) = 'object'
    ),
    content_sha256 TEXT NOT NULL CHECK (
        length(content_sha256) = 64
        AND content_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (profile_id, version),
    UNIQUE (id, profile_id),
    FOREIGN KEY (profile_id) REFERENCES review_profiles(id) ON DELETE RESTRICT
);

CREATE INDEX idx_review_profile_versions_profile
    ON review_profile_versions(profile_id, version DESC, id);

CREATE TABLE review_intents (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    profile_id TEXT NOT NULL,
    profile_version_id TEXT NOT NULL,
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN (
        'automatic', 'manual', 'on_demand', 'retry'
    )),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) > 0),
    trigger_sha256 TEXT NOT NULL CHECK (
        length(trigger_sha256) = 64
        AND trigger_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    user_context_sha256 TEXT CHECK (
        user_context_sha256 IS NULL OR (
            length(user_context_sha256) = 64
            AND user_context_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    correlation_id TEXT,
    requested_at_us INTEGER NOT NULL CHECK (requested_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (created_at_us >= requested_at_us),
    UNIQUE (idempotency_key),
    UNIQUE (id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (profile_version_id, profile_id)
        REFERENCES review_profile_versions(id, profile_id) ON DELETE RESTRICT
);

CREATE INDEX idx_review_intents_revision
    ON review_intents(pull_request_id, revision_id, profile_version_id, requested_at_us DESC, id);

CREATE TABLE review_runs (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    intent_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    engine_kind TEXT NOT NULL CHECK (engine_kind IN ('cli', 'api')),
    engine_config_json BLOB NOT NULL CHECK (
        json_valid(engine_config_json)
        AND json_type(engine_config_json) = 'object'
    ),
    started_at_us INTEGER NOT NULL CHECK (started_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (created_at_us <= started_at_us),
    UNIQUE (intent_id, attempt_number),
    UNIQUE (id, intent_id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (intent_id, pull_request_id, revision_id, observation_id)
        REFERENCES review_intents(id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_review_runs_revision
    ON review_runs(pull_request_id, revision_id, started_at_us DESC, id);

CREATE TABLE review_run_events (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    run_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    event_kind TEXT NOT NULL CHECK (event_kind IN (
        'queued', 'preparing', 'running', 'validating', 'succeeded',
        'failed_retryable', 'failed_terminal', 'canceled', 'superseded'
    )),
    payload_json BLOB NOT NULL DEFAULT '{}' CHECK (
        json_valid(payload_json)
        AND json_type(payload_json) = 'object'
    ),
    occurred_at_us INTEGER NOT NULL CHECK (occurred_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (created_at_us >= occurred_at_us),
    UNIQUE (run_id, sequence),
    FOREIGN KEY (run_id) REFERENCES review_runs(id) ON DELETE RESTRICT
);

CREATE INDEX idx_review_run_events_timeline
    ON review_run_events(run_id, sequence, occurred_at_us, id);

CREATE TABLE review_run_contexts (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    context_format_version INTEGER NOT NULL CHECK (context_format_version = 1),
    access_mode TEXT NOT NULL CHECK (access_mode IN (
        'diff_only', 'selected_files', 'read_only_worktree'
    )),
    manifest_sha256 TEXT NOT NULL CHECK (
        length(manifest_sha256) = 64
        AND manifest_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    manifest_json BLOB NOT NULL CHECK (
        json_valid(manifest_json)
        AND json_type(manifest_json) = 'object'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (run_id),
    UNIQUE (id, run_id, intent_id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (run_id, intent_id, pull_request_id, revision_id, observation_id)
        REFERENCES review_runs(id, intent_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_review_run_contexts_revision
    ON review_run_contexts(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE assessments (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version = 1),
    verdict TEXT NOT NULL CHECK (verdict IN (
        'pass', 'concerns', 'changes_required', 'inconclusive'
    )),
    summary TEXT NOT NULL CHECK (length(summary) > 0),
    confidence TEXT NOT NULL CHECK (confidence IN ('high', 'medium', 'low')),
    limitations_json BLOB NOT NULL CHECK (
        json_valid(limitations_json)
        AND json_type(limitations_json) = 'array'
    ),
    coverage_json BLOB NOT NULL CHECK (
        json_valid(coverage_json)
        AND json_type(coverage_json) = 'object'
        AND json_type(coverage_json, '$.status') IS 'text'
        AND json_extract(coverage_json, '$.status') IN ('complete', 'partial', 'unknown')
        AND json_type(coverage_json, '$.changed_files_total') IS 'integer'
        AND json_extract(coverage_json, '$.changed_files_total') >= 0
        AND json_type(coverage_json, '$.reviewed_files') IS 'integer'
        AND json_extract(coverage_json, '$.reviewed_files') >= 0
        AND json_extract(coverage_json, '$.reviewed_files') <= json_extract(coverage_json, '$.changed_files_total')
        AND json_type(coverage_json, '$.omitted') IS 'array'
    ),
    output_sha256 TEXT NOT NULL CHECK (
        length(output_sha256) = 64
        AND output_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (run_id),
    UNIQUE (id, run_id, pull_request_id, revision_id, observation_id),
    UNIQUE (id, run_id, intent_id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (run_id, intent_id, pull_request_id, revision_id, observation_id)
        REFERENCES review_runs(id, intent_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_assessments_revision
    ON assessments(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE findings (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    client_id TEXT NOT NULL CHECK (length(client_id) > 0),
    fingerprint_sha256 TEXT NOT NULL CHECK (
        length(fingerprint_sha256) = 64
        AND fingerprint_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    severity TEXT NOT NULL CHECK (severity IN (
        'blocker', 'high', 'medium', 'low', 'note'
    )),
    category TEXT NOT NULL CHECK (category IN (
        'correctness', 'security', 'performance', 'testing', 'maintainability', 'other'
    )),
    path TEXT CHECK (path IS NULL OR length(path) > 0),
    line INTEGER CHECK (line IS NULL OR line > 0),
    side TEXT CHECK (side IS NULL OR side IN ('LEFT', 'RIGHT')),
    message TEXT NOT NULL CHECK (length(message) > 0),
    evidence TEXT,
    suggestion TEXT,
    anchor_status TEXT NOT NULL CHECK (anchor_status IN (
        'valid', 'downgraded', 'unanchored'
    )),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (anchor_status = 'valid'
         AND path IS NOT NULL AND line IS NOT NULL AND side IS NOT NULL)
        OR
        (anchor_status IN ('downgraded', 'unanchored')
         AND line IS NULL AND side IS NULL)
    ),
    UNIQUE (assessment_id, client_id),
    UNIQUE (assessment_id, fingerprint_sha256),
    UNIQUE (id, assessment_id, run_id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (assessment_id, run_id, pull_request_id, revision_id, observation_id)
        REFERENCES assessments(id, run_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_findings_revision_severity
    ON findings(pull_request_id, revision_id, severity, created_at_us DESC, id);

CREATE TABLE validation_warnings (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    warning_code TEXT NOT NULL CHECK (length(warning_code) > 0),
    message TEXT NOT NULL CHECK (length(message) > 0),
    details_json BLOB NOT NULL DEFAULT '{}' CHECK (
        json_valid(details_json)
        AND json_type(details_json) = 'object'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (assessment_id, warning_code, details_json),
    FOREIGN KEY (assessment_id, run_id, pull_request_id, revision_id, observation_id)
        REFERENCES assessments(id, run_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_validation_warnings_assessment
    ON validation_warnings(assessment_id, created_at_us, id);

CREATE TRIGGER trg_review_intent_canonical_evidence
BEFORE INSERT ON review_intents
WHEN NOT EXISTS (
    SELECT 1
    FROM revisions AS revision
    JOIN pull_request_observations AS observation
      ON observation.id = NEW.observation_id
     AND observation.pull_request_id = NEW.pull_request_id
     AND observation.connection_id = NEW.connection_id
    WHERE revision.id = NEW.revision_id
      AND revision.pull_request_id = NEW.pull_request_id
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND revision.head_sha = observation.head_sha
      AND revision.base_sha = observation.base_sha
      AND (
        observation.revision_id = revision.id
        OR EXISTS (
            SELECT 1
            FROM observation_revision_links AS link
            WHERE link.observation_id = observation.id
              AND link.pull_request_id = observation.pull_request_id
              AND link.connection_id = observation.connection_id
              AND link.revision_id = revision.id
        )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'review intent requires matching canonical revision evidence');
END;

CREATE TRIGGER trg_review_profiles_immutable_update
BEFORE UPDATE ON review_profiles
BEGIN
    SELECT RAISE(ABORT, 'review profile is immutable');
END;

CREATE TRIGGER trg_review_profiles_immutable_delete
BEFORE DELETE ON review_profiles
BEGIN
    SELECT RAISE(ABORT, 'review profile is immutable');
END;

CREATE TRIGGER trg_review_profile_versions_immutable_update
BEFORE UPDATE ON review_profile_versions
BEGIN
    SELECT RAISE(ABORT, 'review profile version is immutable');
END;

CREATE TRIGGER trg_review_profile_versions_immutable_delete
BEFORE DELETE ON review_profile_versions
BEGIN
    SELECT RAISE(ABORT, 'review profile version is immutable');
END;

CREATE TRIGGER trg_review_intents_immutable_update
BEFORE UPDATE ON review_intents
BEGIN
    SELECT RAISE(ABORT, 'review intent is immutable');
END;

CREATE TRIGGER trg_review_intents_immutable_delete
BEFORE DELETE ON review_intents
BEGIN
    SELECT RAISE(ABORT, 'review intent is immutable');
END;

CREATE TRIGGER trg_review_runs_immutable_update
BEFORE UPDATE ON review_runs
BEGIN
    SELECT RAISE(ABORT, 'review run is immutable');
END;

CREATE TRIGGER trg_review_runs_immutable_delete
BEFORE DELETE ON review_runs
BEGIN
    SELECT RAISE(ABORT, 'review run is immutable');
END;

CREATE TRIGGER trg_review_run_events_immutable_update
BEFORE UPDATE ON review_run_events
BEGIN
    SELECT RAISE(ABORT, 'review run event is immutable');
END;

CREATE TRIGGER trg_review_run_events_immutable_delete
BEFORE DELETE ON review_run_events
BEGIN
    SELECT RAISE(ABORT, 'review run event is immutable');
END;

CREATE TRIGGER trg_review_run_contexts_immutable_update
BEFORE UPDATE ON review_run_contexts
BEGIN
    SELECT RAISE(ABORT, 'review run context is immutable');
END;

CREATE TRIGGER trg_review_run_contexts_immutable_delete
BEFORE DELETE ON review_run_contexts
BEGIN
    SELECT RAISE(ABORT, 'review run context is immutable');
END;

CREATE TRIGGER trg_assessments_immutable_update
BEFORE UPDATE ON assessments
BEGIN
    SELECT RAISE(ABORT, 'assessment is immutable');
END;

CREATE TRIGGER trg_assessments_immutable_delete
BEFORE DELETE ON assessments
BEGIN
    SELECT RAISE(ABORT, 'assessment is immutable');
END;

CREATE TRIGGER trg_findings_immutable_update
BEFORE UPDATE ON findings
BEGIN
    SELECT RAISE(ABORT, 'finding is immutable');
END;

CREATE TRIGGER trg_findings_immutable_delete
BEFORE DELETE ON findings
BEGIN
    SELECT RAISE(ABORT, 'finding is immutable');
END;

CREATE TRIGGER trg_validation_warnings_immutable_update
BEFORE UPDATE ON validation_warnings
BEGIN
    SELECT RAISE(ABORT, 'validation warning is immutable');
END;

CREATE TRIGGER trg_validation_warnings_immutable_delete
BEFORE DELETE ON validation_warnings
BEGIN
    SELECT RAISE(ABORT, 'validation warning is immutable');
END;
