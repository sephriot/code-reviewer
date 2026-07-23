// Package notificationworker records local notification outcomes from durable
// jobs. It has no network, browser, sound, or text-to-speech dependency.
package notificationworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// DeliverJobKind is durable job type for one retained local notification.
const DeliverJobKind = "notification.deliver.v1"

const maxDeliveryIDBytes = 256

// SchedulerStore is narrow durable scheduling boundary used by Scheduler.
type SchedulerStore interface {
	EnsureJob(context.Context, sqlite.JobInput) (sqlite.EnsureJobResult, error)
}

// Scheduler queues one retained notification delivery. It never inspects
// payload or invokes a local channel.
type Scheduler struct {
	Store SchedulerStore
	Now   func() time.Time
}

// Schedule returns matching active delivery job or creates it.
func (s Scheduler) Schedule(ctx context.Context, deliveryID string) (sqlite.EnsureJobResult, error) {
	if s.Store == nil {
		return sqlite.EnsureJobResult{}, errors.New("notification delivery job store is required")
	}
	if !validDeliveryID(deliveryID) {
		return sqlite.EnsureJobResult{}, errors.New("notification delivery ID is invalid")
	}
	payload, err := json.Marshal(jobPayload{DeliveryID: deliveryID})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("encode notification delivery job payload: %w", err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	result, err := s.Store.EnsureJob(ctx, sqlite.JobInput{
		Kind: DeliverJobKind, ResourceType: "notification_delivery", ResourceID: deliveryID,
		DedupeKey: DeliverJobKind + ":" + deliveryID, Payload: payload,
		AvailableAt: now, MaxAttempts: 3,
	})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("ensure notification delivery job: %w", err)
	}
	return result, nil
}

// DeliveryLoader loads retained delivery facts and progress.
type DeliveryLoader interface {
	LoadNotificationDelivery(context.Context, string) (sqlite.NotificationDeliveryTarget, error)
}

// OutcomeRecorder atomically retains one terminal local delivery outcome.
type OutcomeRecorder interface {
	RecordNotificationDeliveryOutcome(context.Context, sqlite.NotificationDeliveryOutcome) (sqlite.RecordNotificationDeliveryOutcomeResult, error)
}

// PreferencesLoader loads only machine-local notification settings required by
// local sound and speech adapters.
type PreferencesLoader interface {
	LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error)
}

// LocalNotifier invokes a configured machine-local sound or speech adapter.
// It has no browser or network capability.
type LocalNotifier interface {
	PlaySound(context.Context, string) error
	Speak(context.Context, string, int) error
}

// ErrLocalNotifierUnavailable lets a host suppress channels unsupported on
// this machine without retrying a job forever.
var ErrLocalNotifierUnavailable = errors.New("local notification adapter is unavailable")

// PrintfLogger allows tests and host programs to receive bounded log delivery
// messages. It receives no notification payload or local preference data.
type PrintfLogger interface {
	Printf(string, ...any)
}

// Handler dispatches only safe local actions. Log deliveries write a bounded
// metadata line. Browser deliveries stay queued for an open loopback dashboard
// to claim; sound and TTS are explicitly suppressed until local adapters exist.
type Handler struct {
	Loader        DeliveryLoader
	Recorder      OutcomeRecorder
	Logger        PrintfLogger
	Preferences   PreferencesLoader
	LocalNotifier LocalNotifier
	Now           func() time.Time
}

