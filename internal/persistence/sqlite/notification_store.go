package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	localNotificationPreferencesID = "local-default"
	maxNotificationJSONBytes       = 64 * 1024
	maxNotificationDedupeKeyBytes  = 512
	maxNotificationPathBytes       = 4096
	maxBrowserNotificationPageSize = 50
)

var (
	// ErrNotificationPreferencesConflict means a caller attempted to replace
	// preferences based on an older version.
	ErrNotificationPreferencesConflict = errors.New("notification preferences version conflict")
	// ErrNotificationDeliveryConflict means a dedupe identity is already bound
	// to different immutable notification facts.
	ErrNotificationDeliveryConflict = errors.New("notification delivery facts conflict")
	// ErrNotificationDeliveryNotFound means a requested durable delivery is absent.
	ErrNotificationDeliveryNotFound = errors.New("notification delivery not found")
	// ErrNotificationDeliveryNotPending means a delivery cannot receive a new
	// terminal outcome because it is not queued.
	ErrNotificationDeliveryNotPending = errors.New("notification delivery is not pending")
)

// NotificationChannel identifies one local delivery adapter. This store never
// invokes the adapter; it only records its durable work item.
type NotificationChannel string

const (
	NotificationChannelBrowser NotificationChannel = "browser"
	NotificationChannelSound   NotificationChannel = "sound"
	NotificationChannelTTS     NotificationChannel = "tts"
	NotificationChannelLog     NotificationChannel = "log"
)

// NotificationDeliveryState records local delivery progress.
type NotificationDeliveryState string

const (
	NotificationDeliveryQueued     NotificationDeliveryState = "queued"
	NotificationDeliveryDelivering NotificationDeliveryState = "delivering"
	NotificationDeliveryDelivered  NotificationDeliveryState = "delivered"
	NotificationDeliverySuppressed NotificationDeliveryState = "suppressed"
	NotificationDeliveryFailed     NotificationDeliveryState = "failed"
)

// NotificationPreferences contains the one local operator's mutable
// notification configuration. JSON fields intentionally remain typed objects
// so channel and template schemas can evolve without a destructive migration.
type NotificationPreferences struct {
	Version            int64
	ChannelsJSON       []byte
	QuietHoursJSON     []byte
	EventTemplatesJSON []byte
	MutedUntil         *time.Time
	SpeechRateMilli    int
	CustomSoundPath    string
	UpdatedAt          time.Time
}

// UpdateNotificationPreferencesInput replaces the local preference document
// only when ExpectedVersion matches the current durable version.
type UpdateNotificationPreferencesInput struct {
	ExpectedVersion    int64
	ChannelsJSON       []byte
	QuietHoursJSON     []byte
	EventTemplatesJSON []byte
	MutedUntil         *time.Time
	SpeechRateMilli    int
	CustomSoundPath    string
	UpdatedAt          time.Time
}

// LoadNotificationPreferences reads the seeded local preference document.
func (s *Store) LoadNotificationPreferences(ctx context.Context) (NotificationPreferences, error) {
	preferences, err := loadNotificationPreferences(ctx, s.db)
	if err != nil {
		return NotificationPreferences{}, fmt.Errorf("load notification preferences: %w", err)
	}
	return preferences, nil
}

