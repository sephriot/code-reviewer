package publishworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// EnabledJobKind is durable job type for one explicitly enabled publication.
const EnabledJobKind = "publication.enabled.v1"

// EnabledClaimer owns crash-safe pre-send claim persistence.
type EnabledClaimer interface {
	ClaimEnabledPublicationAttempt(context.Context, string, time.Time) (sqlite.ClaimEnabledPublicationAttemptResult, error)
}

// EnabledRecorder owns immutable enabled-attempt outcomes.
type EnabledRecorder interface {
	RecordEnabledPublicationAttempt(context.Context, sqlite.RecordEnabledPublicationAttemptInput) (sqlite.RecordEnabledPublicationAttemptResult, error)
}

// EnabledDiffReader is intentionally GET-only.
type EnabledDiffReader interface {
	GetPullRequestDiff(context.Context, string, string, int, string) (github.PullRequestDiffResult, error)
}

// EnabledPublisher is the sole outbound GitHub review boundary.
type EnabledPublisher interface {
	SubmitReview(context.Context, github.ReviewSubmission) (github.SubmittedReview, error)
}

// EnabledHandler dispatches one claimed effect. Any failure after claim becomes
// uncertain and completes the job; it never blindly retries an external write.
type EnabledHandler struct {
	Claimer   EnabledClaimer
	Recorder  EnabledRecorder
	Reader    EnabledDiffReader
	Publisher EnabledPublisher
	Now       func() time.Time
}

// Handle implements worker.Handler.
func (h EnabledHandler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != EnabledJobKind {
		return worker.Permanent(errors.New("unexpected enabled publication job kind"))
	}
	effectID, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed enabled publication job payload: %w", err))
	}
	if h.Claimer == nil || h.Recorder == nil || h.Reader == nil || h.Publisher == nil {
		return worker.Permanent(errors.New("enabled publication handler dependencies are required"))
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	claim, err := h.Claimer.ClaimEnabledPublicationAttempt(ctx, effectID, now)
	if err != nil {
		if errors.Is(err, sqlite.ErrPublicationEffectNotFound) || errors.Is(err, sqlite.ErrPublicationEffectNotCurrent) || errors.Is(err, sqlite.ErrPublicationEffectNotDispatchable) {
			return worker.Permanent(errors.New("enabled publication effect is not dispatchable"))
		}
		return errors.New("claim enabled publication effect failed")
	}
	if claim.Effect.ID != effectID || claim.Effect.PublicationMode != sqlite.PublicationModeEnabled {
		return worker.Permanent(errors.New("enabled publication effect is not dispatchable"))
	}
	if !claim.Created {
		return h.recordUncertain(ctx, effectID, now, "interrupted_claim", "previous publication request may have been sent")
	}

	diff, err := h.Reader.GetPullRequestDiff(ctx, claim.Effect.Owner, claim.Effect.Repository, claim.Effect.PullRequestNumber, "")
	if err != nil || diff.NotModified || len(diff.Bytes) == 0 {
		return h.recordUncertain(ctx, effectID, now, "diff_read", "could not verify current pull request diff")
	}
	parsed, err := github.ParseUnifiedDiff(diff.Bytes)
	if err != nil {
		return h.recordUncertain(ctx, effectID, now, "diff_parse", "could not verify current pull request diff")
	}
	lines, err := github.RightSideDiffLines(parsed)
	if err != nil {
		return h.recordUncertain(ctx, effectID, now, "anchor_validation", "could not validate inline review anchors")
	}
	submission, err := buildEnabledSubmission(claim.Effect, lines)
	if err != nil {
		return h.recordUncertain(ctx, effectID, now, "payload_validation", "could not build publication payload")
	}
	result, err := h.Publisher.SubmitReview(ctx, submission)
	if err != nil {
		return h.recordUncertain(ctx, effectID, now, "github_request", "GitHub publication result is uncertain")
	}
	response, _ := json.Marshal(map[string]any{"review_id": result.ID, "node_id": result.NodeID, "state": result.State})
	if _, err := h.Recorder.RecordEnabledPublicationAttempt(ctx, sqlite.RecordEnabledPublicationAttemptInput{EffectID: effectID, Outcome: sqlite.PublicationAttemptSucceeded, ResponseJSON: response, GitHubArtifactID: strconv.FormatInt(result.ID, 10), CompletedAt: now}); err != nil {
		return errors.New("record enabled publication success failed")
	}
	return nil
}

func (h EnabledHandler) recordUncertain(ctx context.Context, effectID string, now time.Time, class, message string) error {
	_, err := h.Recorder.RecordEnabledPublicationAttempt(ctx, sqlite.RecordEnabledPublicationAttemptInput{EffectID: effectID, Outcome: sqlite.PublicationAttemptUncertain, ResponseJSON: []byte(`{}`), ErrorClass: class, ErrorMessage: message, CompletedAt: now})
	if err != nil {
		return errors.New("record enabled publication uncertainty failed")
	}
	return nil
}

func buildEnabledSubmission(effect sqlite.PublicationEffectTarget, validLines map[string]map[int]struct{}) (github.ReviewSubmission, error) {
	var payload struct {
		Body     string `json:"body"`
		Comments []struct {
			Path    string `json:"path"`
			EndLine int    `json:"end_line"`
			Side    string `json:"side"`
			Body    string `json:"body"`
		} `json:"inline_comments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(effect.PayloadJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return github.ReviewSubmission{}, errors.New("enabled publication payload is invalid")
	}
	if err := requireEOF(decoder); err != nil {
		return github.ReviewSubmission{}, errors.New("enabled publication payload is invalid")
	}
	event, err := reviewEvent(effect.EffectType)
	if err != nil {
		return github.ReviewSubmission{}, err
	}
	comments := make([]github.ReviewComment, 0, len(payload.Comments))
	fallback := make([]string, 0)
	for _, comment := range payload.Comments {
		if comment.Side == "RIGHT" && github.IsValidRightSideAnchor(validLines, comment.Path, comment.EndLine) {
			comments = append(comments, github.ReviewComment{Path: comment.Path, Line: comment.EndLine, Side: comment.Side, Body: comment.Body})
			continue
		}
		fallback = append(fallback, fmt.Sprintf("- `%s:%d`: %s", comment.Path, comment.EndLine, comment.Body))
	}
	if len(fallback) > 0 {
		payload.Body = strings.TrimSpace(payload.Body + "\n\nUnanchored findings:\n" + strings.Join(fallback, "\n"))
	}
	return github.ReviewSubmission{Owner: effect.Owner, Repository: effect.Repository, Number: effect.PullRequestNumber, Event: event, Body: payload.Body, Comments: comments}, nil
}

func reviewEvent(effectType string) (github.ReviewEvent, error) {
	switch effectType {
	case "review_approval":
		return github.ReviewEventApprove, nil
	case "review_comment":
		return github.ReviewEventComment, nil
	case "review_changes":
		return github.ReviewEventRequestChanges, nil
	default:
		return "", errors.New("enabled publication effect type is invalid")
	}
}

var _ worker.Handler = EnabledHandler{}
