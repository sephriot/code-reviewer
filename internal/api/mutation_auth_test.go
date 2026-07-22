package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMutationAuthOnlyProtectsVersionedMutationPrefix(t *testing.T) {
	t.Parallel()
	guard := newMutationAuth(time.Now)
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

func TestSessionEndpointRequiresLoopbackAndSetsBrowserSafeCookie(t *testing.T) {
	t.Parallel()
	guard := newMutationAuth(time.Now)
	handler := guard.Wrap(http.NotFoundHandler())

	remote := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, want %d", remoteResponse.Code, http.StatusForbidden)
	}
	if len(remoteResponse.Result().Cookies()) != 0 {
		t.Fatal("remote session response set a cookie")
	}

	request := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("session status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if response.Body.Len() != 0 {
		t.Fatal("session response must not expose a body")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value == "" {
		t.Fatalf("cookie = %#v", cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure {
		t.Fatalf("cookie protections = httpOnly:%t sameSite:%d secure:%t", cookie.HttpOnly, cookie.SameSite, cookie.Secure)
	}
	if cookie.Path != "/api/v1/" || cookie.MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("cookie scope = path:%q maxAge:%d", cookie.Path, cookie.MaxAge)
	}
}

func TestSessionEndpointAllowsOnlyGet(t *testing.T) {
	t.Parallel()
	guard := newMutationAuth(time.Now)
	handler := guard.Wrap(http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodPost, sessionPath, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestMutationAuthRejectsNonLoopbackBeforeCheckingSession(t *testing.T) {
	t.Parallel()
	guard := newMutationAuth(time.Now)
	called := false
	handler := guard.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
	request.RemoteAddr = "198.51.100.7:443"
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "untrusted"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if called {
		t.Fatal("non-loopback request reached mutation handler")
	}
}

func TestMutationAuthRequiresExactCurrentSessionForLoopback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	guard := newMutationAuth(func() time.Time { return now })
	handler := guard.Wrap(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	session := issueSession(t, handler)

	for _, test := range []struct {
		name          string
		remoteAddress string
		cookie        *http.Cookie
		want          int
	}{
		{name: "missing", remoteAddress: "127.0.0.1:1234", want: http.StatusUnauthorized},
		{name: "wrong", remoteAddress: "[::1]:1234", cookie: &http.Cookie{Name: sessionCookieName, Value: "wrong"}, want: http.StatusUnauthorized},
		{name: "wrong name", remoteAddress: "127.0.0.1:1234", cookie: &http.Cookie{Name: "other", Value: session.Value}, want: http.StatusUnauthorized},
		{name: "malformed remote", remoteAddress: "127.0.0.1", cookie: session, want: http.StatusForbidden},
		{name: "ipv4 loopback", remoteAddress: "127.0.0.1:1234", cookie: session, want: http.StatusNoContent},
		{name: "ipv6 loopback", remoteAddress: "[::1]:1234", cookie: session, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
			request.RemoteAddr = test.remoteAddress
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestMutationAuthDoesNotAcceptBearerCredentials(t *testing.T) {
	t.Parallel()
	guard := newMutationAuth(time.Now)
	handler := guard.Wrap(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer ignored")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestMutationAuthRejectsExpiredAndAmbiguousSessions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	guard := newMutationAuth(func() time.Time { return now })
	handler := guard.Wrap(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	session := issueSession(t, handler)

	for _, test := range []struct {
		name    string
		cookies []*http.Cookie
		advance time.Duration
	}{
		{name: "expired", cookies: []*http.Cookie{session}, advance: sessionTTL + time.Nanosecond},
		{name: "ambiguous", cookies: []*http.Cookie{session, {Name: sessionCookieName, Value: "wrong"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.advance > 0 {
				now = now.Add(test.advance)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/example", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			for _, cookie := range test.cookies {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}
}

func issueSession(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("issue status = %d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("issued cookies = %d", len(cookies))
	}
	return cookies[0]
}
