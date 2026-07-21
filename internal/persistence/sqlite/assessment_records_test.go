package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
)

func TestRecordAssessmentPersistsValidatedEvidenceBoundOutput(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	prepared, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}

	assessmentResult := testValidatedAssessmentResult(t)
	assessmentResult.ValidationWarnings = []assessment.ValidationWarning{{
		Code: "coverage_adjusted", Message: "Coverage normalized after validation.",
	}}
	recorded, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: prepared.RunID, Result: assessmentResult, RecordedAt: time.Unix(40, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.Created || recorded.AssessmentID == "" || len(recorded.OutputSHA256) != 64 {
		t.Fatalf("recorded = %+v", recorded)
	}
	for _, table := range []string{"assessments", "findings"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	assertTableCount(t, ctx, store.db, "validation_warnings", 1)
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}

	rows, err := store.db.QueryContext(ctx, `SELECT event_kind FROM review_run_events WHERE run_id = ? ORDER BY sequence`, prepared.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var events []string
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "queued,preparing,validating,succeeded" {
		t.Fatalf("events = %q", got)
	}

	var path, side string
	var line int
	if err := store.db.QueryRowContext(ctx, `SELECT path, line, side FROM findings WHERE assessment_id = ?`, recorded.AssessmentID).Scan(&path, &line, &side); err != nil {
		t.Fatal(err)
	}
	if path != "internal/example.go" || line != 2 || side != "RIGHT" {
		t.Fatalf("finding anchor = %q:%d:%q", path, line, side)
	}
}

func TestRecordAssessmentIsIdempotentAndRejectsChangedOutput(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	prepared, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}
	input := RecordAssessmentInput{RunID: prepared.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(40, 0).UTC()}
	first, err := store.RecordAssessment(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.RecordedAt = input.RecordedAt.Add(time.Hour)
	second, err := store.RecordAssessment(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.AssessmentID != second.AssessmentID || first.OutputSHA256 != second.OutputSHA256 {
		t.Fatalf("results = %+v / %+v", first, second)
	}

	input.Result.Assessment.Summary = "Different validated output."
	_, err = store.RecordAssessment(ctx, input)
	if !errors.Is(err, ErrAssessmentConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	for _, table := range []string{"assessments", "findings"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 4)
}

func TestRecordAssessmentRequiresStillCurrentCanonicalEvidence(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	prepared, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordAssessment(ctx, RecordAssessmentInput{RunID: prepared.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(40, 0).UTC()})
	if !errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		t.Fatalf("error = %v", err)
	}
	for _, table := range []string{"assessments", "findings", "validation_warnings"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 2)
}

func testValidatedAssessmentResult(t *testing.T) assessment.Result {
	t.Helper()
	raw := []byte(`{
"version":1,
"verdict":"concerns",
"summary":"Nil input can panic.",
"confidence":"high",
"limitations":[],
"coverage":{"status":"complete","changed_files_total":1,"reviewed_files":1,"omitted":[]},
"findings":[{"client_id":"nil-input","severity":"high","category":"correctness","message":"Guard optional input.","evidence":"Caller allows nil.","suggestion":"Return early for nil.","anchor":{"path":"internal/example.go","start_line":2,"end_line":2,"side":"RIGHT","sha":"` + projectionHeadSHA + `"}}]
}`)
	result, err := assessment.Validate(raw, assessment.RevisionEvidence{
		HeadSHA: projectionHeadSHA,
		BaseSHA: projectionBaseSHA,
		Files:   []assessment.FileEvidence{{Path: "internal/example.go", Right: []assessment.LineRange{{Start: 1, End: 2}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
