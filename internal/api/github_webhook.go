package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const (
	githubWebhookPath            = "/api/v1/webhooks/github"
	githubSignatureHeader        = "X-Hub-Signature-256"
	githubDeliveryHeader         = "X-GitHub-Delivery"
	githubEventHeader            = "X-GitHub-Event"
	maxGitHubWebhookPayloadBytes = 1 << 20
)

var githubWebhookDeliveryIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// GitHubWebhookEvent identifies an event type accepted by this ingress
// foundation. Each accepted value has a bounded metadata parser.
type GitHubWebhookEvent string

const (
	GitHubWebhookEventPing              GitHubWebhookEvent = "ping"
	GitHubWebhookEventPullRequest       GitHubWebhookEvent = "pull_request"
	GitHubWebhookEventPullRequestReview GitHubWebhookEvent = "pull_request_review"
)

// GitHubWebhookStore retains verified metadata and exposes no provider capability.
type GitHubWebhookStore interface {
	RecordGitHubWebhookDelivery(context.Context, sqlite.RecordGitHubWebhookDeliveryInput) (sqlite.RecordGitHubWebhookDeliveryResult, error)
}

// GitHubWebhookScheduler accepts one durable local follow-up after verified
// metadata is retained. It must not call GitHub synchronously.
type GitHubWebhookScheduler interface {
	Schedule(context.Context) error
}

// GitHubWebhookOptions identifies the in-memory signing secret and metadata
// store. Secret is loaded from a named local reference by app wiring; it is
// never put in configuration, persistence, logs, or responses.
type GitHubWebhookOptions struct {
	Enabled   bool
	Secret    []byte
	Store     GitHubWebhookStore
	Scheduler GitHubWebhookScheduler
	Now       func() time.Time
}

// NewGitHubWebhookHandler serves one loopback-only, signed ingress endpoint.
// It retains verified delivery metadata only; it never calls GitHub or starts
// review, reconciliation, publication, event, outbox, or job work.
func NewGitHubWebhookHandler(options GitHubWebhookOptions) http.Handler {
	return githubWebhookHandler{enabled: options.Enabled, secret: append([]byte(nil), options.Secret...), store: options.Store, scheduler: options.Scheduler, now: options.Now}
}

func registerGitHubWebhookRoute(mux *http.ServeMux, options GitHubWebhookOptions) {
	if mux == nil || !options.Enabled {
		return
	}
	mux.Handle("POST "+githubWebhookPath, NewGitHubWebhookHandler(options))
}

type githubWebhookHandler struct {
	enabled   bool
	secret    []byte
	store     GitHubWebhookStore
	scheduler GitHubWebhookScheduler
	now       func() time.Time
}

func (h githubWebhookHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if !h.enabled || h.store == nil || len(h.secret) == 0 {
		writeControlError(response, http.StatusServiceUnavailable, "webhook_unavailable", "github webhook ingress is unavailable", true)
		return
	}
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeControlError(response, http.StatusMethodNotAllowed, "method_not_allowed", "github webhook ingress accepts POST only", false)
		return
	}
	if request.URL.RawQuery != "" {
		writeControlError(response, http.StatusBadRequest, "invalid_webhook", "github webhook query parameters are unsupported", false)
		return
	}
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "github webhook ingress is available only on loopback", false)
		return
	}
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeControlError(response, http.StatusUnsupportedMediaType, "unsupported_media_type", "github webhook content type must be application/json", false)
		return
	}
	deliveryID, eventType, signature, err := parseGitHubWebhookHeaders(request.Header)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_webhook", "github webhook headers are invalid", false)
		return
	}
	if request.ContentLength > maxGitHubWebhookPayloadBytes {
		writeControlError(response, http.StatusRequestEntityTooLarge, "payload_too_large", "github webhook payload is too large", false)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxGitHubWebhookPayloadBytes))
	if err != nil {
		writeControlError(response, http.StatusRequestEntityTooLarge, "payload_too_large", "github webhook payload is too large", false)
		return
	}
	if len(body) == 0 {
		writeControlError(response, http.StatusBadRequest, "invalid_webhook", "github webhook payload is invalid", false)
		return
	}
	if !verifyGitHubWebhookSignature(h.secret, body, signature) {
		writeControlError(response, http.StatusUnauthorized, "invalid_signature", "github webhook signature is invalid", false)
		return
	}
	metadata, err := parseGitHubWebhookPayload(eventType, body)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_webhook", "github webhook payload is invalid", false)
		return
	}
	digest := sha256.Sum256(body)
	_, err = h.store.RecordGitHubWebhookDelivery(request.Context(), sqlite.RecordGitHubWebhookDeliveryInput{
		DeliveryID: deliveryID, EventType: string(eventType), Action: metadata.Action,
		RepositoryGitHubID: metadata.RepositoryGitHubID, PullRequestNumber: metadata.PullRequestNumber,
		PayloadSHA256: hex.EncodeToString(digest[:]), PayloadBytes: len(body), ReceivedAt: h.currentTime(),
	})
	if err != nil {
		if errors.Is(err, sqlite.ErrGitHubWebhookDeliveryConflict) {
			writeControlError(response, http.StatusConflict, "webhook_delivery_conflict", "github webhook delivery conflicts with retained metadata", false)
			return
		}
		writeControlError(response, http.StatusServiceUnavailable, "webhook_unavailable", "github webhook ingress is unavailable", true)
		return
	}
	if h.scheduler != nil {
		if err := h.scheduler.Schedule(request.Context()); err != nil {
			writeControlError(response, http.StatusServiceUnavailable, "webhook_unavailable", "github webhook ingress is unavailable", true)
			return
		}
	}
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusAccepted)
}

