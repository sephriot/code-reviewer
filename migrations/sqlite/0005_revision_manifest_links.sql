-- Canonical hydration arrives after the immutable GitHub observation.  The
-- link records that later proof without changing the fact snapshot itself.
CREATE TABLE revision_manifests (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    revision_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    manifest_format_version INTEGER NOT NULL CHECK (manifest_format_version = 1),
    manifest_sha256 TEXT NOT NULL CHECK (
        length(manifest_sha256) = 64
        AND manifest_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    entry_count INTEGER NOT NULL CHECK (entry_count >= 0),
    manifest_json BLOB NOT NULL CHECK (
        json_valid(manifest_json)
        AND json_type(manifest_json) = 'object'
    ),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    UNIQUE (revision_id),
    UNIQUE (id, revision_id, pull_request_id),
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT
);

CREATE INDEX idx_revision_manifests_pull_request
    ON revision_manifests(pull_request_id, created_at_us DESC, id);

CREATE TRIGGER trg_revision_manifest_canonical_identity
BEFORE INSERT ON revision_manifests
WHEN NOT EXISTS (
    SELECT 1
    FROM revisions AS revision
    WHERE revision.id = NEW.revision_id
      AND revision.pull_request_id = NEW.pull_request_id
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND revision.diff_sha256 = NEW.manifest_sha256
)
BEGIN
    SELECT RAISE(ABORT, 'revision manifest requires publishable canonical revision identity');
END;

CREATE TRIGGER trg_revision_manifest_immutable_update
BEFORE UPDATE ON revision_manifests
BEGIN
    SELECT RAISE(ABORT, 'revision manifest is immutable');
END;

CREATE TRIGGER trg_revision_manifest_immutable_delete
BEFORE DELETE ON revision_manifests
BEGIN
    SELECT RAISE(ABORT, 'revision manifest is immutable');
END;

CREATE TABLE observation_revision_links (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    observation_id TEXT NOT NULL,
    pull_request_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    revision_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    attached_at_us INTEGER NOT NULL CHECK (attached_at_us >= 0),
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    CHECK (created_at_us = attached_at_us),
    UNIQUE (observation_id),
    UNIQUE (id, observation_id, pull_request_id, connection_id, revision_id),
    FOREIGN KEY (observation_id, pull_request_id, connection_id)
        REFERENCES pull_request_observations(id, pull_request_id, connection_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (revision_id, pull_request_id)
        REFERENCES revisions(id, pull_request_id) ON DELETE RESTRICT,
    FOREIGN KEY (manifest_id, revision_id, pull_request_id)
        REFERENCES revision_manifests(id, revision_id, pull_request_id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_observation_revision_links_revision
    ON observation_revision_links(revision_id, pull_request_id, observation_id);

CREATE TRIGGER trg_observation_revision_link_identity
BEFORE INSERT ON observation_revision_links
WHEN NOT EXISTS (
    SELECT 1
    FROM pull_request_observations AS observation
    JOIN revisions AS revision
      ON revision.id = NEW.revision_id
     AND revision.pull_request_id = NEW.pull_request_id
    JOIN revision_manifests AS manifest
      ON manifest.id = NEW.manifest_id
     AND manifest.revision_id = revision.id
     AND manifest.pull_request_id = revision.pull_request_id
    WHERE observation.id = NEW.observation_id
      AND observation.pull_request_id = NEW.pull_request_id
      AND observation.connection_id = NEW.connection_id
      AND observation.head_sha = revision.head_sha
      AND observation.base_sha = revision.base_sha
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND manifest.manifest_sha256 = revision.diff_sha256
)
BEGIN
    SELECT RAISE(ABORT, 'observation revision link identity does not match canonical revision');
END;

CREATE TRIGGER trg_observation_revision_link_immutable_update
BEFORE UPDATE ON observation_revision_links
BEGIN
    SELECT RAISE(ABORT, 'observation revision link is immutable');
END;

CREATE TRIGGER trg_observation_revision_link_immutable_delete
BEFORE DELETE ON observation_revision_links
BEGIN
    SELECT RAISE(ABORT, 'observation revision link is immutable');
END;

-- Version 3 permitted a projection to match a revision embedded in its
-- observation.  Keep that historic path valid while admitting a later,
-- append-only canonical attachment for metadata-only observations.
DROP TRIGGER trg_projection_observation_revision_insert;
DROP TRIGGER trg_projection_observation_revision_update;

CREATE TRIGGER trg_projection_observation_revision_insert
BEFORE INSERT ON pull_request_projection_state
WHEN NOT EXISTS (
    SELECT 1
    FROM pull_request_observations AS observation
    WHERE observation.id = NEW.current_observation_id
      AND observation.pull_request_id = NEW.pull_request_id
      AND observation.connection_id = NEW.connection_id
      AND (
        observation.revision_id IS NEW.current_revision_id
        OR EXISTS (
            SELECT 1
            FROM observation_revision_links AS link
            WHERE link.observation_id = observation.id
              AND link.pull_request_id = observation.pull_request_id
              AND link.connection_id = observation.connection_id
              AND link.revision_id = NEW.current_revision_id
        )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'projection revision must match current observation or canonical attachment');
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
      AND (
        observation.revision_id IS NEW.current_revision_id
        OR EXISTS (
            SELECT 1
            FROM observation_revision_links AS link
            WHERE link.observation_id = observation.id
              AND link.pull_request_id = observation.pull_request_id
              AND link.connection_id = observation.connection_id
              AND link.revision_id = NEW.current_revision_id
        )
      )
)
BEGIN
    SELECT RAISE(ABORT, 'projection revision must match current observation or canonical attachment');
END;
