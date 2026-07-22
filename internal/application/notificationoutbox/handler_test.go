package notificationoutbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/application/notificationdispatch"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestHandlerDispatchesStrictEnvelope(t *testing.T) {
	dispatcher := &fakeDispatcher{}
	err := (Handler{Dispatcher: dispatcher}).Handle(context.Background(), sqlite.OutboxDelivery{Topic: DispatchTopic, Payload: []byte(`{"domain_event_id":"event-1","dedupe_key":"policy-evaluation-1","payload":{"policy_evaluation_id":"evaluation-1"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if dispatcher.request.DomainEventID != "event-1" || dispatcher.request.DedupeKey != "policy-evaluation-1" || string(dispatcher.request.PayloadJSON) != `{"policy_evaluation_id":"evaluation-1"}` {
		t.Fatalf("request=%+v", dispatcher.request)
	}
}

func TestHandlerRejectsUnsafeEnvelopesBeforeDispatch(t *testing.T) {
	for _, testCase := range []struct{ name, topic, payload string }{
		{name: "topic", topic: "other", payload: `{}`},
		{name: "missing event", topic: DispatchTopic, payload: `{"dedupe_key":"key","payload":{}}`},
		{name: "unknown field", topic: DispatchTopic, payload: `{"domain_event_id":"event-1","dedupe_key":"key","payload":{},"extra":true}`},
		{name: "array payload", topic: DispatchTopic, payload: `{"domain_event_id":"event-1","dedupe_key":"key","payload":[]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dispatcher := &fakeDispatcher{}
			err := (Handler{Dispatcher: dispatcher}).Handle(context.Background(), sqlite.OutboxDelivery{Topic: testCase.topic, Payload: []byte(testCase.payload)})
			if err == nil || !worker.IsPermanent(err) || dispatcher.called {
				t.Fatalf("err=%v called=%v", err, dispatcher.called)
			}
		})
	}
}

func TestHandlerKeepsDispatchFailureRetryable(t *testing.T) {
	err := (Handler{Dispatcher: &fakeDispatcher{err: errors.New("database offline")}}).Handle(context.Background(), sqlite.OutboxDelivery{Topic: DispatchTopic, Payload: []byte(`{"domain_event_id":"event-1","dedupe_key":"key","payload":{}}`)})
	if err == nil || worker.IsPermanent(err) || strings.Contains(err.Error(), "offline") {
		t.Fatalf("err=%v", err)
	}
}

type fakeDispatcher struct {
	request notificationdispatch.Request
	called  bool
	err     error
}

func (d *fakeDispatcher) Dispatch(_ context.Context, request notificationdispatch.Request) (notificationdispatch.Result, error) {
	d.called, d.request = true, request
	return notificationdispatch.Result{}, d.err
}
