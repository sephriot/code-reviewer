package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
)

const mutationPrefix = "/api/v1/mutate/"

// MutationAuth protects future local control-plane mutation endpoints.
// Its bearer secret exists only in this process and is never serialized.
type MutationAuth struct {
	token string
}

// NewMutationAuth creates a new cryptographically random per-process bearer secret.
func NewMutationAuth() (MutationAuth, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return MutationAuth{}, err
	}
	return MutationAuth{token: base64.RawURLEncoding.EncodeToString(secret)}, nil
}

// Wrap requires a loopback RemoteAddr and bearer secret for the versioned
// mutation namespace. All other paths pass through unchanged.
func (auth MutationAuth) Wrap(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, mutationPrefix) {
			next.ServeHTTP(response, request)
			return
		}
		if !isLoopbackRemoteAddress(request.RemoteAddr) {
			http.Error(response, "forbidden", http.StatusForbidden)
			return
		}
		if !auth.matchesBearer(request.Header.Values("Authorization")) {
			response.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (auth MutationAuth) matchesBearer(headers []string) bool {
	if len(headers) != 1 || auth.token == "" {
		return false
	}
	header := headers[0]
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(auth.token)) == 1
}

func isLoopbackRemoteAddress(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
