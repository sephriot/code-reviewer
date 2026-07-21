package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

// ErrAssessmentConflict means a completed run already has a different
// immutable validated assessment.
var ErrAssessmentConflict = errors.New("review assessment conflicts with existing run output")

// RecordAssessmentInput holds a previously validated engine judgment for one
// prepared run. Callers must obtain Result from assessment.Validate before
// storing it; this method separately verifies durable evidence is still
// current and exact.
type RecordAssessmentInput struct {
	RunID      string
	Result     assessment.Result
	RecordedAt time.Time
}

// RecordAssessmentResult identifies the immutable assessment written for a
// run. A matching repeated request returns the original IDs without adding
// records or events.
type RecordAssessmentResult struct {
	AssessmentID string
	OutputSHA256 string
	Created      bool
}

// RecordAssessment atomically records a validated assessment and terminal
// success events after proving its run still points at the selected canonical
// evidence. It creates no jobs, domain events, outbox rows, or GitHub work.
func (s *Store) RecordAssessment(ctx context.Context, input RecordAssessmentInput) (RecordAssessmentResult, error) {
	normalized, err := normalizeRecordAssessmentInput(input)
	if err != nil {
		return RecordAssessmentResult{}, err
	}

	var result RecordAssessmentResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		run, err := loadAssessmentRun(ctx, conn, normalized.RunID)
		if err != nil {
			return err
		}
		target, err := loadCurrentCanonicalReviewTarget(ctx, conn, run.ConnectionID, run.PullRequestID)
		if err != nil {
			return fmt.Errorf("load current canonical target: %w", err)
		}
		if !assessmentRunMatchesCurrentTarget(run, target) {
			return errors.New("review run no longer matches current canonical evidence")
		}

		existing, found, err := loadRecordedAssessment(ctx, conn, normalized.RunID)
		if err != nil {
			return err
		}
		if found {
			if existing.OutputSHA256 != normalized.OutputSHA256 {
				return fmt.Errorf("%w: run=%q", ErrAssessmentConflict, normalized.RunID)
			}
			result = RecordAssessmentResult{AssessmentID: existing.ID, OutputSHA256: existing.OutputSHA256}
			return nil
		}

		recordedAt := normalized.RecordedAt.UnixMicro()
		assessmentID := stableID("assessment", normalized.RunID, normalized.OutputSHA256)
		if _, err := conn.ExecContext(ctx, `
INSERT INTO assessments(
 id, run_id, intent_id, pull_request_id, revision_id, observation_id,
 schema_version, verdict, summary, confidence, limitations_json, coverage_json,
 output_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			assessmentID, normalized.RunID, run.IntentID, run.PullRequestID, run.RevisionID, run.ObservationID,
			normalized.Assessment.Version, normalized.Assessment.Verdict, normalized.Assessment.Summary,
			normalized.Assessment.Confidence, normalized.LimitationsJSON, normalized.CoverageJSON,
			normalized.OutputSHA256, recordedAt); err != nil {
			return fmt.Errorf("insert assessment: %w", err)
		}
		for _, finding := range normalized.Assessment.Findings {
			if err := insertFinding(ctx, conn, assessmentID, run, finding, recordedAt); err != nil {
				return err
			}
		}
		for _, warning := range normalized.ValidationWarnings {
			if err := insertAssessmentWarning(ctx, conn, assessmentID, run, warning, recordedAt); err != nil {
				return err
			}
		}
		sequence, err := nextReviewRunEventSequence(ctx, conn, normalized.RunID)
		if err != nil {
			return err
		}
		for _, kind := range []string{"validating", "succeeded"} {
			if _, err := conn.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES (?, ?, ?, ?, '{}', ?, ?)`,
				stableID("review-run-event", normalized.RunID, fmt.Sprintf("%d", sequence)), normalized.RunID, sequence, kind, recordedAt, recordedAt); err != nil {
				return fmt.Errorf("insert review run %s event: %w", kind, err)
			}
			sequence++
		}
		result = RecordAssessmentResult{AssessmentID: assessmentID, OutputSHA256: normalized.OutputSHA256, Created: true}
		return nil
	})
	if err != nil {
		return RecordAssessmentResult{}, fmt.Errorf("record assessment: %w", err)
	}
	return result, nil
}

