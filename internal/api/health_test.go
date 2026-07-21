package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthLive(t *testing.T) {
	t.Parallel()

	handler := NewHealthHandler(Readiness{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", response.Header().Get("Content-Type"))
	}
	assertJSONStatus(t, response, "live")
}

func TestHealthReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		readiness  Readiness
		wantCode   int
		wantStatus string
		wantDB     string
		wantSchema string
	}{
		{
			name: "ready",
			readiness: Readiness{
				Ping: func(context.Context) error { return nil },
				SchemaStatus: func(context.Context) (SchemaStatus, error) {
					return SchemaStatus{Current: 3, Latest: 3, Pending: 0}, nil
				},
			},
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			wantDB:     "ok",
			wantSchema: "ok",
		},
		{
			name: "database unavailable",
			readiness: Readiness{
				Ping: func(context.Context) error { return errors.New("database offline") },
				SchemaStatus: func(context.Context) (SchemaStatus, error) {
					t.Fatal("SchemaStatus called after failed Ping")
					return SchemaStatus{}, nil
				},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantDB:     "failed",
			wantSchema: "unknown",
		},
		{
			name: "schema check fails",
			readiness: Readiness{
				Ping: func(context.Context) error { return nil },
				SchemaStatus: func(context.Context) (SchemaStatus, error) {
					return SchemaStatus{}, errors.New("schema unreadable")
				},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantDB:     "ok",
			wantSchema: "failed",
		},
		{
			name: "migrations pending",
			readiness: Readiness{
				Ping: func(context.Context) error { return nil },
				SchemaStatus: func(context.Context) (SchemaStatus, error) {
					return SchemaStatus{Current: 2, Latest: 3, Pending: 1}, nil
				},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantDB:     "ok",
			wantSchema: "pending",
		},
		{
			name: "inconsistent schema status",
			readiness: Readiness{
				Ping: func(context.Context) error { return nil },
				SchemaStatus: func(context.Context) (SchemaStatus, error) {
					return SchemaStatus{Current: 3, Latest: 2, Pending: 0}, nil
				},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantDB:     "ok",
			wantSchema: "failed",
		},
		{
			name:       "checks not configured",
			readiness:  Readiness{},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantDB:     "unknown",
			wantSchema: "unknown",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := NewHealthHandler(test.readiness)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != test.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantCode, response.Body.String())
			}
			var body healthResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Checks == nil {
				t.Fatalf("response has no readiness checks: %+v", body)
			}
			if body.Status != test.wantStatus || body.Checks.Database != test.wantDB || body.Checks.Schema != test.wantSchema {
				t.Fatalf("response = %+v", body)
			}
		})
	}
}

func TestHealthRejectsUnsupportedMethod(t *testing.T) {
	t.Parallel()

	handler := NewHealthHandler(Readiness{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/health/live", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if response.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", response.Header().Get("Allow"))
	}
}

func TestHealthUnknownRoute(t *testing.T) {
	t.Parallel()

	handler := NewHealthHandler(Readiness{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/other", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func assertJSONStatus(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body healthResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != want {
		t.Errorf("status body = %q, want %q", body.Status, want)
	}
}
