package api

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	mutationPrefix    = "/api/v1/mutate/"
	sessionPath       = "/api/v1/session"
	sessionCookieName = "reviewd_session"
	sessionTTL        = 10 * time.Minute
)

// MutationAuth protects local control-plane mutations with short-lived,
// browser-managed sessions. Session values exist only in process memory and
// are never returned in a response body.
type MutationAuth struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	now      func() time.Time
}

// NewMutationAuth creates a per-process session issuer for local dashboard
// clients. The caller should wrap its complete API handler with Wrap.
func NewMutationAuth() (*MutationAuth, error) {
	return newMutationAuth(time.Now), nil
}

func newMutationAuth(now func() time.Time) *MutationAuth {
	if now == nil {
		now = time.Now
	}
	return &MutationAuth{sessions: make(map[string]time.Time), now: now}
}

// Wrap serves the loopback-only session bootstrap and protects the versioned
// mutation namespace. All other paths pass through unchanged.
func (auth *MutationAuth) Wrap(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == sessionPath {
			auth.session(response, request)
			return
		}
		if !strings.HasPrefix(request.URL.Path, mutationPrefix) {
			next.ServeHTTP(response, request)
			return
		}
		if !isLoopbackRemoteAddress(request.RemoteAddr) {
			http.Error(response, "forbidden", http.StatusForbidden)
			return
		}
		if !auth.matchesSession(request) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (auth *MutationAuth) session(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}
	token, err := newSessionToken()
	if err != nil {
		http.Error(response, "session unavailable", http.StatusServiceUnavailable)
		return
	}
	now := auth.now()
	auth.mu.Lock()
	auth.pruneExpired(now)
	auth.sessions[token] = now.Add(sessionTTL)
	auth.mu.Unlock()

	// Secure remains false because the dashboard is deliberately served on
	// loopback HTTP; browsers otherwise withhold this cookie from localhost.
	http.SetCookie(response, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/api/v1/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusNoContent)
}

func newSessionToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func (auth *MutationAuth) matchesSession(request *http.Request) bool {
	cookies := request.Cookies()
	var token string
	for _, cookie := range cookies {
		if cookie.Name != sessionCookieName {
			continue
		}
		if token != "" || cookie.Value == "" {
			return false
		}
		token = cookie.Value
	}
	if token == "" {
		return false
	}
	now := auth.now()
	auth.mu.Lock()
	defer auth.mu.Unlock()
	auth.pruneExpired(now)
	expiresAt, found := auth.sessions[token]
	return found && now.Before(expiresAt)
}

func (auth *MutationAuth) pruneExpired(now time.Time) {
	for token, expiresAt := range auth.sessions {
		if !now.Before(expiresAt) {
			delete(auth.sessions, token)
		}
	}
}

func isLoopbackRemoteAddress(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