type normalizedRecordAssessmentInput struct {
	RunID              string
	Assessment         assessment.Assessment
	ValidationWarnings []assessment.ValidationWarning
	LimitationsJSON    []byte
	CoverageJSON       []byte
	OutputSHA256       string
	RecordedAt         time.Time
}

func normalizeRecordAssessmentInput(input RecordAssessmentInput) (normalizedRecordAssessmentInput, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	if input.RunID == "" {
		return normalizedRecordAssessmentInput{}, errors.New("assessment run ID is required")
	}
	if err := validateRecordableAssessment(input.Result); err != nil {
		return normalizedRecordAssessmentInput{}, err
	}
	limitations, err := json.Marshal(input.Result.Assessment.Limitations)
	if err != nil {
		return normalizedRecordAssessmentInput{}, fmt.Errorf("encode assessment limitations: %w", err)
	}
	coverage, err := json.Marshal(input.Result.Assessment.Coverage)
	if err != nil {
		return normalizedRecordAssessmentInput{}, fmt.Errorf("encode assessment coverage: %w", err)
	}
	payload, err := json.Marshal(struct {
		Assessment         assessment.Assessment          `json:"assessment"`
		ValidationWarnings []assessment.ValidationWarning `json:"validation_warnings"`
	}{Assessment: input.Result.Assessment, ValidationWarnings: input.Result.ValidationWarnings})
	if err != nil {
		return normalizedRecordAssessmentInput{}, fmt.Errorf("encode assessment output: %w", err)
	}
	digest := sha256.Sum256(payload)
	if input.RecordedAt.IsZero() {
		input.RecordedAt = time.Now().UTC()
	} else {
		input.RecordedAt = input.RecordedAt.UTC()
	}
	if input.RecordedAt.UnixMicro() < 0 {
		return normalizedRecordAssessmentInput{}, errors.New("assessment recorded time is invalid")
	}
	return normalizedRecordAssessmentInput{
		RunID: input.RunID, Assessment: input.Result.Assessment,
		ValidationWarnings: append([]assessment.ValidationWarning(nil), input.Result.ValidationWarnings...),
		LimitationsJSON:    limitations, CoverageJSON: coverage,
		OutputSHA256: hex.EncodeToString(digest[:]), RecordedAt: input.RecordedAt,
	}, nil
}

func validateRecordableAssessment(result assessment.Result) error {
	value := result.Assessment
	if value.Version != assessment.Version1 || value.Summary == "" || value.Limitations == nil || value.Findings == nil || value.Coverage.Omitted == nil {
		return errors.New("assessment result is not a validated v1 contract")
	}
	warnings := make(map[string]struct{}, len(result.ValidationWarnings))
	for _, warning := range result.ValidationWarnings {
		if strings.TrimSpace(warning.Code) == "" || strings.TrimSpace(warning.Message) == "" {
			return errors.New("assessment validation warning is invalid")
		}
		if _, duplicate := warnings[warning.Code]; duplicate {
			return errors.New("assessment validation warning code is duplicated")
		}
		warnings[warning.Code] = struct{}{}
	}
	return nil
}

type assessmentRun struct {
	RunID          string
	IntentID       string
	ConnectionID   string
	PullRequestID  string
	RevisionID     string
	ObservationID  string
	ManifestSHA256 string
	ManifestJSON   []byte
}

