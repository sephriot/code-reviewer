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

type fakeBrowserNotificationDeliveries struct {
	items   []sqlite.NotificationDeliveryTarget
	listErr error
	outcome sqlite.NotificationDeliveryOutcome
	err     error
}

func (f *fakeBrowserNotificationDeliveries) ListQueuedBrowserNotificationDeliveries(context.Context, int) ([]sqlite.NotificationDeliveryTarget, error) {
	return f.items, f.listErr
}

func (f *fakeBrowserNotificationDeliveries) RecordNotificationDeliveryOutcome(_ context.Context, outcome sqlite.NotificationDeliveryOutcome) (sqlite.RecordNotificationDeliveryOutcomeResult, error) {
	f.outcome = outcome
	if f.err != nil {
		return sqlite.RecordNotificationDeliveryOutcomeResult{}, f.err
	}
	return sqlite.RecordNotificationDeliveryOutcomeResult{Recorded: true}, nil
}

func TestBrowserNotificationDeliveryRoutesListAndAcknowledge(t *testing.T) {
	now := time.Unix(300, 0).UTC()
	store := &fakeBrowserNotificationDeliveries{items: []sqlite.NotificationDeliveryTarget{{ID: "delivery-1", EventType: "policy.evaluated", Channel: sqlite.NotificationChannelBrowser, State: sqlite.NotificationDeliveryQueued}}}
	mux := http.NewServeMux()
	registerBrowserNotificationDeliveryRoutes(mux, BrowserNotificationDeliveryOptions{Store: store, Now: func() time.Time { return now }})
	list := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, browserNotificationDeliveriesPath, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	mux.ServeHTTP(list, request)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var page browserNotificationDeliveryPageResponse
	if err := json.NewDecoder(list.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "delivery-1" || page.Items[0].EventType != "policy.evaluated" {
		t.Fatalf("page = %+v", page)
	}
	inner := mux
	guarded := newMutationAuth(time.Now).Wrap(inner)
	session := httptest.NewRecorder()
	sessionRequest := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	sessionRequest.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(session, sessionRequest)
	ack := httptest.NewRecorder()
	ackRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/notification-deliveries/browser/delivery-1/outcome", strings.NewReader(`{"state":"delivered"}`))
	ackRequest.Header.Set("Content-Type", jsonContentType)
	ackRequest.RemoteAddr = "127.0.0.1:1234"
	ackRequest.Header.Set("Cookie", session.Result().Header.Get("Set-Cookie"))
	guarded.ServeHTTP(ack, ackRequest)
	if ack.Code != http.StatusNoContent || store.outcome.ID != "delivery-1" || store.outcome.State != sqlite.NotificationDeliveryDelivered || !store.outcome.AttemptedAt.Equal(now) {
		t.Fatalf("ack status=%d outcome=%+v body=%s", ack.Code, store.outcome, ack.Body.String())
	}
}

func TestBrowserNotificationDeliveryRoutesFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		store  *fakeBrowserNotificationDeliveries
		path   string
		body   string
		remote string
		want   int
	}{
		{name: "remote list", store: &fakeBrowserNotificationDeliveries{}, path: browserNotificationDeliveriesPath, remote: "192.0.2.1:1234", want: http.StatusForbidden},
		{name: "bad outcome", store: &fakeBrowserNotificationDeliveries{}, path: "/api/v1/mutate/notification-deliveries/browser/delivery-1/outcome", body: `{"state":"queued"}`, remote: "127.0.0.1:1234", want: http.StatusBadRequest},
		{name: "missing", store: &fakeBrowserNotificationDeliveries{err: sqlite.ErrNotificationDeliveryNotFound}, path: "/api/v1/mutate/notification-deliveries/browser/delivery-1/outcome", body: `{"state":"suppressed"}`, remote: "127.0.0.1:1234", want: http.StatusNotFound},
		{name: "unavailable", store: &fakeBrowserNotificationDeliveries{listErr: errors.New("offline")}, path: browserNotificationDeliveriesPath, remote: "127.0.0.1:1234", want: http.StatusServiceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registerBrowserNotificationDeliveryRoutes(mux, BrowserNotificationDeliveryOptions{Store: testCase.store})
			response := httptest.NewRecorder()
			method := http.MethodGet
			if testCase.body != "" {
				method = http.MethodPost
			}
			request := httptest.NewRequest(method, testCase.path, strings.NewReader(testCase.body))
			request.RemoteAddr = testCase.remote
			if testCase.body != "" {
				request.Header.Set("Content-Type", jsonContentType)
			}
			mux.ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}
