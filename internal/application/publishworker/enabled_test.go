package publishworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestBuildEnabledSubmissionKeepsValidAnchorsAndFallsBackInvalid(t *testing.T) {
	effect := sqlite.PublicationEffectTarget{
		Owner: "acme", Repository: "widgets", PullRequestNumber: 42, EffectType: "review_changes",
		PayloadJSON: []byte(`{"body":"Needs fixes","inline_comments":[{"path":"item.go","end_line":7,"side":"RIGHT","body":"valid"},{"path":"item.go","end_line":99,"side":"RIGHT","body":"invalid"}]}`),
	}
	submission, err := buildEnabledSubmission(effect, map[string]map[int]struct{}{"item.go": {7: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if submission.Event != "REQUEST_CHANGES" || len(submission.Comments) != 1 || submission.Comments[0].Line != 7 || submission.Body != "Needs fixes\n\nUnanchored findings:\n- `item.go:99`: invalid" {
		t.Fatalf("submission = %+v", submission)
	}
}

func TestBuildEnabledSubmissionRejectsUnknownPayloadAndEffect(t *testing.T) {
	for _, effect := range []sqlite.PublicationEffectTarget{
		{EffectType: "review_comment", PayloadJSON: []byte(`{"body":"x","unknown":true}`)},
		{EffectType: "marker_create", PayloadJSON: []byte(`{"body":"x","inline_comments":[]}`)},
	} {
		if _, err := buildEnabledSubmission(effect, nil); err == nil {
			t.Fatalf("accepted effect = %+v", effect)
		}
	}
}

func TestEnabledHandlerClaimsValidatesAndRecordsSuccess(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	effect := enabledEffect(`{"body":"Needs fixes","inline_comments":[{"path":"item.go","end_line":2,"side":"RIGHT","body":"fix this"}]}`)
	claimer := &enabledClaimer{result: sqlite.ClaimEnabledPublicationAttemptResult{Effect: effect, Created: true}}
	reader := &enabledReader{result: github.PullRequestDiffResult{Bytes: []byte("diff --git a/item.go b/item.go\n--- a/item.go\n+++ b/item.go\n@@ -1 +1,2 @@\n old\n+new\n")}}
	publisher := &enabledPublisher{result: github.SubmittedReview{ID: 77, NodeID: "node-77", State: "CHANGES_REQUESTED"}}
	recorder := &enabledRecorder{}
	handler := EnabledHandler{Claimer: claimer, Reader: reader, Publisher: publisher, Recorder: recorder, Now: func() time.Time { return now }}

	if err := handler.Handle(context.Background(), sqlite.Job{Kind: EnabledJobKind, Payload: []byte(`{"effect_id":"effect-1"}`)}); err != nil {
		t.Fatal(err)
	}
	if claimer.effectID != "effect-1" || reader.owner != "acme" || reader.repository != "widgets" || reader.number != 42 || publisher.submission.Event != github.ReviewEventRequestChanges || len(publisher.submission.Comments) != 1 {
		t.Fatalf("claimer=%+v reader=%+v publisher=%+v", claimer, reader, publisher)
	}
	if recorder.input.EffectID != "effect-1" || recorder.input.Outcome != sqlite.PublicationAttemptSucceeded || recorder.input.GitHubArtifactID != "77" || !recorder.input.CompletedAt.Equal(now) {
		t.Fatalf("recorder=%+v", recorder)
	}
}

func TestEnabledHandlerMarksPostClaimFailuresUncertainWithoutRetry(t *testing.T) {
	tests := []struct {
		name      string
		claimer   sqlite.ClaimEnabledPublicationAttemptResult
		reader    *enabledReader
		publisher *enabledPublisher
		wantClass string
	}{
		{name: "recovered claim", claimer: sqlite.ClaimEnabledPublicationAttemptResult{Effect: enabledEffect(`{"body":"x","inline_comments":[]}`)}, wantClass: "interrupted_claim"},
		{name: "diff read", claimer: sqlite.ClaimEnabledPublicationAttemptResult{Effect: enabledEffect(`{"body":"x","inline_comments":[]}`), Created: true}, reader: &enabledReader{err: errors.New("offline")}, publisher: &enabledPublisher{}, wantClass: "diff_read"},
		{name: "publish", claimer: sqlite.ClaimEnabledPublicationAttemptResult{Effect: enabledEffect(`{"body":"x","inline_comments":[]}`), Created: true}, reader: &enabledReader{result: github.PullRequestDiffResult{Bytes: []byte("diff --git a/item.go b/item.go\n--- a/item.go\n+++ b/item.go\n@@ -0,0 +1 @@\n+x\n")}}, publisher: &enabledPublisher{err: errors.New("connection reset")}, wantClass: "github_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claimer := &enabledClaimer{result: test.claimer}
			reader := test.reader
			if reader == nil {
				reader = &enabledReader{}
			}
			publisher := test.publisher
			if publisher == nil {
				publisher = &enabledPublisher{}
			}
			recorder := &enabledRecorder{}
			err := (EnabledHandler{Claimer: claimer, Reader: reader, Publisher: publisher, Recorder: recorder}).Handle(context.Background(), sqlite.Job{Kind: EnabledJobKind, Payload: []byte(`{"effect_id":"effect-1"}`)})
			if err != nil || recorder.input.Outcome != sqlite.PublicationAttemptUncertain || recorder.input.ErrorClass != test.wantClass {
				t.Fatalf("err=%v recorder=%+v", err, recorder)
			}
			if test.name == "recovered claim" && (reader.called || publisher.called) {
				t.Fatalf("recovered claim performed external work: reader=%+v publisher=%+v", reader, publisher)
			}
		})
	}
}

func TestEnabledHandlerRetriesOnlyPreClaimFailure(t *testing.T) {
	claimer := &enabledClaimer{err: errors.New("database unavailable")}
	err := (EnabledHandler{Claimer: claimer, Reader: &enabledReader{}, Publisher: &enabledPublisher{}, Recorder: &enabledRecorder{}}).Handle(context.Background(), sqlite.Job{Kind: EnabledJobKind, Payload: []byte(`{"effect_id":"effect-1"}`)})
	if err == nil || worker.IsPermanent(err) || errors.Is(err, claimer.err) {
		t.Fatalf("error = %v", err)
	}
}

func enabledEffect(payload string) sqlite.PublicationEffectTarget {
	return sqlite.PublicationEffectTarget{ID: "effect-1", Owner: "acme", Repository: "widgets", PullRequestNumber: 42, PublicationMode: sqlite.PublicationModeEnabled, EffectType: "review_changes", PayloadJSON: []byte(payload)}
}

type enabledClaimer struct {
	effectID string
	result   sqlite.ClaimEnabledPublicationAttemptResult
	err      error
}

func (f *enabledClaimer) ClaimEnabledPublicationAttempt(_ context.Context, effectID string, _ time.Time) (sqlite.ClaimEnabledPublicationAttemptResult, error) {
	f.effectID = effectID
	return f.result, f.err
}

type enabledReader struct {
	owner, repository string
	number            int
	called            bool
	result            github.PullRequestDiffResult
	err               error
}

func (f *enabledReader) GetPullRequestDiff(_ context.Context, owner, repository string, number int, _ string) (github.PullRequestDiffResult, error) {
	f.owner, f.repository, f.number, f.called = owner, repository, number, true
	return f.result, f.err
}

type enabledPublisher struct {
	called     bool
	submission github.ReviewSubmission
	result     github.SubmittedReview
	err        error
}

func (f *enabledPublisher) SubmitReview(_ context.Context, submission github.ReviewSubmission) (github.SubmittedReview, error) {
	f.called, f.submission = true, submission
	return f.result, f.err
}

type enabledRecorder struct {
	input sqlite.RecordEnabledPublicationAttemptInput
	err   error
}

func (f *enabledRecorder) RecordEnabledPublicationAttempt(_ context.Context, input sqlite.RecordEnabledPublicationAttemptInput) (sqlite.RecordEnabledPublicationAttemptResult, error) {
	f.input = input
	return sqlite.RecordEnabledPublicationAttemptResult{}, f.err
}