func loadAssessmentRun(ctx context.Context, conn *sql.Conn, runID string) (assessmentRun, error) {
	var run assessmentRun
	err := conn.QueryRowContext(ctx, `
	SELECT run.id, run.intent_id, intent.connection_id, run.pull_request_id, run.revision_id,
       run.observation_id, context.manifest_sha256, context.manifest_json
FROM review_runs AS run
JOIN review_intents AS intent ON intent.id = run.intent_id
JOIN review_run_contexts AS context ON context.run_id = run.id
WHERE run.id = ?`, runID).Scan(
		&run.RunID, &run.IntentID, &run.ConnectionID, &run.PullRequestID, &run.RevisionID,
		&run.ObservationID, &run.ManifestSHA256, &run.ManifestJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return assessmentRun{}, errors.New("review run does not exist or has no immutable context")
	}
	if err != nil {
		return assessmentRun{}, fmt.Errorf("load assessment run: %w", err)
	}
	verified, err := canonical.Validate(run.ManifestJSON)
	if err != nil || verified.ManifestSHA256 != run.ManifestSHA256 {
		return assessmentRun{}, errors.New("review run manifest context is invalid")
	}
	return run, nil
}

func assessmentRunMatchesCurrentTarget(run assessmentRun, target CanonicalReviewTarget) bool {
	return run.ConnectionID == target.ConnectionID &&
		run.PullRequestID == target.PullRequestID &&
		run.RevisionID == target.RevisionID &&
		run.ObservationID == target.ObservationID &&
		run.ManifestSHA256 == target.ManifestSHA256 &&
		bytes.Equal(run.ManifestJSON, target.ManifestJSON)
}

type recordedAssessment struct {
	ID           string
	OutputSHA256 string
}

func loadRecordedAssessment(ctx context.Context, conn *sql.Conn, runID string) (recordedAssessment, bool, error) {
	var value recordedAssessment
	err := conn.QueryRowContext(ctx, `SELECT id, output_sha256 FROM assessments WHERE run_id = ?`, runID).Scan(&value.ID, &value.OutputSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return recordedAssessment{}, false, nil
	}
	if err != nil {
		return recordedAssessment{}, false, fmt.Errorf("load recorded assessment: %w", err)
	}
	return value, true, nil
}

func insertFinding(ctx context.Context, conn *sql.Conn, assessmentID string, run assessmentRun, finding assessment.Finding, createdAt int64) error {
	fingerprintPayload, err := json.Marshal(finding)
	if err != nil {
		return fmt.Errorf("encode finding fingerprint: %w", err)
	}
	fingerprint := sha256.Sum256(fingerprintPayload)
	path, line, side, status := "", 0, "", "unanchored"
	if finding.Anchor != nil {
		path, line, side, status = finding.Anchor.Path, finding.Anchor.StartLine, string(finding.Anchor.Side), "valid"
	}
	if _, err := conn.ExecContext(ctx, `
INSERT INTO findings(
 id, assessment_id, run_id, pull_request_id, revision_id, observation_id,
 client_id, fingerprint_sha256, severity, category, path, line, side, message,
 evidence, suggestion, anchor_status, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''), ?,
        NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		stableID("assessment-finding", assessmentID, finding.ClientID), assessmentID, run.RunID, run.PullRequestID, run.RevisionID, run.ObservationID,
		finding.ClientID, hex.EncodeToString(fingerprint[:]), finding.Severity, finding.Category, path, line, side, finding.Message,
		finding.Evidence, finding.Suggestion, status, createdAt); err != nil {
		return fmt.Errorf("insert finding %q: %w", finding.ClientID, err)
	}
	return nil
}

func insertAssessmentWarning(ctx context.Context, conn *sql.Conn, assessmentID string, run assessmentRun, warning assessment.ValidationWarning, createdAt int64) error {
	if _, err := conn.ExecContext(ctx, `
INSERT INTO validation_warnings(
 id, assessment_id, run_id, pull_request_id, revision_id, observation_id,
 warning_code, message, details_json, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', ?)`,
		stableID("assessment-warning", assessmentID, warning.Code), assessmentID, run.RunID, run.PullRequestID, run.RevisionID, run.ObservationID,
		warning.Code, warning.Message, createdAt); err != nil {
		return fmt.Errorf("insert validation warning %q: %w", warning.Code, err)
	}
	return nil
}

func nextReviewRunEventSequence(ctx context.Context, conn *sql.Conn, runID string) (int, error) {
	var sequence int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM review_run_events WHERE run_id = ?`, runID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("next review run event sequence: %w", err)
	}
	return sequence, nil
}