func (h githubWebhookHandler) currentTime() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func parseGitHubWebhookHeaders(header http.Header) (string, GitHubWebhookEvent, string, error) {
	deliveryID, ok := singleHeaderValue(header, githubDeliveryHeader)
	if !ok || !githubWebhookDeliveryIDPattern.MatchString(deliveryID) {
		return "", "", "", errors.New("github delivery ID is invalid")
	}
	eventValue, ok := singleHeaderValue(header, githubEventHeader)
	if !ok || !validGitHubWebhookEvent(GitHubWebhookEvent(eventValue)) {
		return "", "", "", errors.New("github event type is invalid")
	}
	signature, ok := singleHeaderValue(header, githubSignatureHeader)
	if !ok || !validGitHubWebhookSignatureHeader(signature) {
		return "", "", "", errors.New("github signature header is invalid")
	}
	return deliveryID, GitHubWebhookEvent(eventValue), signature, nil
}

func singleHeaderValue(header http.Header, key string) (string, bool) {
	values := header.Values(key)
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func validGitHubWebhookEvent(event GitHubWebhookEvent) bool {
	switch event {
	case GitHubWebhookEventPing, GitHubWebhookEventPullRequest, GitHubWebhookEventPullRequestReview:
		return true
	default:
		return false
	}
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func validGitHubWebhookSignatureHeader(value string) bool {
	if !strings.HasPrefix(value, "sha256=") || len(value) != len("sha256=")+sha256.Size*2 {
		return false
	}
	for _, character := range value[len("sha256="):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func verifyGitHubWebhookSignature(secret, body []byte, signature string) bool {
	if len(secret) == 0 || !validGitHubWebhookSignatureHeader(signature) {
		return false
	}
	supplied, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), supplied)
}

type githubWebhookPayloadMetadata struct {
	Action             string
	RepositoryGitHubID int64
	PullRequestNumber  int
}

func parseGitHubWebhookPayload(eventType GitHubWebhookEvent, body []byte) (githubWebhookPayloadMetadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil || document == nil {
		return githubWebhookPayloadMetadata{}, errors.New("github webhook payload is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return githubWebhookPayloadMetadata{}, errors.New("github webhook payload has trailing data")
	}
	if len(document) > 128 {
		return githubWebhookPayloadMetadata{}, errors.New("github webhook payload has too many fields")
	}
	repositoryID, err := parseGitHubWebhookRepositoryID(document["repository"])
	if err != nil {
		return githubWebhookPayloadMetadata{}, err
	}
	metadata := githubWebhookPayloadMetadata{RepositoryGitHubID: repositoryID}
	if eventType == GitHubWebhookEventPing {
		metadata.Action = "ping"
		return metadata, nil
	}
	action, err := parseGitHubWebhookAction(document["action"])
	if err != nil {
		return githubWebhookPayloadMetadata{}, err
	}
	number, err := parseGitHubWebhookPullRequestNumber(document["number"])
	if err != nil {
		return githubWebhookPayloadMetadata{}, err
	}
	metadata.Action, metadata.PullRequestNumber = action, number
	return metadata, nil
}

func parseGitHubWebhookRepositoryID(raw json.RawMessage) (int64, error) {
	var repository struct {
		ID json.Number `json:"id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &repository) != nil {
		return 0, errors.New("github webhook repository is invalid")
	}
	id, err := repository.ID.Int64()
	if err != nil || id <= 0 {
		return 0, errors.New("github webhook repository ID is invalid")
	}
	return id, nil
}

func parseGitHubWebhookAction(raw json.RawMessage) (string, error) {
	var action string
	if len(raw) == 0 || json.Unmarshal(raw, &action) != nil || !validGitHubWebhookAction(action) {
		return "", errors.New("github webhook action is invalid")
	}
	return action, nil
}

func parseGitHubWebhookPullRequestNumber(raw json.RawMessage) (int, error) {
	var number json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &number) != nil {
		return 0, errors.New("github webhook pull request number is invalid")
	}
	value, err := number.Int64()
	if err != nil || value < 1 || value > int64(^uint(0)>>1) {
		return 0, errors.New("github webhook pull request number is invalid")
	}
	return int(value), nil
}

func validGitHubWebhookAction(action string) bool {
	if len(action) < 1 || len(action) > 64 {
		return false
	}
	for _, value := range []byte(action) {
		if value < '!' || value > '~' {
			return false
		}
	}
	return true
}
