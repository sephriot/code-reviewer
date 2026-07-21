-- Policy, proposal, decision, and publication records are append-only evidence.
-- Mutable rule selection is deliberately limited to a stable rule's enabled flag
-- and pointer to an immutable version. Publication attempts are outcomes, not
-- in-flight state; in-flight coordination remains in the durable jobs table.

CREATE TABLE policy_sets (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    generation INTEGER NOT NULL CHECK (generation > 0),
    content_sha256 TEXT NOT NULL CHECK (
        length(content_sha256) = 64
        AND content_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (generation),
    UNIQUE (content_sha256)
);

CREATE TABLE watch_rules (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    rule_key TEXT NOT NULL COLLATE NOCASE CHECK (length(rule_key) > 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    current_version_id TEXT,
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    UNIQUE (rule_key)
);

CREATE TABLE watch_rule_versions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    rule_id TEXT NOT NULL,
    policy_set_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    priority INTEGER NOT NULL CHECK (priority >= 0),
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN (
        'ignore', 'track_only', 'automatic', 'manual'
    )),
    external_action_policy TEXT NOT NULL CHECK (external_action_policy IN (
        'advisory_only', 'require_confirmation', 'auto_publish', 'human_attention'
    )),
    profile_id TEXT,
    profile_version_id TEXT,
    match_json BLOB NOT NULL CHECK (
        json_valid(match_json)
        AND json_type(match_json) = 'object'
    ),
    review_json BLOB NOT NULL CHECK (
        json_valid(review_json)
        AND json_type(review_json) = 'object'
    ),
    publication_json BLOB NOT NULL CHECK (
        json_valid(publication_json)
        AND json_type(publication_json) = 'object'
    ),
    content_sha256 TEXT NOT NULL CHECK (
        length(content_sha256) = 64
        AND content_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (profile_id IS NULL AND profile_version_id IS NULL)
        OR (profile_id IS NOT NULL AND profile_version_id IS NOT NULL)
    ),
    UNIQUE (rule_id, version),
    UNIQUE (policy_set_id, priority),
    UNIQUE (id, rule_id),
    FOREIGN KEY (rule_id) REFERENCES watch_rules(id) ON DELETE RESTRICT,
    FOREIGN KEY (policy_set_id) REFERENCES policy_sets(id) ON DELETE RESTRICT,
    FOREIGN KEY (profile_version_id, profile_id)
        REFERENCES review_profile_versions(id, profile_id) ON DELETE RESTRICT
);

CREATE INDEX idx_watch_rule_versions_policy_priority
    ON watch_rule_versions(policy_set_id, priority, rule_id, id);

CREATE INDEX idx_watch_rule_versions_rule
    ON watch_rule_versions(rule_id, version DESC, id);