// Handle implements worker.Handler.
func (h Handler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != DeliverJobKind {
		return worker.Permanent(errors.New("unexpected notification delivery job kind"))
	}
	deliveryID, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed notification delivery job payload: %w", err))
	}
	if h.Loader == nil || h.Recorder == nil {
		return worker.Permanent(errors.New("notification delivery handler dependencies are required"))
	}

	delivery, err := h.Loader.LoadNotificationDelivery(ctx, deliveryID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotificationDeliveryNotFound) {
			return worker.Permanent(errors.New("notification delivery is no longer available"))
		}
		return errors.New("load notification delivery failed")
	}
	if delivery.ID != deliveryID {
		return worker.Permanent(errors.New("notification delivery is no longer available"))
	}
	if delivery.State == sqlite.NotificationDeliveryDelivered || delivery.State == sqlite.NotificationDeliverySuppressed {
		return nil
	}
	if delivery.State != sqlite.NotificationDeliveryQueued {
		return worker.Permanent(errors.New("notification delivery is not pending"))
	}

	outcome := sqlite.NotificationDeliverySuppressed
	switch delivery.Channel {
	case sqlite.NotificationChannelLog:
		outcome = sqlite.NotificationDeliveryDelivered
		logger := h.Logger
		if logger == nil {
			logger = log.Default()
		}
		logger.Printf("notification delivered id=%q event_type=%q channel=%q", delivery.ID, delivery.EventType, delivery.Channel)
	case sqlite.NotificationChannelBrowser:
		// Browser capability lives in the loopback dashboard process, not this
		// background worker. Leave the durable item queued for dashboard polling.
		return nil
	case sqlite.NotificationChannelSound, sqlite.NotificationChannelTTS:
		outcome, err = h.deliverLocal(ctx, delivery)
		if err != nil {
			return err
		}
	default:
		return worker.Permanent(errors.New("notification delivery channel is unsupported"))
	}

	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	if _, err := h.Recorder.RecordNotificationDeliveryOutcome(ctx, sqlite.NotificationDeliveryOutcome{
		ID: deliveryID, State: outcome, AttemptedAt: now,
	}); err != nil {
		if errors.Is(err, sqlite.ErrNotificationDeliveryNotFound) || errors.Is(err, sqlite.ErrNotificationDeliveryNotPending) {
			return worker.Permanent(errors.New("notification delivery is no longer pending"))
		}
		return errors.New("record notification delivery outcome failed")
	}
	return nil
}

func (h Handler) deliverLocal(ctx context.Context, delivery sqlite.NotificationDeliveryTarget) (sqlite.NotificationDeliveryState, error) {
	if h.Preferences == nil || h.LocalNotifier == nil {
		return sqlite.NotificationDeliverySuppressed, nil
	}
	preferences, err := h.Preferences.LoadNotificationPreferences(ctx)
	if err != nil {
		return "", errors.New("load local notification preferences failed")
	}
	switch delivery.Channel {
	case sqlite.NotificationChannelSound:
		err = h.LocalNotifier.PlaySound(ctx, preferences.CustomSoundPath)
	case sqlite.NotificationChannelTTS:
		message := speechMessage(preferences.EventTemplatesJSON, delivery.EventType)
		if message == "" {
			return sqlite.NotificationDeliverySuppressed, nil
		}
		err = h.LocalNotifier.Speak(ctx, message, preferences.SpeechRateMilli)
	default:
		return "", worker.Permanent(errors.New("local notification channel is unsupported"))
	}
	if errors.Is(err, ErrLocalNotifierUnavailable) {
		return sqlite.NotificationDeliverySuppressed, nil
	}
	if err != nil {
		return "", errors.New("deliver local notification failed")
	}
	return sqlite.NotificationDeliveryDelivered, nil
}

func speechMessage(raw []byte, eventType string) string {
	values := map[string]string{}
	if json.Unmarshal(raw, &values) == nil {
		if value, found := values[eventType]; found {
			return value
		}
	}
	switch eventType {
	case "review.started":
		return "Review started."
	case "review.completed":
		return "Review completed."
	case "review.failed":
		return "Review failed."
	default:
		return ""
	}
}

type jobPayload struct {
	DeliveryID string `json:"delivery_id"`
}

func parseJobPayload(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload jobPayload
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("must be a single supported JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return "", errors.New("must be a single JSON object")
	}
	if !validDeliveryID(payload.DeliveryID) {
		return "", errors.New("delivery ID is invalid")
	}
	return payload.DeliveryID, nil
}

func validDeliveryID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxDeliveryIDBytes {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra value")
	}
	return err
}

var _ worker.Handler = Handler{}
