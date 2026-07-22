package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

type fakeGitHubWebhookStore struct {
	input  sqlite.RecordGitHubWebhookDeliveryInput
	err    error
	result sqlite.RecordGitHubWebhookDeliveryResult
}

func (f *fakeGitHubWebhookStore) RecordGitHubWebhookDelivery(_ context.Context, input sqlite.RecordGitHubWebhookDeliveryInput) (sqlite.RecordGitHubWebhookDeliveryResult, error) {
	f.input = input
	if f.err != nil {
		return sqlite.RecordGitHubWebhookDeliveryResult{}, f.err
	}
	return f.result, nil
}

func TestGitHubWebhookAcceptsVerifiedBoundedPullRequestMetadata(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret")
	store := &fakeGitHubWebhookStore{result: sqlite.RecordGitHubWebhookDeliveryResult{Created: true}}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	handler := NewGitHubWebhookHandler(GitHubWebhookOptions{Enabled: true, Secret: secret, Store: store, Now: func() time.Time { return now }})
	body := `{"action":"opened","number":42,"repository":{"id":12345},"ignored":{"nested":true}}`
	request := signedGitHubWebhookRequest(t, secret, body)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.input.DeliveryID != "123e4567-e89b-12d3-a456-426614174000" || store.input.EventType != string(GitHubWebhookEventPullRequest) ||
		store.input.Action != "opened" || store.input.RepositoryGitHubID != 12345 || store.input.PullRequestNumber != 42 ||
		store.input.PayloadBytes != len(body) || !store.input.ReceivedAt.Equal(now) {
		t.Fatalf("store input=%+v", store.input)
	}
	wantHash := sha256.Sum256([]byte(body))
	if store.input.PayloadSHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("payload hash=%q", store.input.PayloadSHA256)
	}
}

func TestGitHubWebhookRejectsInvalidOriginSignatureHeadersAndPayloadWithoutRecording(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret")
	body := `{"action":"opened","number":42,"repository":{"id":12345}}`
	for _, testCase := range []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "remote", mutate: func(r *http.Request) { r.RemoteAddr = "198.51.100.7:443" }, want: http.StatusForbidden},
		{name: "signature", mutate: func(r *http.Request) { r.Header.Set(githubSignatureHeader, "sha256="+strings.Repeat("0", 64)) }, want: http.StatusUnauthorized},
		{name: "missing delivery", mutate: func(r *http.Request) { r.Header.Del(githubDeliveryHeader) }, want: http.StatusBadRequest},
		{name: "unsupported event", mutate: func(r *http.Request) { r.Header.Set(githubEventHeader, "issues") }, want: http.StatusBadRequest},
		{name: "bad content type", mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "query", mutate: func(r *http.Request) { r.URL.RawQuery = "ignored=true" }, want: http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeGitHubWebhookStore{}
			handler := NewGitHubWebhookHandler(GitHubWebhookOptions{Enabled: true, Secret: secret, Store: store})
			request := signedGitHubWebhookRequest(t, secret, body)
			testCase.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, testCase.want, response.Body.String())
			}
			if store.input.DeliveryID != "" {
				t.Fatalf("unsafe webhook was recorded: %+v", store.input)
			}
		})
	}
}

func TestGitHubWebhookRejectsSignedInvalidPayloadWithoutRecording(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret")
	store := &fakeGitHubWebhookStore{}
	handler := NewGitHubWebhookHandler(GitHubWebhookOptions{Enabled: true, Secret: secret, Store: store})
	body := `{"action":"opened","number":0,"repository":{"id":12345}}`
	request := signedGitHubWebhookRequest(t, secret, body)
	request.Body = io.NopCloser(strings.NewReader(body))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || store.input.DeliveryID != "" {
		t.Fatalf("status=%d recorded=%+v", response.Code, store.input)
	}
}

func TestGitHubWebhookRejectsOversizeBodyBeforeStore(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret")
	store := &fakeGitHubWebhookStore{}
	body := `{"action":"opened","number":42,"repository":{"id":12345}}` + strings.Repeat(" ", maxGitHubWebhookPayloadBytes)
	handler := NewGitHubWebhookHandler(GitHubWebhookOptions{Enabled: true, Secret: secret, Store: store})
	request := signedGitHubWebhookRequest(t, secret, body)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge || store.input.DeliveryID != "" {
		t.Fatalf("status=%d recorded=%+v", response.Code, store.input)
	}
}

func TestControlRegistersEnabledGitHubWebhookRouteOnly(t *testing.T) {
	t.Parallel()
	secret := []byte("webhook-test-secret")
	store := &fakeGitHubWebhookStore{result: sqlite.RecordGitHubWebhookDeliveryResult{Created: true}}
	control := NewControlHandler(Readiness{}, ControlOptions{GitHubWebhooks: GitHubWebhookOptions{Enabled: true, Secret: secret, Store: store}})
	request := signedGitHubWebhookRequest(t, secret, `{"action":"opened","number":42,"repository":{"id":12345}}`)
	response := httptest.NewRecorder()
	control.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.input.DeliveryID == "" {
		t.Fatalf("enabled route status=%d input=%+v", response.Code, store.input)
	}

	disabled := NewControlHandler(Readiness{}, ControlOptions{})
	response = httptest.NewRecorder()
	disabled.ServeHTTP(response, signedGitHubWebhookRequest(t, secret, `{"action":"opened","number":42,"repository":{"id":12345}}`))
	if response.Code == http.StatusAccepted {
		t.Fatalf("disabled route accepted webhook")
	}
}

func signedGitHubWebhookRequest(t *testing.T, secret []byte, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, githubWebhookPath, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(githubDeliveryHeader, "123e4567-e89b-12d3-a456-426614174000")
	request.Header.Set(githubEventHeader, string(GitHubWebhookEventPullRequest))
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(body))
	request.Header.Set(githubSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}
