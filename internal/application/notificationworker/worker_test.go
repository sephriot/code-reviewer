package notificationworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestHandlerRecordsLogDelivery(t *testing.T) {
	loader := &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", EventType: "review_observed", Channel: sqlite.NotificationChannelLog, State: sqlite.NotificationDeliveryQueued}}
	recorder := &outcomeRecorder{}
	logger := &testLogger{}
	handler := Handler{Loader: loader, Recorder: recorder, Logger: logger, Now: func() time.Time { return time.Unix(7, 0).UTC() }}

	if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.outcome.ID != "delivery-1" || recorder.outcome.State != sqlite.NotificationDeliveryDelivered || !recorder.outcome.AttemptedAt.Equal(time.Unix(7, 0).UTC()) {
		t.Fatalf("outcome = %+v", recorder.outcome)
	}
	if !strings.Contains(logger.message, "delivery-1") || strings.Contains(logger.message, "payload") {
		t.Fatalf("log = %q", logger.message)
	}
}

func TestHandlerLeavesBrowserDeliveryForDashboardClaim(t *testing.T) {
	recorder := &outcomeRecorder{}
	handler := Handler{Loader: &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", Channel: sqlite.NotificationChannelBrowser, State: sqlite.NotificationDeliveryQueued}}, Recorder: recorder}
	if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.called {
		t.Fatalf("browser delivery was finalized by worker: %+v", recorder)
	}
}

func TestHandlerSuppressesUnconfiguredLocalChannels(t *testing.T) {
	for _, channel := range []sqlite.NotificationChannel{sqlite.NotificationChannelSound, sqlite.NotificationChannelTTS} {
		t.Run(string(channel), func(t *testing.T) {
			recorder := &outcomeRecorder{}
			handler := Handler{Loader: &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", Channel: channel, State: sqlite.NotificationDeliveryQueued}}, Recorder: recorder}
			if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
				t.Fatal(err)
			}
			if recorder.outcome.State != sqlite.NotificationDeliverySuppressed {
				t.Fatalf("outcome = %+v", recorder.outcome)
			}
		})
	}
}

func TestHandlerDeliversConfiguredSoundAndSpeech(t *testing.T) {
	for _, testCase := range []struct {
		channel sqlite.NotificationChannel
	}{
		{channel: sqlite.NotificationChannelSound},
		{channel: sqlite.NotificationChannelTTS},
	} {
		t.Run(string(testCase.channel), func(t *testing.T) {
			notifier := &localNotifier{}
			recorder := &outcomeRecorder{}
			handler := Handler{
				Loader:   &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", EventType: "policy.evaluated", Channel: testCase.channel, State: sqlite.NotificationDeliveryQueued}},
				Recorder: recorder, Preferences: &preferencesLoader{preferences: sqlite.NotificationPreferences{SpeechRateMilli: 1250, CustomSoundPath: "/tmp/tone.aiff"}}, LocalNotifier: notifier,
			}
			if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
				t.Fatal(err)
			}
			wantState := sqlite.NotificationDeliveryDelivered
			if testCase.channel == sqlite.NotificationChannelTTS {
				wantState = sqlite.NotificationDeliverySuppressed
			}
			if recorder.outcome.State != wantState {
				t.Fatalf("outcome=%+v", recorder.outcome)
			}
			if testCase.channel == sqlite.NotificationChannelSound && notifier.sound != "/tmp/tone.aiff" {
				t.Fatalf("sound=%q", notifier.sound)
			}
			if testCase.channel == sqlite.NotificationChannelTTS && notifier.message != "" {
				t.Fatalf("message=%q rate=%d", notifier.message, notifier.rate)
			}
		})
	}
}

