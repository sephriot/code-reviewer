package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublisherSubmitsBoundedReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/pulls/42/reviews" {
			t.Errorf("request=%s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers=%v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["event"] != "REQUEST_CHANGES" || body["body"] != "Needs a nil guard" {
			t.Errorf("body=%#v", body)
		}
		_, _ = response.Write([]byte(`{"id":77,"node_id":"PRR_1","state":"CHANGES_REQUESTED"}`))
	}))
	defer server.Close()
	publisher, err := NewPublisher(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.SubmitReview(context.Background(), ReviewSubmission{Owner: "acme", Repository: "widgets", Number: 42, Event: ReviewEventRequestChanges, Body: "Needs a nil guard", Comments: []ReviewComment{{Path: "internal/item.go", Line: 7, Side: "RIGHT", Body: "Guard nil"}}})
	if err != nil || result.ID != 77 || result.State != "CHANGES_REQUESTED" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPublisherRejectsUnsafeSubmissionBeforeNetwork(t *testing.T) {
	publisher, err := NewPublisher("https://api.github.com", "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, submission := range []ReviewSubmission{{}, {Owner: "acme", Repository: "widgets", Number: 1, Event: "MERGE"}, {Owner: "acme", Repository: "widgets", Number: 1, Event: ReviewEventComment, Comments: []ReviewComment{{Path: "../secret", Line: 1, Side: "RIGHT", Body: "no"}}}} {
		if _, err := publisher.SubmitReview(context.Background(), submission); err == nil {
			t.Fatalf("accepted=%+v", submission)
		}
	}
}

func TestPublisherSanitizesProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusForbidden)
		_, _ = response.Write([]byte(`{"message":"token test-token refused"}`))
	}))
	defer server.Close()
	publisher, _ := NewPublisher(server.URL, "test-token", server.Client())
	_, err := publisher.SubmitReview(context.Background(), ReviewSubmission{Owner: "acme", Repository: "widgets", Number: 1, Event: ReviewEventComment})
	if err == nil || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error=%v", err)
	}
}
