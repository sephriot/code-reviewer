package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/notify"
)

func TestMuteAPIRoundTrip(t *testing.T) {
	n := notify.New(&config.Config{})
	s := New(&config.Config{}, nil, nil, nil)
	s.SetNotifier(n)

	get := httptest.NewRecorder()
	s.apiMuteNotifications(get, httptest.NewRequest(http.MethodGet, "/api/notifications/mute", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status %d", get.Code)
	}
	var body map[string]bool
	if err := json.Unmarshal(get.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET json: %v body %s", err, get.Body.String())
	}
	if body["muted"] {
		t.Fatalf("GET muted=true want false")
	}

	post := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/mute", strings.NewReader(`{"muted":true}`))
	req.Header.Set("Content-Type", "application/json")
	s.apiMuteNotifications(post, req)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status %d body %s", post.Code, post.Body.String())
	}
	if !n.Muted() {
		t.Fatal("notifier not muted after POST")
	}
	body = nil
	if err := json.Unmarshal(post.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST json: %v", err)
	}
	if !body["muted"] {
		t.Fatalf("POST muted=false want true")
	}
}

func TestMuteAPIBadJSON(t *testing.T) {
	n := notify.New(&config.Config{})
	s := New(&config.Config{}, nil, nil, nil)
	s.SetNotifier(n)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/mute", strings.NewReader(`{}`))
	s.apiMuteNotifications(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d want 400", rr.Code)
	}
}

func TestMuteAPIUnavailableWithoutNotifier(t *testing.T) {
	s := New(&config.Config{}, nil, nil, nil)
	rr := httptest.NewRecorder()
	s.apiMuteNotifications(rr, httptest.NewRequest(http.MethodGet, "/api/notifications/mute", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d want 503", rr.Code)
	}
}