func TestHandlerSpeaksConfiguredTemplate(t *testing.T) {
	notifier := &localNotifier{}
	recorder := &outcomeRecorder{}
	handler := Handler{
		Loader:        &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", EventType: "review.completed", PayloadJSON: []byte(`{"repository":"acme/widgets","number":42,"title":"Fix widget","author":"octocat"}`), Channel: sqlite.NotificationChannelTTS, State: sqlite.NotificationDeliveryQueued}},
		Recorder:      recorder,
		Preferences:   &preferencesLoader{preferences: sqlite.NotificationPreferences{SpeechRateMilli: 1250, EventTemplatesJSON: []byte(`{"review.completed":"{repository} #{number}: {title} by {author}"}`)}},
		LocalNotifier: notifier,
	}
	if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.outcome.State != sqlite.NotificationDeliveryDelivered || notifier.message != "acme/widgets #42: Fix widget by octocat" || notifier.rate != 1250 {
		t.Fatalf("outcome=%+v message=%q rate=%d", recorder.outcome, notifier.message, notifier.rate)
	}
}

func TestHandlerSuppressesEmptyConfiguredSpeechTemplate(t *testing.T) {
	notifier := &localNotifier{}
	recorder := &outcomeRecorder{}
	handler := Handler{
		Loader:   &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", EventType: "review.completed", Channel: sqlite.NotificationChannelTTS, State: sqlite.NotificationDeliveryQueued}},
		Recorder: recorder, Preferences: &preferencesLoader{preferences: sqlite.NotificationPreferences{SpeechRateMilli: 1000, EventTemplatesJSON: []byte(`{"review.completed":""}`)}}, LocalNotifier: notifier,
	}
	if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.outcome.State != sqlite.NotificationDeliverySuppressed || notifier.message != "" {
		t.Fatalf("outcome=%+v message=%q", recorder.outcome, notifier.message)
	}
}