// UpdateNotificationPreferences safely replaces local preferences with
// optimistic concurrency. It creates no job, event, outbox, or external work.
func (s *Store) UpdateNotificationPreferences(ctx context.Context, input UpdateNotificationPreferencesInput) (NotificationPreferences, error) {
	normalized, err := normalizeNotificationPreferencesInput(input)
	if err != nil {
		return NotificationPreferences{}, err
	}
	var result NotificationPreferences
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		current, err := loadNotificationPreferences(ctx, conn)
		if err != nil {
			return err
		}
		if current.Version != normalized.ExpectedVersion {
			return ErrNotificationPreferencesConflict
		}
		nextVersion := current.Version + 1
		var mutedUntil any
		if normalized.MutedUntil != nil {
			mutedUntil = normalized.MutedUntil.UnixMicro()
		}
		updated, err := conn.ExecContext(ctx, `
UPDATE notification_preferences
SET version = ?, channels_json = ?, quiet_hours_json = ?, event_templates_json = ?,
    muted_until_us = ?, speech_rate_milli = ?, custom_sound_path = NULLIF(?, ''),
    updated_at_us = ?
WHERE id = ? AND version = ?`,
			nextVersion, normalized.ChannelsJSON, normalized.QuietHoursJSON, normalized.EventTemplatesJSON,
			mutedUntil, normalized.SpeechRateMilli, normalized.CustomSoundPath,
			normalized.UpdatedAt.UnixMicro(), localNotificationPreferencesID, current.Version)
		if err != nil {
			return fmt.Errorf("update notification preferences: %w", err)
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("read notification preferences update: %w", err)
		}
		if count != 1 {
			return ErrNotificationPreferencesConflict
		}
		result = NotificationPreferences{
			Version: nextVersion, ChannelsJSON: normalized.ChannelsJSON, QuietHoursJSON: normalized.QuietHoursJSON,
			EventTemplatesJSON: normalized.EventTemplatesJSON, MutedUntil: normalized.MutedUntil,
			SpeechRateMilli: normalized.SpeechRateMilli, CustomSoundPath: normalized.CustomSoundPath,
			UpdatedAt: normalized.UpdatedAt,
		}
		return nil
	})
	if err != nil {
		return NotificationPreferences{}, fmt.Errorf("update notification preferences: %w", err)
	}
	return result, nil
}

type normalizedNotificationPreferencesInput struct {
	UpdateNotificationPreferencesInput
}

func normalizeNotificationPreferencesInput(input UpdateNotificationPreferencesInput) (normalizedNotificationPreferencesInput, error) {
	if input.ExpectedVersion < 1 || input.SpeechRateMilli < 500 || input.SpeechRateMilli > 2000 {
		return normalizedNotificationPreferencesInput{}, errors.New("notification preferences input is invalid")
	}
	channels, err := normalizeNotificationJSONObject(input.ChannelsJSON)
	if err != nil {
		return normalizedNotificationPreferencesInput{}, fmt.Errorf("notification channels: %w", err)
	}
	quietHours, err := normalizeNotificationJSONObject(input.QuietHoursJSON)
	if err != nil {
		return normalizedNotificationPreferencesInput{}, fmt.Errorf("notification quiet hours: %w", err)
	}
	templates, err := normalizeNotificationJSONObject(input.EventTemplatesJSON)
	if err != nil {
		return normalizedNotificationPreferencesInput{}, fmt.Errorf("notification templates: %w", err)
	}
	path := strings.TrimSpace(input.CustomSoundPath)
	if len(path) > maxNotificationPathBytes {
		return normalizedNotificationPreferencesInput{}, errors.New("notification custom sound path is invalid")
	}
	if input.MutedUntil != nil {
		mutedUntil := input.MutedUntil.UTC()
		if mutedUntil.IsZero() || mutedUntil.UnixMicro() < 0 {
			return normalizedNotificationPreferencesInput{}, errors.New("notification mute time is invalid")
		}
		input.MutedUntil = &mutedUntil
	}
	updatedAt := input.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	if updatedAt.UnixMicro() < 0 {
		return normalizedNotificationPreferencesInput{}, errors.New("notification update time is invalid")
	}
	input.ChannelsJSON, input.QuietHoursJSON, input.EventTemplatesJSON = channels, quietHours, templates
	input.CustomSoundPath, input.UpdatedAt = path, updatedAt
	return normalizedNotificationPreferencesInput{UpdateNotificationPreferencesInput: input}, nil
}

