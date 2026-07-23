package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

type fakeNotificationPreferences struct {
	preferences sqlite.NotificationPreferences
	updateInput sqlite.UpdateNotificationPreferencesInput
	loadErr     error
	updateErr   error
}

func (f *fakeNotificationPreferences) LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error) {
	if f.loadErr != nil {
		return sqlite.NotificationPreferences{}, f.loadErr
	}
	return f.preferences, nil
}

func (f *fakeNotificationPreferences) UpdateNotificationPreferences(_ context.Context, input sqlite.UpdateNotificationPreferencesInput) (sqlite.NotificationPreferences, error) {
	f.updateInput = input
	if f.updateErr != nil {
		return sqlite.NotificationPreferences{}, f.updateErr
	}
	f.preferences = sqlite.NotificationPreferences{
		Version: input.ExpectedVersion + 1, ChannelsJSON: input.ChannelsJSON, QuietHoursJSON: input.QuietHoursJSON,
		EventTemplatesJSON: input.EventTemplatesJSON, MutedUntil: input.MutedUntil, SpeechRateMilli: input.SpeechRateMilli,
		CustomSoundPath: input.CustomSoundPath, UpdatedAt: input.UpdatedAt,
	}
	return f.preferences, nil
}

func TestNotificationPreferencesRoutesExposeAndUpdateSpeechTemplates(t *testing.T) {
	t.Parallel()
	mutedUntil := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	preferences := &fakeNotificationPreferences{preferences: sqlite.NotificationPreferences{
		Version: 4, ChannelsJSON: []byte(`{"browser":true,"log":false,"sound":true,"tts":false,"private_future_value":"do-not-expose"}`),
		QuietHoursJSON: []byte(`{"secret":"do-not-expose"}`), EventTemplatesJSON: []byte(`{"review.completed":"Finished","secret":"do-not-expose"}`),
		MutedUntil: &mutedUntil, SpeechRateMilli: 1200, CustomSoundPath: "/private/sound.wav", UpdatedAt: time.Unix(20, 0).UTC(),
	}}
	now := time.Unix(100, 0).UTC()
	handler := NewNotificationPreferencesHandler(NotificationPreferencesOptions{Store: preferences, Now: func() time.Time { return now }})

	get := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notification-preferences", nil)
	getRequest.RemoteAddr = "127.0.0.1:1234"
	handler.ServeHTTP(get, getRequest)
	if get.Code != http.StatusOK || get.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("get status=%d headers=%v", get.Code, get.Header())
	}
	var loaded notificationPreferencesResponse
	if err := json.NewDecoder(get.Body).Decode(&loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 4 || !loaded.Channels.Browser || loaded.Channels.Log || !loaded.Channels.Sound || loaded.Channels.TTS || loaded.SpeechRateMilli != 1200 || loaded.MutedUntil == nil || !loaded.MutedUntil.Equal(mutedUntil) || loaded.SpeechTemplates.ReviewCompleted != "Finished" {
		t.Fatalf("safe response = %+v", loaded)
	}
	if strings.Contains(get.Body.String(), "secret") || strings.Contains(get.Body.String(), "private") || strings.Contains(get.Body.String(), "path") {
		t.Fatalf("unsafe preference fields exposed: %s", get.Body.String())
	}

	post := httptest.NewRecorder()
	postRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-preferences", strings.NewReader(`{"expected_version":4,"channels":{"browser":false,"log":true,"sound":false,"tts":true},"muted_until":null,"speech_rate_milli":900,"speech_templates":{"review.started":"Starting","review.completed":"Done","review.failed":"Failed","policy.evaluated":""}}`))
	postRequest.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(post, postRequest)
	if post.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", post.Code, post.Body.String())
	}
	if preferences.updateInput.ExpectedVersion != 4 || string(preferences.updateInput.ChannelsJSON) != `{"browser":false,"log":true,"private_future_value":"do-not-expose","sound":false,"tts":true}` ||
		string(preferences.updateInput.QuietHoursJSON) != `{"secret":"do-not-expose"}` || string(preferences.updateInput.EventTemplatesJSON) != `{"policy.evaluated":"","review.completed":"Done","review.failed":"Failed","review.started":"Starting","secret":"do-not-expose"}` || preferences.updateInput.MutedUntil != nil || preferences.updateInput.SpeechRateMilli != 900 || preferences.updateInput.CustomSoundPath != "/private/sound.wav" || !preferences.updateInput.UpdatedAt.Equal(now) {
		t.Fatalf("store input=%+v", preferences.updateInput)
	}
}