CREATE TABLE policy_evaluations (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    policy_set_id TEXT NOT NULL,
    matched_rule_id TEXT,
    matched_rule_version_id TEXT,
    profile_id TEXT NOT NULL,
    profile_version_id TEXT NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN (
        'no_external_action', 'auto_publish_approval', 'propose_approval',
        'propose_comment', 'propose_changes', 'require_human_review'
    )),
    input_json BLOB NOT NULL CHECK (
        json_valid(input_json)
        AND json_type(input_json) = 'object'
    ),
    input_sha256 TEXT NOT NULL CHECK (
        length(input_sha256) = 64
        AND input_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    safety_overrides_json BLOB NOT NULL DEFAULT '[]' CHECK (
        json_valid(safety_overrides_json)
        AND json_type(safety_overrides_json) = 'array'
    ),
    rendered_output_sha256 TEXT CHECK (
        rendered_output_sha256 IS NULL OR (
            length(rendered_output_sha256) = 64
            AND rendered_output_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (matched_rule_id IS NULL AND matched_rule_version_id IS NULL)
        OR (matched_rule_id IS NOT NULL AND matched_rule_version_id IS NOT NULL)
    ),
    UNIQUE (assessment_id, input_sha256),
    UNIQUE (id, assessment_id, run_id, intent_id, pull_request_id, revision_id, observation_id),
    FOREIGN KEY (assessment_id, run_id, intent_id, pull_request_id, revision_id, observation_id)
        REFERENCES assessments(id, run_id, intent_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id) ON DELETE RESTRICT,
    FOREIGN KEY (policy_set_id) REFERENCES policy_sets(id) ON DELETE RESTRICT,
    FOREIGN KEY (matched_rule_version_id, matched_rule_id)
        REFERENCES watch_rule_versions(id, rule_id) ON DELETE RESTRICT,
    FOREIGN KEY (profile_version_id, profile_id)
        REFERENCES review_profile_versions(id, profile_id) ON DELETE RESTRICT
);

CREATE INDEX idx_policy_evaluations_revision
    ON policy_evaluations(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE proposals (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    policy_evaluation_id TEXT NOT NULL,
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    proposal_kind TEXT NOT NULL CHECK (proposal_kind IN (
        'approval', 'comment', 'changes'
    )),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (policy_evaluation_id),
    UNIQUE (id, policy_evaluation_id, assessment_id, run_id, intent_id,
        pull_request_id, revision_id, observation_id),
    FOREIGN KEY (policy_evaluation_id, assessment_id, run_id, intent_id,
        pull_request_id, revision_id, observation_id)
        REFERENCES policy_evaluations(id, assessment_id, run_id, intent_id,
            pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_proposals_revision
    ON proposals(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE proposal_revisions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    proposal_id TEXT NOT NULL,
    policy_evaluation_id TEXT NOT NULL,
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    revision_number INTEGER NOT NULL CHECK (revision_number > 0),
    editor_kind TEXT NOT NULL CHECK (editor_kind IN ('policy', 'human')),
    body TEXT NOT NULL,
    inline_comments_json BLOB NOT NULL DEFAULT '[]' CHECK (
        json_valid(inline_comments_json)
        AND json_type(inline_comments_json) = 'array'
    ),
    content_sha256 TEXT NOT NULL CHECK (
        length(content_sha256) = 64
        AND content_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (proposal_id, revision_number),
    UNIQUE (id, proposal_id, policy_evaluation_id, assessment_id, run_id, intent_id,
        pull_request_id, revision_id, observation_id),
    FOREIGN KEY (proposal_id, policy_evaluation_id, assessment_id, run_id, intent_id,
        pull_request_id, revision_id, observation_id)
        REFERENCES proposals(id, policy_evaluation_id, assessment_id, run_id, intent_id,
            pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_proposal_revisions_proposal
    ON proposal_revisions(proposal_id, revision_number DESC, id);

CREATE TABLE decisions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    proposal_id TEXT NOT NULL,
    proposal_revision_id TEXT NOT NULL,
    policy_evaluation_id TEXT NOT NULL,
    assessment_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    actor_kind TEXT NOT NULL CHECK (actor_kind IN ('human', 'policy')),
    actor_id TEXT NOT NULL CHECK (length(actor_id) > 0),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) > 0),
    reason TEXT,
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (proposal_revision_id),
    UNIQUE (idempotency_key),
    UNIQUE (id, proposal_revision_id),
    FOREIGN KEY (proposal_revision_id, proposal_id, policy_evaluation_id,
        assessment_id, run_id, intent_id, pull_request_id, revision_id, observation_id)
        REFERENCES proposal_revisions(id, proposal_id, policy_evaluation_id,
            assessment_id, run_id, intent_id, pull_request_id, revision_id, observation_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_decisions_revision
    ON decisions(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE publication_effects (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    owner_kind TEXT NOT NULL CHECK (owner_kind IN (
        'proposal_revision', 'operational_lifecycle'
    )),
    owner_id TEXT NOT NULL CHECK (length(owner_id) > 0),
    proposal_revision_id TEXT,
    authorization_decision_id TEXT,
    connection_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'review_approval', 'review_comment', 'review_changes',
        'marker_create', 'marker_delete'
    )),
    payload_json BLOB NOT NULL CHECK (
        json_valid(payload_json)
        AND json_type(payload_json) = 'object'
    ),
    payload_sha256 TEXT NOT NULL CHECK (
        length(payload_sha256) = 64
        AND payload_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) > 0),
    publication_mode_at_authorization TEXT NOT NULL CHECK (
        publication_mode_at_authorization IN ('disabled', 'simulated', 'enabled')
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (
        (owner_kind = 'proposal_revision'
         AND proposal_revision_id IS NOT NULL
         AND authorization_decision_id IS NOT NULL
         AND owner_id = proposal_revision_id)
        OR
        (owner_kind = 'operational_lifecycle'
         AND proposal_revision_id IS NULL
         AND authorization_decision_id IS NULL)
    ),
    UNIQUE (owner_kind, owner_id, revision_id, observation_id, effect_type, payload_sha256),
    UNIQUE (idempotency_key),
    FOREIGN KEY (proposal_revision_id) REFERENCES proposal_revisions(id) ON DELETE RESTRICT,
    FOREIGN KEY (authorization_decision_id) REFERENCES decisions(id) ON DELETE RESTRICT,
    FOREIGN KEY (connection_id, repository_id)
        REFERENCES connection_repositories(connection_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (pull_request_id, repository_id)
        REFERENCES pull_requests(id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id) ON DELETE RESTRICT
);

CREATE INDEX idx_publication_effects_revision
    ON publication_effects(pull_request_id, revision_id, created_at_us DESC, id);

CREATE TABLE publication_attempts (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    effect_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    publication_mode TEXT NOT NULL CHECK (publication_mode IN ('simulated', 'enabled')),
    outcome TEXT NOT NULL CHECK (outcome IN (
        'simulated', 'succeeded', 'failed_retryable', 'failed_terminal', 'uncertain'
    )),
    request_sha256 TEXT NOT NULL CHECK (
        length(request_sha256) = 64
        AND request_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    response_json BLOB NOT NULL DEFAULT '{}' CHECK (
        json_valid(response_json)
        AND json_type(response_json) = 'object'
    ),
    error_class TEXT,
    error_message TEXT,
    github_artifact_id TEXT,
    attempted_at_us INTEGER NOT NULL CHECK (attempted_at_us >= 0),
    completed_at_us INTEGER NOT NULL CHECK (completed_at_us >= attempted_at_us),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= completed_at_us),
    CHECK (
        (publication_mode = 'simulated' AND outcome = 'simulated')
        OR (publication_mode = 'enabled' AND outcome <> 'simulated')
    ),
    UNIQUE (effect_id, attempt_number),
    UNIQUE (id, effect_id),
    FOREIGN KEY (effect_id) REFERENCES publication_effects(id) ON DELETE RESTRICT
);

CREATE INDEX idx_publication_attempts_effect
    ON publication_attempts(effect_id, attempt_number DESC, id);

CREATE TRIGGER trg_watch_rules_mutable_fields_only
BEFORE UPDATE ON watch_rules
WHEN OLD.id IS NOT NEW.id
  OR OLD.rule_key IS NOT NEW.rule_key
  OR OLD.created_at_us IS NOT NEW.created_at_us
  OR NEW.updated_at_us < OLD.updated_at_us
BEGIN
    SELECT RAISE(ABORT, 'watch rule identity is immutable');
END;

CREATE TRIGGER trg_watch_rules_immutable_delete
BEFORE DELETE ON watch_rules
BEGIN
    SELECT RAISE(ABORT, 'watch rule is immutable');
END;

CREATE TRIGGER trg_watch_rules_current_version
BEFORE UPDATE OF current_version_id ON watch_rules
WHEN NEW.current_version_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM watch_rule_versions AS version
    WHERE version.id = NEW.current_version_id
      AND version.rule_id = NEW.id
 )
BEGIN
    SELECT RAISE(ABORT, 'watch rule current version must belong to rule');
END;

CREATE TRIGGER trg_policy_evaluation_current_evidence
BEFORE INSERT ON policy_evaluations
WHEN NOT EXISTS (
    SELECT 1
    FROM assessments AS assessment
    JOIN pull_request_projection_state AS projection
      ON projection.pull_request_id = NEW.pull_request_id
     AND projection.connection_id = NEW.connection_id
     AND projection.current_revision_id = NEW.revision_id
     AND projection.current_observation_id = NEW.observation_id
    JOIN revisions AS revision
      ON revision.id = NEW.revision_id
     AND revision.pull_request_id = NEW.pull_request_id
    JOIN revision_manifests AS manifest
      ON manifest.revision_id = revision.id
     AND manifest.pull_request_id = revision.pull_request_id
     AND manifest.manifest_sha256 = revision.diff_sha256
    JOIN pull_request_observations AS observation
      ON observation.id = NEW.observation_id
     AND observation.pull_request_id = NEW.pull_request_id
     AND observation.connection_id = NEW.connection_id
    WHERE assessment.id = NEW.assessment_id
      AND assessment.run_id = NEW.run_id
      AND assessment.intent_id = NEW.intent_id
      AND assessment.pull_request_id = NEW.pull_request_id
      AND assessment.revision_id = NEW.revision_id
      AND assessment.observation_id = NEW.observation_id
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND revision.head_sha = observation.head_sha
      AND revision.base_sha = observation.base_sha
      AND (
        observation.revision_id = revision.id
        OR EXISTS (
            SELECT 1 FROM observation_revision_links AS link
            WHERE link.observation_id = observation.id
              AND link.pull_request_id = observation.pull_request_id
              AND link.connection_id = observation.connection_id
              AND link.revision_id = revision.id
        )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'policy evaluation requires current canonical assessment evidence');
END;

CREATE TRIGGER trg_policy_evaluation_rule_generation
BEFORE INSERT ON policy_evaluations
WHEN NEW.matched_rule_version_id IS NOT NULL
 AND NOT EXISTS (
    SELECT 1
    FROM watch_rule_versions AS version
    WHERE version.id = NEW.matched_rule_version_id
      AND version.rule_id = NEW.matched_rule_id
      AND version.policy_set_id = NEW.policy_set_id
 )
BEGIN
    SELECT RAISE(ABORT, 'policy evaluation rule must belong to policy set');
END;

CREATE TRIGGER trg_decision_current_evidence
BEFORE INSERT ON decisions
WHEN NOT EXISTS (
    SELECT 1
    FROM proposal_revisions AS proposal_revision
    JOIN pull_request_projection_state AS projection
      ON projection.pull_request_id = NEW.pull_request_id
     AND projection.connection_id = NEW.connection_id
     AND projection.current_revision_id = NEW.revision_id
     AND projection.current_observation_id = NEW.observation_id
    WHERE proposal_revision.id = NEW.proposal_revision_id
      AND proposal_revision.proposal_id = NEW.proposal_id
      AND proposal_revision.policy_evaluation_id = NEW.policy_evaluation_id
      AND proposal_revision.assessment_id = NEW.assessment_id
      AND proposal_revision.run_id = NEW.run_id
      AND proposal_revision.intent_id = NEW.intent_id
      AND proposal_revision.pull_request_id = NEW.pull_request_id
      AND proposal_revision.revision_id = NEW.revision_id
      AND proposal_revision.observation_id = NEW.observation_id
)
BEGIN
    SELECT RAISE(ABORT, 'decision requires current proposal evidence');
END;

CREATE TRIGGER trg_publication_effect_current_authorization
BEFORE INSERT ON publication_effects
WHEN NOT EXISTS (
    SELECT 1
    FROM pull_request_projection_state AS projection
    JOIN revisions AS revision
      ON revision.id = NEW.revision_id
     AND revision.pull_request_id = NEW.pull_request_id
    JOIN revision_manifests AS manifest
      ON manifest.revision_id = revision.id
     AND manifest.pull_request_id = revision.pull_request_id
     AND manifest.manifest_sha256 = revision.diff_sha256
    JOIN pull_request_observations AS observation
      ON observation.id = NEW.observation_id
     AND observation.pull_request_id = NEW.pull_request_id
     AND observation.connection_id = NEW.connection_id
    WHERE projection.pull_request_id = NEW.pull_request_id
      AND projection.connection_id = NEW.connection_id
      AND projection.current_revision_id = NEW.revision_id
      AND projection.current_observation_id = NEW.observation_id
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND revision.head_sha = observation.head_sha
      AND revision.base_sha = observation.base_sha
      AND (
        observation.revision_id = revision.id
        OR EXISTS (
            SELECT 1 FROM observation_revision_links AS link
            WHERE link.observation_id = observation.id
              AND link.pull_request_id = observation.pull_request_id
              AND link.connection_id = observation.connection_id
              AND link.revision_id = revision.id
        )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'publication effect requires current canonical evidence');
END;

CREATE TRIGGER trg_publication_effect_proposal_authorization
BEFORE INSERT ON publication_effects
WHEN NEW.owner_kind = 'proposal_revision'
 AND NOT EXISTS (
    SELECT 1
    FROM proposal_revisions AS proposal_revision
    JOIN proposals AS proposal
      ON proposal.id = proposal_revision.proposal_id
    JOIN decisions AS decision
      ON decision.id = NEW.authorization_decision_id
     AND decision.proposal_revision_id = proposal_revision.id
     AND decision.decision = 'approve'
    WHERE proposal_revision.id = NEW.proposal_revision_id
      AND proposal_revision.pull_request_id = NEW.pull_request_id
      AND proposal_revision.revision_id = NEW.revision_id
      AND proposal_revision.observation_id = NEW.observation_id
      AND (
        (proposal.proposal_kind = 'approval' AND NEW.effect_type = 'review_approval')
        OR (proposal.proposal_kind = 'comment' AND NEW.effect_type = 'review_comment')
        OR (proposal.proposal_kind = 'changes' AND NEW.effect_type = 'review_changes')
      )
)
BEGIN
    SELECT RAISE(ABORT, 'publication effect requires approved matching proposal revision');
END;

CREATE TRIGGER trg_policy_sets_immutable_update
BEFORE UPDATE ON policy_sets
BEGIN
    SELECT RAISE(ABORT, 'policy set is immutable');
END;

CREATE TRIGGER trg_policy_sets_immutable_delete
BEFORE DELETE ON policy_sets
BEGIN
    SELECT RAISE(ABORT, 'policy set is immutable');
END;

CREATE TRIGGER trg_watch_rule_versions_immutable_update
BEFORE UPDATE ON watch_rule_versions
BEGIN
    SELECT RAISE(ABORT, 'watch rule version is immutable');
END;

CREATE TRIGGER trg_watch_rule_versions_immutable_delete
BEFORE DELETE ON watch_rule_versions
BEGIN
    SELECT RAISE(ABORT, 'watch rule version is immutable');
END;

CREATE TRIGGER trg_policy_evaluations_immutable_update
BEFORE UPDATE ON policy_evaluations
BEGIN
    SELECT RAISE(ABORT, 'policy evaluation is immutable');
END;

CREATE TRIGGER trg_policy_evaluations_immutable_delete
BEFORE DELETE ON policy_evaluations
BEGIN
    SELECT RAISE(ABORT, 'policy evaluation is immutable');
END;

CREATE TRIGGER trg_proposals_immutable_update
BEFORE UPDATE ON proposals
BEGIN
    SELECT RAISE(ABORT, 'proposal is immutable');
END;

CREATE TRIGGER trg_proposals_immutable_delete
BEFORE DELETE ON proposals
BEGIN
    SELECT RAISE(ABORT, 'proposal is immutable');
END;

CREATE TRIGGER trg_proposal_revisions_immutable_update
BEFORE UPDATE ON proposal_revisions
BEGIN
    SELECT RAISE(ABORT, 'proposal revision is immutable');
END;

CREATE TRIGGER trg_proposal_revisions_immutable_delete
BEFORE DELETE ON proposal_revisions
BEGIN
    SELECT RAISE(ABORT, 'proposal revision is immutable');
END;

CREATE TRIGGER trg_decisions_immutable_update
BEFORE UPDATE ON decisions
BEGIN
    SELECT RAISE(ABORT, 'decision is immutable');
END;

CREATE TRIGGER trg_decisions_immutable_delete
BEFORE DELETE ON decisions
BEGIN
    SELECT RAISE(ABORT, 'decision is immutable');
END;

CREATE TRIGGER trg_publication_effects_immutable_update
BEFORE UPDATE ON publication_effects
BEGIN
    SELECT RAISE(ABORT, 'publication effect is immutable');
END;

CREATE TRIGGER trg_publication_effects_immutable_delete
BEFORE DELETE ON publication_effects
BEGIN
    SELECT RAISE(ABORT, 'publication effect is immutable');
END;

CREATE TRIGGER trg_publication_attempts_immutable_update
BEFORE UPDATE ON publication_attempts
BEGIN
    SELECT RAISE(ABORT, 'publication attempt is immutable');
END;

CREATE TRIGGER trg_publication_attempts_immutable_delete
BEFORE DELETE ON publication_attempts
BEGIN
    SELECT RAISE(ABORT, 'publication attempt is immutable');
END;