func loadNotificationPreferences(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (NotificationPreferences, error) {
	var preferences NotificationPreferences
	var mutedUntil sql.NullInt64
	var updatedAtUS int64
	err := queryer.QueryRowContext(ctx, `
SELECT version, channels_json, quiet_hours_json, event_templates_json,
       muted_until_us, speech_rate_milli, COALESCE(custom_sound_path, ''), updated_at_us
FROM notification_preferences WHERE id = ?`, localNotificationPreferencesID).Scan(
		&preferences.Version, &preferences.ChannelsJSON, &preferences.QuietHoursJSON,
		&preferences.EventTemplatesJSON, &mutedUntil, &preferences.SpeechRateMilli,
		&preferences.CustomSoundPath, &updatedAtUS)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationPreferences{}, errors.New("local notification preferences are missing")
	}
	if err != nil {
		return NotificationPreferences{}, err
	}
	if preferences.Version < 1 || preferences.SpeechRateMilli < 500 || preferences.SpeechRateMilli > 2000 || updatedAtUS < 0 {
		return NotificationPreferences{}, errors.New("stored notification preferences are invalid")
	}
	if mutedUntil.Valid {
		if mutedUntil.Int64 < 0 {
			return NotificationPreferences{}, errors.New("stored notification mute time is invalid")
		}
		value := time.UnixMicro(mutedUntil.Int64).UTC()
		preferences.MutedUntil = &value
	}
	preferences.UpdatedAt = time.UnixMicro(updatedAtUS).UTC()
	return preferences, nil
}

// CreateNotificationDeliveryInput supplies one idempotent, adapter-neutral
// notification work item for a committed domain event.
type CreateNotificationDeliveryInput struct {
	DomainEventID   string
	Channel         NotificationChannel
	TemplateVersion int
	DedupeKey       string
	PayloadJSON     []byte
	AvailableAt     time.Time
	CreatedAt       time.Time
}

// CreateNotificationDeliveryResult identifies one queued local delivery.
type CreateNotificationDeliveryResult struct {
	ID      string
	State   NotificationDeliveryState
	Created bool
}

// NotificationDeliveryTarget is immutable delivery work plus its current
// progress state. Payload facts remain opaque to local channel adapters.
type NotificationDeliveryTarget struct {
	ID        string
	EventType string
	Channel   NotificationChannel
	State     NotificationDeliveryState
	Attempt   int
}

// NotificationDeliveryOutcome supplies one local terminal delivery result.
// Only delivered and suppressed are safe local outcomes; failures remain job
// failures so the worker's durable retry policy owns them.
type NotificationDeliveryOutcome struct {
	ID          string
	State       NotificationDeliveryState
	AttemptedAt time.Time
}

// RecordNotificationDeliveryOutcomeResult reports the retained terminal state.
type RecordNotificationDeliveryOutcomeResult struct {
	State    NotificationDeliveryState
	Attempt  int
	Recorded bool
}

// LoadNotificationDelivery loads one retained delivery for a bounded local
// adapter. It invokes no adapter and exposes no destination configuration.
func (s *Store) LoadNotificationDelivery(ctx context.Context, id string) (NotificationDeliveryTarget, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > maxNotificationDedupeKeyBytes {
		return NotificationDeliveryTarget{}, ErrNotificationDeliveryNotFound
	}
	var target NotificationDeliveryTarget
	err := s.db.QueryRowContext(ctx, `
SELECT id, event_type, channel, state, attempt
FROM notification_deliveries WHERE id = ?`, id).Scan(
		&target.ID, &target.EventType, &target.Channel, &target.State, &target.Attempt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationDeliveryTarget{}, ErrNotificationDeliveryNotFound
	}
	if err != nil {
		return NotificationDeliveryTarget{}, fmt.Errorf("load notification delivery: %w", err)
	}
	if target.ID != id || !validNotificationChannel(target.Channel) || !validNotificationDeliveryState(target.State) || target.Attempt < 0 {
		return NotificationDeliveryTarget{}, errors.New("stored notification delivery is invalid")
	}
	return target, nil
}

