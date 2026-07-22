package notificationdispatch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestDispatchCreatesAndSchedulesEnabledChannels(t *testing.T) {
	now := time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC)
	store := &fakeStore{preferences: sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"browser":true,"log":true,"sound":false,"tts":true}`)}}
	scheduler := &fakeScheduler{}

	result, err := (Service{Store: store, Scheduler: scheduler, Now: func() time.Time { return now }}).Dispatch(context.Background(), Request{
		DomainEventID: "event-1", DedupeKey: "policy-evaluation:assessment-1", PayloadJSON: []byte(`{"assessment_id":"assessment-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Muted || !reflect.DeepEqual(result.DeliveryIDs, []string{"delivery-browser", "delivery-log", "delivery-tts"}) {
		t.Fatalf("result = %+v", result)
	}
	if len(store.inputs) != 3 {
		t.Fatalf("deliveries = %+v", store.inputs)
	}
	for index, channel := range []sqlite.NotificationChannel{sqlite.NotificationChannelBrowser, sqlite.NotificationChannelLog, sqlite.NotificationChannelTTS} {
		input := store.inputs[index]
		if input.DomainEventID != "event-1" || input.Channel != channel || input.TemplateVersion != 1 || input.DedupeKey != "policy-evaluation:assessment-1" || string(input.PayloadJSON) != `{"assessment_id":"assessment-1"}` || !input.AvailableAt.Equal(now) || !input.CreatedAt.Equal(now) {
			t.Fatalf("delivery %d = %+v", index, input)
		}
	}
	if !reflect.DeepEqual(scheduler.ids, result.DeliveryIDs) {
		t.Fatalf("scheduled = %v, want %v", scheduler.ids, result.DeliveryIDs)
	}
}

func TestDispatchSkipsMutedAndDisabledChannels(t *testing.T) {
	now := time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name        string
		preferences sqlite.NotificationPreferences
		muted       bool
	}{
		{name: "muted", preferences: sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"browser":true,"log":true,"sound":true,"tts":true}`), MutedUntil: pointer(now.Add(time.Minute))}, muted: true},
		{name: "all disabled", preferences: sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"browser":false,"log":false,"sound":false,"tts":false}`)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{preferences: testCase.preferences}
			scheduler := &fakeScheduler{}
			result, err := (Service{Store: store, Scheduler: scheduler, Now: func() time.Time { return now }}).Dispatch(context.Background(), Request{DomainEventID: "event-1", DedupeKey: "event-1", PayloadJSON: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if result.Muted != testCase.muted || len(result.DeliveryIDs) != 0 || len(store.inputs) != 0 || len(scheduler.ids) != 0 {
				t.Fatalf("result=%+v inputs=%+v scheduled=%v", result, store.inputs, scheduler.ids)
			}
		})
	}
}

func TestDispatchRejectsInvalidPreferencesAndDependencies(t *testing.T) {
	valid := sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"browser":false,"log":true,"sound":false,"tts":false}`)}
	for _, testCase := range []struct {
		name    string
		service Service
		request Request
	}{
		{name: "missing store", service: Service{}, request: validRequest()},
		{name: "missing scheduler", service: Service{Store: &fakeStore{preferences: valid}}, request: validRequest()},
		{name: "missing event", service: Service{Store: &fakeStore{preferences: valid}, Scheduler: &fakeScheduler{}}, request: Request{DedupeKey: "event-1", PayloadJSON: []byte(`{}`)}},
		{name: "bad channels", service: Service{Store: &fakeStore{preferences: sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"log":"yes"}`)}}, Scheduler: &fakeScheduler{}}, request: validRequest()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.service.Dispatch(context.Background(), testCase.request); err == nil {
				t.Fatal("Dispatch error = nil")
			}
		})
	}
}

func TestDispatchDoesNotScheduleAfterCreateFailure(t *testing.T) {
	store := &fakeStore{preferences: sqlite.NotificationPreferences{ChannelsJSON: []byte(`{"browser":false,"log":true,"sound":false,"tts":false}`)}, createErr: errors.New("database offline")}
	scheduler := &fakeScheduler{}
	_, err := (Service{Store: store, Scheduler: scheduler}).Dispatch(context.Background(), validRequest())
	if err == nil || len(scheduler.ids) != 0 {
		t.Fatalf("err=%v scheduled=%v", err, scheduler.ids)
	}
}

func validRequest() Request {
	return Request{DomainEventID: "event-1", DedupeKey: "event-1", PayloadJSON: []byte(`{}`)}
}

func pointer(value time.Time) *time.Time { return &value }

type fakeStore struct {
	preferences sqlite.NotificationPreferences
	inputs      []sqlite.CreateNotificationDeliveryInput
	loadErr     error
	createErr   error
}

func (s *fakeStore) LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error) {
	return s.preferences, s.loadErr
}

func (s *fakeStore) CreateNotificationDelivery(_ context.Context, input sqlite.CreateNotificationDeliveryInput) (sqlite.CreateNotificationDeliveryResult, error) {
	s.inputs = append(s.inputs, input)
	if s.createErr != nil {
		return sqlite.CreateNotificationDeliveryResult{}, s.createErr
	}
	return sqlite.CreateNotificationDeliveryResult{ID: "delivery-" + string(input.Channel), State: sqlite.NotificationDeliveryQueued, Created: true}, nil
}

type fakeScheduler struct{ ids []string }

func (s *fakeScheduler) Schedule(_ context.Context, id string) (sqlite.EnsureJobResult, error) {
	s.ids = append(s.ids, id)
	return sqlite.EnsureJobResult{ID: "job-" + id, Created: true}, nil
}