func TestHandlerRejectsMalformedPayloadBeforeLoading(t *testing.T) {
	loader := &deliveryLoader{}
	handler := Handler{Loader: loader, Recorder: &outcomeRecorder{}}
	for _, payload := range []string{
		`{}`, `{"delivery_id":"delivery-1","extra":true}`, `{"delivery_id":" delivery-1"}`,
		`{"delivery_id":"delivery/1"}`, `{"delivery_id":"delivery-1"} null`,
	} {
		err := handler.Handle(context.Background(), notificationJob(payload))
		if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	if loader.id != "" {
		t.Fatalf("loader called for malformed payload: %+v", loader)
	}
}

func TestHandlerCompletesFinalDeliveryWithoutNewAttempt(t *testing.T) {
	recorder := &outcomeRecorder{}
	handler := Handler{Loader: &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", Channel: sqlite.NotificationChannelLog, State: sqlite.NotificationDeliveryDelivered}}, Recorder: recorder}
	if err := handler.Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.called {
		t.Fatalf("final delivery recorded again: %+v", recorder)
	}
}

func TestHandlerClassifiesUnavailableAndStorageFailures(t *testing.T) {
	tests := []struct {
		name      string
		loader    *deliveryLoader
		recorder  *outcomeRecorder
		permanent bool
	}{
		{name: "missing", loader: &deliveryLoader{err: sqlite.ErrNotificationDeliveryNotFound}, recorder: &outcomeRecorder{}, permanent: true},
		{name: "not pending", loader: &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", Channel: sqlite.NotificationChannelLog, State: sqlite.NotificationDeliveryFailed}}, recorder: &outcomeRecorder{}, permanent: true},
		{name: "load transient", loader: &deliveryLoader{err: errors.New("database offline")}, recorder: &outcomeRecorder{}},
		{name: "record transient", loader: &deliveryLoader{target: sqlite.NotificationDeliveryTarget{ID: "delivery-1", Channel: sqlite.NotificationChannelLog, State: sqlite.NotificationDeliveryQueued}}, recorder: &outcomeRecorder{err: errors.New("database offline")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Handler{Loader: test.loader, Recorder: test.recorder}).Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`))
			if err == nil || worker.IsPermanent(err) != test.permanent || strings.Contains(err.Error(), "offline") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHandlerRejectsInvalidDependenciesAndJobKind(t *testing.T) {
	if err := (Handler{}).Handle(context.Background(), notificationJob(`{"delivery_id":"delivery-1"}`)); err == nil || !worker.IsPermanent(err) {
		t.Fatalf("dependencies err=%v", err)
	}
	job := notificationJob(`{"delivery_id":"delivery-1"}`)
	job.Kind = "other"
	if err := (Handler{}).Handle(context.Background(), job); err == nil || !worker.IsPermanent(err) {
		t.Fatalf("kind err=%v", err)
	}
}

func TestSchedulerQueuesOneDeliveryBoundJob(t *testing.T) {
	store := &schedulerStore{}
	result, err := (Scheduler{Store: store, Now: func() time.Time { return time.Unix(7, 0).UTC() }}).Schedule(context.Background(), "delivery-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "job-1" || !result.Created {
		t.Fatalf("result = %+v", result)
	}
	input := store.input
	if input.Kind != DeliverJobKind || input.ResourceType != "notification_delivery" || input.ResourceID != "delivery-1" ||
		input.DedupeKey != DeliverJobKind+":delivery-1" || string(input.Payload) != `{"delivery_id":"delivery-1"}` ||
		!input.AvailableAt.Equal(time.Unix(7, 0).UTC()) || input.MaxAttempts != 3 {
		t.Fatalf("job input = %+v", input)
	}
}

func TestSchedulerRejectsInvalidDeliveryBeforeWriting(t *testing.T) {
	store := &schedulerStore{}
	for _, deliveryID := range []string{"", " delivery-1", "delivery/1"} {
		if _, err := (Scheduler{Store: store}).Schedule(context.Background(), deliveryID); err == nil || !strings.Contains(err.Error(), "delivery ID") {
			t.Fatalf("delivery ID %q error = %v", deliveryID, err)
		}
	}
	if store.called {
		t.Fatalf("store called for invalid delivery: %+v", store)
	}
}

func TestSchedulerRequiresStore(t *testing.T) {
	if _, err := (Scheduler{}).Schedule(context.Background(), "delivery-1"); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("Schedule() error = %v", err)
	}
}

func notificationJob(payload string) sqlite.Job {
	return sqlite.Job{Kind: DeliverJobKind, Payload: []byte(payload)}
}

type deliveryLoader struct {
	id     string
	target sqlite.NotificationDeliveryTarget
	err    error
}

func (l *deliveryLoader) LoadNotificationDelivery(_ context.Context, id string) (sqlite.NotificationDeliveryTarget, error) {
	l.id = id
	return l.target, l.err
}

type outcomeRecorder struct {
	outcome sqlite.NotificationDeliveryOutcome
	called  bool
	err     error
}

func (r *outcomeRecorder) RecordNotificationDeliveryOutcome(_ context.Context, outcome sqlite.NotificationDeliveryOutcome) (sqlite.RecordNotificationDeliveryOutcomeResult, error) {
	r.called, r.outcome = true, outcome
	return sqlite.RecordNotificationDeliveryOutcomeResult{}, r.err
}

type testLogger struct{ message string }

func (l *testLogger) Printf(format string, arguments ...any) {
	l.message = fmt.Sprintf(format, arguments...)
}

type schedulerStore struct {
	input  sqlite.JobInput
	called bool
	err    error
}

type preferencesLoader struct {
	preferences sqlite.NotificationPreferences
	err         error
}

func (l *preferencesLoader) LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error) {
	return l.preferences, l.err
}

type localNotifier struct {
	sound   string
	message string
	rate    int
	err     error
}

func (n *localNotifier) PlaySound(_ context.Context, path string) error { n.sound = path; return n.err }
func (n *localNotifier) Speak(_ context.Context, message string, rate int) error {
	n.message, n.rate = message, rate
	return n.err
}

func (s *schedulerStore) EnsureJob(_ context.Context, input sqlite.JobInput) (sqlite.EnsureJobResult, error) {
	s.called, s.input = true, input
	if s.err != nil {
		return sqlite.EnsureJobResult{}, s.err
	}
	return sqlite.EnsureJobResult{ID: "job-1", Created: true}, nil
}