// ListQueuedBrowserNotificationDeliveries exposes bounded, local-only browser
// work for an open loopback dashboard. It returns no payload or preferences.
func (s *Store) ListQueuedBrowserNotificationDeliveries(ctx context.Context, limit int) ([]NotificationDeliveryTarget, error) {
	if limit <= 0 || limit > maxBrowserNotificationPageSize {
		return nil, errors.New("browser notification delivery limit is invalid")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, event_type, channel, state, attempt
FROM notification_deliveries
WHERE channel = 'browser' AND state = 'queued'
ORDER BY available_at_us, id
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list queued browser notification deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]NotificationDeliveryTarget, 0, limit)
	for rows.Next() {
		var item NotificationDeliveryTarget
		if err := rows.Scan(&item.ID, &item.EventType, &item.Channel, &item.State, &item.Attempt); err != nil {
			return nil, fmt.Errorf("scan queued browser notification delivery: %w", err)
		}
		if item.ID == "" || item.EventType == "" || item.Channel != NotificationChannelBrowser || item.State != NotificationDeliveryQueued || item.Attempt < 0 {
			return nil, errors.New("stored browser notification delivery is invalid")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued browser notification deliveries: %w", err)
	}
	return items, nil
}

// RecordNotificationDeliveryOutcome atomically retains one safe local result.
// Replaying a completed job returns its existing result without increasing the
// attempt count; immutable delivery facts are never rewritten.
func (s *Store) RecordNotificationDeliveryOutcome(ctx context.Context, outcome NotificationDeliveryOutcome) (RecordNotificationDeliveryOutcomeResult, error) {
	if err := normalizeNotificationDeliveryOutcome(&outcome); err != nil {
		return RecordNotificationDeliveryOutcomeResult{}, err
	}
	var result RecordNotificationDeliveryOutcomeResult
	err := withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var current struct {
			state   NotificationDeliveryState
			attempt int
		}
		err := conn.QueryRowContext(ctx, "SELECT state, attempt FROM notification_deliveries WHERE id = ?", outcome.ID).Scan(&current.state, &current.attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotificationDeliveryNotFound
		}
		if err != nil {
			return fmt.Errorf("load notification delivery outcome: %w", err)
		}
		if current.attempt < 0 || !validNotificationDeliveryState(current.state) {
			return errors.New("stored notification delivery is invalid")
		}
		if current.state == NotificationDeliveryDelivered || current.state == NotificationDeliverySuppressed {
			result = RecordNotificationDeliveryOutcomeResult{State: current.state, Attempt: current.attempt}
			return nil
		}
		if current.state != NotificationDeliveryQueued {
			return ErrNotificationDeliveryNotPending
		}
		var deliveredAt any
		if outcome.State == NotificationDeliveryDelivered {
			deliveredAt = outcome.AttemptedAt.UnixMicro()
		}
		updated, err := conn.ExecContext(ctx, `
UPDATE notification_deliveries
SET state = ?, attempt = attempt + 1, delivered_at_us = ?, last_error = NULL, updated_at_us = ?
WHERE id = ? AND state = 'queued'`, outcome.State, deliveredAt, outcome.AttemptedAt.UnixMicro(), outcome.ID)
		if err != nil {
			return fmt.Errorf("record notification delivery outcome: %w", err)
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("read notification delivery outcome: %w", err)
		}
		if count != 1 {
			return ErrNotificationDeliveryNotPending
		}
		result = RecordNotificationDeliveryOutcomeResult{State: outcome.State, Attempt: current.attempt + 1, Recorded: true}
		return nil
	})
	if err != nil {
		return RecordNotificationDeliveryOutcomeResult{}, fmt.Errorf("record notification delivery outcome: %w", err)
	}
	return result, nil
}

