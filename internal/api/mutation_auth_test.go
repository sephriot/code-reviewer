package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMutationAuthOnlyProtectsVersionedMutationPrefix(t *testing.T) {
	t.Parallel()
	guard := MutationAuth{token: "test-secret"}
	next := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})
	handler := guard.Wrap(next)

	for _, path := range []string{
		"/health/live",
		"/api/v1/inbox",
		"/api/v1/mutate",
		"/api/mutate/example",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "198.51.100.7:443"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("%s status = %d, want %d", path, response.Code, http.StatusNoContent)
		}
	}
}

func TestMutationAuthRejectsNonLoopbackBeforeCheckingBearer(t *testing.T) {
	t.Parallel()
	guard := MutationAuth{token: "test-secret"}
	called := false
	handler := guard.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
	request.RemoteAddr = "198.51.100.7:443"
	request.Header.Set("Authorization", "Bearer test-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if called {
		t.Fatal("non-loopback request reached mutation handler")
	}
}

func TestMutationAuthRequiresExactBearerForLoopback(t *testing.T) {
	t.Parallel()
	guard := MutationAuth{token: "test-secret"}
	cases := []struct {
		name          string
		remoteAddress string
		authorization string
		want          int
	}{
		{name: "missing", remoteAddress: "127.0.0.1:1234", want: http.StatusUnauthorized},
		{name: "wrong scheme", remoteAddress: "127.0.0.1:1234", authorization: "Basic test-secret", want: http.StatusUnauthorized},
		{name: "wrong token", remoteAddress: "[::1]:1234", authorization: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "malformed remote", remoteAddress: "127.0.0.1", authorization: "Bearer test-secret", want: http.StatusForbidden},
		{name: "ipv4 loopback", remoteAddress: "127.0.0.1:1234", authorization: "Bearer test-secret", want: http.StatusNoContent},
		{name: "ipv6 loopback", remoteAddress: "[::1]:1234", authorization: "Bearer test-secret", want: http.StatusNoContent},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := guard.Wrap(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				called = true
				response.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
			request.RemoteAddr = test.remoteAddress
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if called != (test.want == http.StatusNoContent) {
				t.Fatalf("next called = %t, want %t", called, test.want == http.StatusNoContent)
			}
		})
	}
}

func TestMutationAuthRejectsMultipleAuthorizationValues(t *testing.T) {
	t.Parallel()
	guard := MutationAuth{token: "test-secret"}
	called := false
	handler := guard.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Add("Authorization", "Bearer test-secret")
	request.Header.Add("Authorization", "Bearer wrong")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("ambiguous authorization reached mutation handler")
	}
}

func TestNewMutationAuthCreatesUsableDistinctSecrets(t *testing.T) {
	t.Parallel()
	first, err := NewMutationAuth()
	if err != nil {
		t.Fatalf("new first mutation auth: %v", err)
	}
	second, err := NewMutationAuth()
	if err != nil {
		t.Fatalf("new second mutation auth: %v", err)
	}
	if first.token == "" || second.token == "" {
		t.Fatal("generated mutation secret is empty")
	}
	if first.token == second.token {
		t.Fatal("generated mutation secrets are equal")
	}
}
