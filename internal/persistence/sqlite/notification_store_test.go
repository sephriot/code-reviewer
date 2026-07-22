package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNotificationPreferencesDefaultAndVersionedUpdate(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	initial, err := store.LoadNotificationPreferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Version != 1 || string(initial.ChannelsJSON) != `{"browser":false,"log":true,"sound":false,"tts":false}` ||
		string(initial.QuietHoursJSON) != `{}` || string(initial.EventTemplatesJSON) != `{}` ||
		initial.MutedUntil != nil || initial.SpeechRateMilli != 1000 || initial.CustomSoundPath != "" {
		t.Fatalf("initial preferences = %+v", initial)
	}

	mutedUntil := time.Date(2026, 7, 22, 14, 30, 0, 0, time.UTC)
	updated, err := store.UpdateNotificationPreferences(ctx, UpdateNotificationPreferencesInput{
		ExpectedVersion:    initial.Version,
		ChannelsJSON:       []byte(`{"log":true,"sound":true}`),
		QuietHoursJSON:     []byte(`{"timezone":"Europe/Warsaw"}`),
		EventTemplatesJSON: []byte(`{"review_observed":"new review"}`),
		MutedUntil:         &mutedUntil,
		SpeechRateMilli:    1250,
		CustomSoundPath:    "/tmp/review.wav",
		UpdatedAt:          time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || string(updated.ChannelsJSON) != `{"log":true,"sound":true}` ||
		updated.MutedUntil == nil || !updated.MutedUntil.Equal(mutedUntil) || updated.SpeechRateMilli != 1250 || updated.CustomSoundPath != "/tmp/review.wav" {
		t.Fatalf("updated preferences = %+v", updated)
	}

	if _, err := store.UpdateNotificationPreferences(ctx, UpdateNotificationPreferencesInput{
		ExpectedVersion: initial.Version, ChannelsJSON: []byte(`{}`), QuietHoursJSON: []byte(`{}`), EventTemplatesJSON: []byte(`{}`), SpeechRateMilli: 1000,
	}); !errors.Is(err, ErrNotificationPreferencesConflict) {
		t.Fatalf("stale update error = %v, want ErrNotificationPreferencesConflict", err)
	}
}

func TestNotificationDeliveryIsIdempotentAndRetained(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	event := appendNotificationTestEvent(t, ctx, store, "event-review-observed", "review_observed")
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	input := CreateNotificationDeliveryInput{
		DomainEventID:   event.EventID,
		Channel:         NotificationChannelSound,
		TemplateVersion: 1,
		DedupeKey:       "review-observed:connection-1:observation-1",
		PayloadJSON:     []byte(`{"repository":"sephriot/code-reviewer","number":42}`),
		AvailableAt:     now,
		CreatedAt:       now,
	}

	first, err := store.CreateNotificationDelivery(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.ID == "" || first.State != NotificationDeliveryQueued {
		t.Fatalf("first delivery = %+v", first)
	}
	second, err := store.CreateNotificationDelivery(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ID != first.ID || second.State != NotificationDeliveryQueued {
		t.Fatalf("second delivery = %+v", second)
	}

	changed := input
	changed.PayloadJSON = []byte(`{"repository":"changed"}`)
	if _, err := store.CreateNotificationDelivery(ctx, changed); !errors.Is(err, ErrNotificationDeliveryConflict) {
		t.Fatalf("changed delivery error = %v, want ErrNotificationDeliveryConflict", err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM notification_deliveries WHERE id = ?", first.ID); err == nil {
		t.Fatal("notification delivery delete was accepted")
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE notification_deliveries SET payload_json = '{}' WHERE id = ?", first.ID); err == nil {
		t.Fatal("notification delivery fact rewrite was accepted")
	}
}

func TestNotificationDeliveryRejectsUnknownEventsAndUnsafeInput(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	for _, input := range []CreateNotificationDeliveryInput{
		{DomainEventID: "missing", Channel: NotificationChannelLog, TemplateVersion: 1, DedupeKey: "missing", PayloadJSON: []byte(`{}`)},
		{DomainEventID: "missing", Channel: NotificationChannel("email"), TemplateVersion: 1, DedupeKey: "bad-channel", PayloadJSON: []byte(`{}`)},
		{DomainEventID: "missing", Channel: NotificationChannelLog, TemplateVersion: 0, DedupeKey: "bad-version", PayloadJSON: []byte(`{}`)},
		{DomainEventID: "missing", Channel: NotificationChannelLog, TemplateVersion: 1, DedupeKey: "bad-json", PayloadJSON: []byte(`[]`)},
	} {
		if _, err := store.CreateNotificationDelivery(ctx, input); err == nil {
			t.Fatalf("unsafe input accepted: %+v", input)
		}
	}
}

func appendNotificationTestEvent(t *testing.T, ctx context.Context, store *Store, id, eventType string) AppendedEvent {
	t.Helper()
	event, err := store.AppendEventWithOutbox(ctx, DomainEventInput{
		ID: id, AggregateType: "pull_request", AggregateID: "pr-1", EventType: eventType,
		EventVersion: 1, Payload: []byte(`{"pull_request_id":"pr-1"}`), OccurredAt: time.Unix(1, 0).UTC(),
	}, []OutboxInput{{Topic: "notification", Payload: []byte(`{"event_id":"` + id + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return event
}