func TestNotificationPreferencesRoutesRejectUnsafeOrMalformedRequests(t *testing.T) {
	t.Parallel()
	preferences := &fakeNotificationPreferences{preferences: sqlite.NotificationPreferences{Version: 1, ChannelsJSON: []byte(`{"browser":false,"log":true,"sound":false,"tts":false}`), QuietHoursJSON: []byte(`{}`), EventTemplatesJSON: []byte(`{}`), SpeechRateMilli: 1000}}
	handler := NewNotificationPreferencesHandler(NotificationPreferencesOptions{Store: preferences})

	remote := httptest.NewRecorder()
	remoteRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notification-preferences", nil)
	remoteRequest.RemoteAddr = "198.51.100.7:443"
	handler.ServeHTTP(remote, remoteRequest)
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote get status=%d", remote.Code)
	}

	for _, testCase := range []struct{ contentType, body string }{
		{"text/plain", `{}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false},"muted_until":null,"speech_rate_milli":1000,"unknown":true}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false},"muted_until":null,"speech_rate_milli":1000}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false,"external":true},"muted_until":null,"speech_rate_milli":1000}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":"yes","log":true,"sound":false,"tts":false},"muted_until":null,"speech_rate_milli":1000}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false},"muted_until":"not-a-time","speech_rate_milli":1000}`},
		{jsonContentType, `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false},"muted_until":null,"speech_rate_milli":499}`},
		{jsonContentType, `{} {}`},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-preferences", strings.NewReader(testCase.body))
		request.Header.Set("Content-Type", testCase.contentType)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d response=%s", testCase.body, response.Code, response.Body.String())
		}
	}
	if preferences.updateInput.ExpectedVersion != 0 {
		t.Fatalf("invalid requests updated store: %+v", preferences.updateInput)
	}
}

func TestNotificationPreferencesRoutesMapConflictsAndNeedOuterGuard(t *testing.T) {
	t.Parallel()
	preferences := &fakeNotificationPreferences{preferences: sqlite.NotificationPreferences{Version: 1, ChannelsJSON: []byte(`{"browser":false,"log":true,"sound":false,"tts":false}`), QuietHoursJSON: []byte(`{}`), EventTemplatesJSON: []byte(`{}`), SpeechRateMilli: 1000}, updateErr: sqlite.ErrNotificationPreferencesConflict}
	inner := NewNotificationPreferencesHandler(NotificationPreferencesOptions{Store: preferences})
	body := `{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false},"muted_until":null,"speech_rate_milli":1000,"speech_templates":{"review.started":"Start","review.completed":"Done","review.failed":"Failed","policy.evaluated":""}}`
	if response := serveNotificationPreferenceMutation(inner, body); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "notification_preferences_conflict") {
		t.Fatalf("conflict status=%d body=%s", response.Code, response.Body.String())
	}

	preferences.updateErr = errors.New("database unavailable")
	if response := serveNotificationPreferenceMutation(inner, body); response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "mutation_failed") {
		t.Fatalf("failure status=%d body=%s", response.Code, response.Body.String())
	}

	guarded := newMutationAuth(time.Now).Wrap(inner)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-preferences", strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	guarded.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("guarded status=%d", response.Code)
	}
}

func TestControlRegistersNotificationPreferencesRoutes(t *testing.T) {
	t.Parallel()
	preferences := &fakeNotificationPreferences{preferences: sqlite.NotificationPreferences{
		Version: 1, ChannelsJSON: []byte(`{"browser":false,"log":true,"sound":false,"tts":false}`),
		QuietHoursJSON: []byte(`{}`), EventTemplatesJSON: []byte(`{}`), SpeechRateMilli: 1000,
	}}
	control := NewControlHandler(Readiness{}, ControlOptions{NotificationPreferences: NotificationPreferencesOptions{Store: preferences}})
	guarded := newMutationAuth(time.Now).Wrap(control)

	get := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, notificationPreferencesPath, nil)
	getRequest.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(get, getRequest)
	if get.Code != http.StatusOK {
		t.Fatalf("registered get status=%d body=%s", get.Code, get.Body.String())
	}

	post := httptest.NewRecorder()
	postRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-preferences", strings.NewReader(`{"expected_version":1,"channels":{"browser":true,"log":true,"sound":false,"tts":false},"muted_until":null,"speech_rate_milli":1000}`))
	postRequest.Header.Set("Content-Type", jsonContentType)
	postRequest.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(post, postRequest)
	if post.Code != http.StatusUnauthorized {
		t.Fatalf("registered mutation status=%d body=%s", post.Code, post.Body.String())
	}
}

func serveNotificationPreferenceMutation(handler http.Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-preferences", strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(response, request)
	return response
}
