DROP TRIGGER trg_revision_manifest_canonical_identity;
DROP TRIGGER trg_observation_revision_link_identity;

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
      AND json_extract(NEW.manifest_json, '$.version') = 1
      AND json_extract(NEW.manifest_json, '$.head_sha') = revision.head_sha
      AND json_extract(NEW.manifest_json, '$.base_sha') = revision.base_sha
      AND json_type(NEW.manifest_json, '$.files') = 'array'
      AND json_array_length(NEW.manifest_json, '$.files') = NEW.entry_count
)
BEGIN
    SELECT RAISE(ABORT, 'revision manifest requires matching canonical identity and envelope');
END;

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
      AND (observation.revision_id IS NULL OR observation.revision_id = NEW.revision_id)
      AND observation.head_sha = revision.head_sha
      AND observation.base_sha = revision.base_sha
      AND revision.identity_kind = 'canonical_diff'
      AND revision.is_publishable = 1
      AND manifest.manifest_sha256 = revision.diff_sha256
)
BEGIN
    SELECT RAISE(ABORT, 'observation revision link identity does not match canonical revision');
END;
