-- Notification delivery stays local and durable. Adapters may later claim
-- queued rows, but no external channel is implied by this schema.

CREATE TABLE notification_preferences (
    id TEXT PRIMARY KEY NOT NULL CHECK (id = 'local-default'),
    version INTEGER NOT NULL CHECK (version > 0),
    channels_json BLOB NOT NULL CHECK (
        json_valid(channels_json)
        AND json_type(channels_json) = 'object'
    ),
    quiet_hours_json BLOB NOT NULL CHECK (
        json_valid(quiet_hours_json)
        AND json_type(quiet_hours_json) = 'object'
    ),
    event_templates_json BLOB NOT NULL CHECK (
        json_valid(event_templates_json)
        AND json_type(event_templates_json) = 'object'
    ),
    muted_until_us INTEGER CHECK (muted_until_us IS NULL OR muted_until_us >= 0),
    speech_rate_milli INTEGER NOT NULL DEFAULT 1000 CHECK (
        speech_rate_milli BETWEEN 500 AND 2000
    ),
    custom_sound_path TEXT CHECK (
        custom_sound_path IS NULL OR length(custom_sound_path) BETWEEN 1 AND 4096
    ),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= 0)
);

INSERT INTO notification_preferences(
    id, version, channels_json, quiet_hours_json, event_templates_json,
    muted_until_us, speech_rate_milli, custom_sound_path, updated_at_us
) VALUES (
    'local-default', 1,
    '{"browser":false,"log":true,"sound":false,"tts":false}',
    '{}', '{}', NULL, 1000, NULL, unixepoch('subsec') * 1000000
);

CREATE TABLE notification_deliveries (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    domain_event_id TEXT NOT NULL,
    event_type TEXT NOT NULL CHECK (length(event_type) > 0),
    channel TEXT NOT NULL CHECK (channel IN ('browser', 'sound', 'tts', 'log')),
    template_version INTEGER NOT NULL CHECK (template_version > 0),
    dedupe_key TEXT NOT NULL CHECK (length(dedupe_key) > 0),
    payload_json BLOB NOT NULL CHECK (
        json_valid(payload_json)
        AND json_type(payload_json) = 'object'
    ),
    payload_sha256 TEXT NOT NULL CHECK (
        length(payload_sha256) = 64
        AND payload_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    state TEXT NOT NULL CHECK (state IN (
        'queued', 'delivering', 'delivered', 'suppressed', 'failed'
    )),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    available_at_us INTEGER NOT NULL CHECK (available_at_us >= 0),
    delivered_at_us INTEGER CHECK (delivered_at_us IS NULL OR delivered_at_us >= 0),
    last_error TEXT,
    created_at_us INTEGER NOT NULL CHECK (created_at_us >= 0),
    updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
    UNIQUE (domain_event_id, channel, template_version),
    UNIQUE (channel, dedupe_key),
    FOREIGN KEY (domain_event_id) REFERENCES domain_events(id) ON DELETE RESTRICT
);

CREATE INDEX idx_notification_deliveries_pending
    ON notification_deliveries(state, available_at_us, id);

CREATE INDEX idx_notification_deliveries_event
    ON notification_deliveries(domain_event_id, channel, template_version);

CREATE TRIGGER trg_notification_delivery_event_type
BEFORE INSERT ON notification_deliveries
WHEN NOT EXISTS (
    SELECT 1 FROM domain_events
    WHERE id = NEW.domain_event_id AND event_type = NEW.event_type
)
BEGIN
    SELECT RAISE(ABORT, 'notification delivery event type must match domain event');
END;

CREATE TRIGGER trg_notification_delivery_immutable_facts
BEFORE UPDATE ON notification_deliveries
WHEN NEW.id IS NOT OLD.id
  OR NEW.domain_event_id IS NOT OLD.domain_event_id
  OR NEW.event_type IS NOT OLD.event_type
  OR NEW.channel IS NOT OLD.channel
  OR NEW.template_version IS NOT OLD.template_version
  OR NEW.dedupe_key IS NOT OLD.dedupe_key
  OR NEW.payload_json IS NOT OLD.payload_json
  OR NEW.payload_sha256 IS NOT OLD.payload_sha256
  OR NEW.available_at_us IS NOT OLD.available_at_us
  OR NEW.created_at_us IS NOT OLD.created_at_us
BEGIN
    SELECT RAISE(ABORT, 'notification delivery facts are immutable');
END;

CREATE TRIGGER trg_notification_delivery_no_delete
BEFORE DELETE ON notification_deliveries
BEGIN
    SELECT RAISE(ABORT, 'notification deliveries are retained');
END;