// CreateNotificationDelivery records one durable notification delivery. It
// only accepts an existing event and uses channel plus dedupe key as the
// logical occurrence identity. It does not invoke a channel or create jobs.
func (s *Store) CreateNotificationDelivery(ctx context.Context, input CreateNotificationDeliveryInput) (CreateNotificationDeliveryResult, error) {
	normalized, err := normalizeCreateNotificationDeliveryInput(input)
	if err != nil {
		return CreateNotificationDeliveryResult{}, err
	}
	var result CreateNotificationDeliveryResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		eventType, err := loadNotificationEventType(ctx, conn, normalized.DomainEventID)
		if err != nil {
			return err
		}
		id := stableID("notification-delivery", string(normalized.Channel), normalized.DedupeKey)
		existing, found, err := loadNotificationDeliveryByDedupe(ctx, conn, normalized.Channel, normalized.DedupeKey)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(id, eventType, normalized) {
				return ErrNotificationDeliveryConflict
			}
			result = CreateNotificationDeliveryResult{ID: existing.ID, State: existing.State}
			return nil
		}
		byEvent, found, err := loadNotificationDeliveryByEvent(ctx, conn, normalized.DomainEventID, normalized.Channel, normalized.TemplateVersion)
		if err != nil {
			return err
		}
		if found {
			if !byEvent.matches(id, eventType, normalized) {
				return ErrNotificationDeliveryConflict
			}
			result = CreateNotificationDeliveryResult{ID: byEvent.ID, State: byEvent.State}
			return nil
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO notification_deliveries(
 id, domain_event_id, event_type, channel, template_version, dedupe_key,
 payload_json, payload_sha256, state, attempt, available_at_us, delivered_at_us,
 last_error, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'queued', 0, ?, NULL, NULL, ?, ?)`,
			id, normalized.DomainEventID, eventType, normalized.Channel, normalized.TemplateVersion,
			normalized.DedupeKey, normalized.PayloadJSON, normalized.PayloadSHA256,
			normalized.AvailableAt.UnixMicro(), normalized.CreatedAt.UnixMicro(), normalized.CreatedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert notification delivery: %w", err)
		}
		result = CreateNotificationDeliveryResult{ID: id, State: NotificationDeliveryQueued, Created: true}
		return nil
	})
	if err != nil {
		return CreateNotificationDeliveryResult{}, fmt.Errorf("create notification delivery: %w", err)
	}
	return result, nil
}

type normalizedNotificationDeliveryInput struct {
	CreateNotificationDeliveryInput
	PayloadSHA256 string
}

func normalizeCreateNotificationDeliveryInput(input CreateNotificationDeliveryInput) (normalizedNotificationDeliveryInput, error) {
	input.DomainEventID = strings.TrimSpace(input.DomainEventID)
	input.DedupeKey = strings.TrimSpace(input.DedupeKey)
	if input.DomainEventID == "" || len(input.DomainEventID) > maxNotificationDedupeKeyBytes ||
		input.DedupeKey == "" || len(input.DedupeKey) > maxNotificationDedupeKeyBytes ||
		input.TemplateVersion < 1 || !validNotificationChannel(input.Channel) {
		return normalizedNotificationDeliveryInput{}, errors.New("notification delivery input is invalid")
	}
	payload, err := normalizeNotificationJSONObject(input.PayloadJSON)
	if err != nil {
		return normalizedNotificationDeliveryInput{}, fmt.Errorf("notification delivery payload: %w", err)
	}
	createdAt := input.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	availableAt := input.AvailableAt.UTC()
	if availableAt.IsZero() {
		availableAt = createdAt
	}
	if createdAt.UnixMicro() < 0 || availableAt.UnixMicro() < 0 {
		return normalizedNotificationDeliveryInput{}, errors.New("notification delivery time is invalid")
	}
	digest := sha256.Sum256(payload)
	input.PayloadJSON, input.CreatedAt, input.AvailableAt = payload, createdAt, availableAt
	return normalizedNotificationDeliveryInput{
		CreateNotificationDeliveryInput: input,
		PayloadSHA256:                   hex.EncodeToString(digest[:]),
	}, nil
}

func validNotificationChannel(channel NotificationChannel) bool {
	switch channel {
	case NotificationChannelBrowser, NotificationChannelSound, NotificationChannelTTS, NotificationChannelLog:
		return true
	default:
		return false
	}
}

func validNotificationDeliveryState(state NotificationDeliveryState) bool {
	switch state {
	case NotificationDeliveryQueued, NotificationDeliveryDelivering, NotificationDeliveryDelivered,
		NotificationDeliverySuppressed, NotificationDeliveryFailed:
		return true
	default:
		return false
	}
}

func normalizeNotificationDeliveryOutcome(outcome *NotificationDeliveryOutcome) error {
	outcome.ID = strings.TrimSpace(outcome.ID)
	if outcome.ID == "" || len(outcome.ID) > maxNotificationDedupeKeyBytes ||
		(outcome.State != NotificationDeliveryDelivered && outcome.State != NotificationDeliverySuppressed) {
		return errors.New("notification delivery outcome is invalid")
	}
	outcome.AttemptedAt = outcome.AttemptedAt.UTC()
	if outcome.AttemptedAt.IsZero() {
		outcome.AttemptedAt = time.Now().UTC()
	}
	if outcome.AttemptedAt.UnixMicro() < 0 {
		return errors.New("notification delivery outcome is invalid")
	}
	return nil
}

func normalizeNotificationJSONObject(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maxNotificationJSONBytes {
		return nil, errors.New("must be a bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON object")
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	if len(normalized) > maxNotificationJSONBytes {
		return nil, errors.New("must be a bounded JSON object")
	}
	return normalized, nil
}

func loadNotificationEventType(ctx context.Context, conn *sql.Conn, eventID string) (string, error) {
	var eventType string
	err := conn.QueryRowContext(ctx, "SELECT event_type FROM domain_events WHERE id = ?", eventID).Scan(&eventType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("notification domain event not found")
	}
	if err != nil {
		return "", fmt.Errorf("load notification domain event: %w", err)
	}
	return eventType, nil
}

type storedNotificationDelivery struct {
	ID              string
	DomainEventID   string
	EventType       string
	Channel         NotificationChannel
	TemplateVersion int
	DedupeKey       string
	PayloadJSON     []byte
	PayloadSHA256   string
	State           NotificationDeliveryState
}

func loadNotificationDeliveryByDedupe(ctx context.Context, conn *sql.Conn, channel NotificationChannel, dedupeKey string) (storedNotificationDelivery, bool, error) {
	return loadNotificationDelivery(ctx, conn, `WHERE channel = ? AND dedupe_key = ?`, channel, dedupeKey)
}

func loadNotificationDeliveryByEvent(ctx context.Context, conn *sql.Conn, eventID string, channel NotificationChannel, templateVersion int) (storedNotificationDelivery, bool, error) {
	return loadNotificationDelivery(ctx, conn, `WHERE domain_event_id = ? AND channel = ? AND template_version = ?`, eventID, channel, templateVersion)
}

func loadNotificationDelivery(ctx context.Context, conn *sql.Conn, clause string, arguments ...any) (storedNotificationDelivery, bool, error) {
	var delivery storedNotificationDelivery
	err := conn.QueryRowContext(ctx, `
SELECT id, domain_event_id, event_type, channel, template_version, dedupe_key,
       payload_json, payload_sha256, state
FROM notification_deliveries `+clause, arguments...).Scan(
		&delivery.ID, &delivery.DomainEventID, &delivery.EventType, &delivery.Channel,
		&delivery.TemplateVersion, &delivery.DedupeKey, &delivery.PayloadJSON,
		&delivery.PayloadSHA256, &delivery.State)
	if errors.Is(err, sql.ErrNoRows) {
		return storedNotificationDelivery{}, false, nil
	}
	if err != nil {
		return storedNotificationDelivery{}, false, fmt.Errorf("load notification delivery: %w", err)
	}
	return delivery, true, nil
}

func (delivery storedNotificationDelivery) matches(id, eventType string, input normalizedNotificationDeliveryInput) bool {
	return delivery.ID == id && delivery.DomainEventID == input.DomainEventID && delivery.EventType == eventType &&
		delivery.Channel == input.Channel && delivery.TemplateVersion == input.TemplateVersion &&
		delivery.DedupeKey == input.DedupeKey && bytes.Equal(delivery.PayloadJSON, input.PayloadJSON) &&
		delivery.PayloadSHA256 == input.PayloadSHA256
}
