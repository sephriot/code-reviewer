package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
)

// ErrPolicyEvaluationTargetNotFound means no completed assessment remains
// bound to the currently selected canonical pull-request evidence.
var ErrPolicyEvaluationTargetNotFound = errors.New("policy evaluation target not found")

// ErrActivePolicyRuleNotFound means an explicitly selected rule is disabled,
// stale, or is not its stable rule's current immutable version.
var ErrActivePolicyRuleNotFound = errors.New("active policy rule not found")

// PolicyEvaluationFacts are the current immutable PR facts required by the
// pure policy package. EvidenceCurrent is true only after the assessment's
// canonical evidence is proved to equal the current projection target.
type PolicyEvaluationFacts struct {
	AuthoredByMe    bool
	Terminal        bool
	Draft           bool
	EvidenceCurrent bool
	Coverage        assessment.Coverage
}

// PolicyEvaluationTarget is the completed assessment and its current policy
// facts. It is read-only and contains no publication authority.
type PolicyEvaluationTarget struct {
	AssessmentID     string
	OutputSHA256     string
	ProfileID        string
	ProfileVersionID string
	Assessment       assessment.Result
	Facts            PolicyEvaluationFacts
}

// ActivePolicyRule is an explicit stable rule/version lookup. Rule matching
// is deliberately outside this read model.
type ActivePolicyRule struct {
	PolicySetID      string
	RuleID           string
	VersionID        string
	RuleKey          string
	ProfileID        string
	ProfileVersionID string
	PublicationJSON  []byte
}

// LoadActivePolicyRule returns one enabled rule only when the requested
// immutable version is its current selected version.
func (s *Store) LoadActivePolicyRule(ctx context.Context, ruleKey, versionID string) (ActivePolicyRule, error) {
	ruleKey = strings.TrimSpace(ruleKey)
	versionID = strings.TrimSpace(versionID)
	if ruleKey == "" || versionID == "" {
		return ActivePolicyRule{}, errors.New("policy rule key and version ID are required")
	}
	var value ActivePolicyRule
	err := s.db.QueryRowContext(ctx, `
SELECT version.policy_set_id, rule.id, version.id, rule.rule_key,
       version.profile_id, version.profile_version_id, version.publication_json
FROM watch_rules AS rule
JOIN watch_rule_versions AS version
  ON version.id = rule.current_version_id AND version.rule_id = rule.id
WHERE rule.rule_key = ? COLLATE NOCASE
  AND version.id = ?
  AND rule.enabled = 1`, ruleKey, versionID).Scan(
		&value.PolicySetID, &value.RuleID, &value.VersionID, &value.RuleKey,
		&value.ProfileID, &value.ProfileVersionID, &value.PublicationJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ActivePolicyRule{}, ErrActivePolicyRuleNotFound
	}
	if err != nil {
		return ActivePolicyRule{}, fmt.Errorf("load active policy rule: %w", err)
	}
	if value.PolicySetID == "" || value.RuleID == "" || value.VersionID != versionID ||
		!strings.EqualFold(value.RuleKey, ruleKey) || value.ProfileID == "" || value.ProfileVersionID == "" {
		return ActivePolicyRule{}, errors.New("stored active policy rule is invalid")
	}
	publication, err := normalizePolicyJSONObject(value.PublicationJSON)
	if err != nil || !bytes.Equal(publication, value.PublicationJSON) {
		return ActivePolicyRule{}, errors.New("stored active policy rule publication is invalid")
	}
	value.PublicationJSON = append([]byte(nil), value.PublicationJSON...)
	return value, nil
}

// LoadPolicyEvaluationTarget returns an assessment only after checking its
// success event, immutable manifest context, and exact current canonical
// observation/revision selection.
func (s *Store) LoadPolicyEvaluationTarget(ctx context.Context, assessmentID string) (PolicyEvaluationTarget, error) {
	assessmentID = strings.TrimSpace(assessmentID)
	if assessmentID == "" {
		return PolicyEvaluationTarget{}, errors.New("assessment ID is required")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return PolicyEvaluationTarget{}, fmt.Errorf("open policy evaluation read connection: %w", err)
	}
	defer conn.Close()
	completed, err := loadCompletedPolicyAssessment(ctx, conn, assessmentID)
	if errors.Is(err, ErrPolicyAssessmentNotFound) {
		return PolicyEvaluationTarget{}, ErrPolicyEvaluationTargetNotFound
	}
	if err != nil {
		return PolicyEvaluationTarget{}, fmt.Errorf("load completed policy assessment: %w", err)
	}
	current, err := loadCurrentCanonicalReviewTarget(ctx, conn, completed.ConnectionID, completed.PullRequestID)
	if errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		return PolicyEvaluationTarget{}, ErrPolicyEvaluationTargetNotFound
	}
	if err != nil {
		return PolicyEvaluationTarget{}, fmt.Errorf("load current policy evidence: %w", err)
	}
	if !completed.matchesCurrentTarget(current) {
		return PolicyEvaluationTarget{}, ErrPolicyEvaluationTargetNotFound
	}

	var value PolicyEvaluationTarget
	var limitations, coverage, relationships []byte
	var draft int
	var state string
	err = conn.QueryRowContext(ctx, `
SELECT assessment.id, assessment.output_sha256, intent.profile_id, intent.profile_version_id,
       assessment.schema_version, assessment.verdict, assessment.summary, assessment.confidence,
       assessment.limitations_json, assessment.coverage_json,
       observation.relationship_set_json, observation.is_draft, observation.github_state
FROM assessments AS assessment
JOIN review_runs AS run ON run.id = assessment.run_id AND run.intent_id = assessment.intent_id
JOIN review_intents AS intent ON intent.id = run.intent_id
JOIN pull_request_observations AS observation
  ON observation.id = assessment.observation_id
 AND observation.pull_request_id = assessment.pull_request_id
 AND observation.connection_id = intent.connection_id
WHERE assessment.id = ?`, assessmentID).Scan(
		&value.AssessmentID, &value.OutputSHA256, &value.ProfileID, &value.ProfileVersionID,
		&value.Assessment.Assessment.Version, &value.Assessment.Assessment.Verdict,
		&value.Assessment.Assessment.Summary, &value.Assessment.Assessment.Confidence,
		&limitations, &coverage, &relationships, &draft, &state,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyEvaluationTarget{}, ErrPolicyEvaluationTargetNotFound
	}
	if err != nil {
		return PolicyEvaluationTarget{}, fmt.Errorf("load policy assessment facts: %w", err)
	}
	if err := json.Unmarshal(limitations, &value.Assessment.Assessment.Limitations); err != nil || value.Assessment.Assessment.Limitations == nil {
		return PolicyEvaluationTarget{}, errors.New("stored assessment limitations are invalid")
	}
	if err := json.Unmarshal(coverage, &value.Assessment.Assessment.Coverage); err != nil || value.Assessment.Assessment.Coverage.Omitted == nil {
		return PolicyEvaluationTarget{}, errors.New("stored assessment coverage is invalid")
	}
	value.Facts.Coverage = value.Assessment.Assessment.Coverage
	value.Facts.Draft = draft == 1
	value.Facts.Terminal = state != "open"
	value.Facts.EvidenceCurrent = true
	if err := decodeAuthoredRelationship(relationships, &value.Facts.AuthoredByMe); err != nil {
		return PolicyEvaluationTarget{}, err
	}
	if err := loadPolicyAssessmentFindings(ctx, conn, assessmentID, current, &value.Assessment); err != nil {
		return PolicyEvaluationTarget{}, err
	}
	if err := loadPolicyAssessmentWarnings(ctx, conn, assessmentID, &value.Assessment); err != nil {
		return PolicyEvaluationTarget{}, err
	}
	return value, nil
}

func decodeAuthoredRelationship(raw []byte, authored *bool) error {
	var relationships []string
	if err := json.Unmarshal(raw, &relationships); err != nil || relationships == nil {
		return errors.New("stored pull request relationships are invalid")
	}
	for _, relationship := range relationships {
		if relationship == "authored_by_me" {
			*authored = true
		}
	}
	return nil
}

func loadPolicyAssessmentFindings(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, assessmentID string, current CanonicalReviewTarget, result *assessment.Result) error {
	rows, err := queryer.QueryContext(ctx, `
SELECT client_id, severity, category, message, evidence, suggestion,
       path, line, side, anchor_status
FROM findings WHERE assessment_id = ? ORDER BY client_id`, assessmentID)
	if err != nil {
		return fmt.Errorf("load assessment findings: %w", err)
	}
	defer rows.Close()
	result.Assessment.Findings = make([]assessment.Finding, 0)
	for rows.Next() {
		var finding assessment.Finding
		var evidence, suggestion, path, side sql.NullString
		var line sql.NullInt64
		var status string
		if err := rows.Scan(&finding.ClientID, &finding.Severity, &finding.Category, &finding.Message, &evidence, &suggestion, &path, &line, &side, &status); err != nil {
			return fmt.Errorf("scan assessment finding: %w", err)
		}
		finding.Evidence, finding.Suggestion = evidence.String, suggestion.String
		switch status {
		case "unanchored", "downgraded":
			if path.Valid || line.Valid || side.Valid {
				return errors.New("stored unanchored finding has coordinates")
			}
		case "valid":
			if !path.Valid || !line.Valid || !side.Valid || line.Int64 <= 0 || (side.String != string(assessment.SideLeft) && side.String != string(assessment.SideRight)) {
				return errors.New("stored anchored finding is invalid")
			}
			sha := current.HeadSHA
			if side.String == string(assessment.SideLeft) {
				sha = current.BaseSHA
			}
			finding.Anchor = &assessment.Anchor{Path: path.String, StartLine: int(line.Int64), EndLine: int(line.Int64), Side: assessment.Side(side.String), SHA: sha}
		default:
			return errors.New("stored finding anchor status is invalid")
		}
		result.Assessment.Findings = append(result.Assessment.Findings, finding)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate assessment findings: %w", err)
	}
	return nil
}

func loadPolicyAssessmentWarnings(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, assessmentID string, result *assessment.Result) error {
	rows, err := queryer.QueryContext(ctx, `SELECT warning_code, message FROM validation_warnings WHERE assessment_id = ? ORDER BY warning_code`, assessmentID)
	if err != nil {
		return fmt.Errorf("load assessment warnings: %w", err)
	}
	defer rows.Close()
	result.ValidationWarnings = make([]assessment.ValidationWarning, 0)
	for rows.Next() {
		var warning assessment.ValidationWarning
		if err := rows.Scan(&warning.Code, &warning.Message); err != nil {
			return fmt.Errorf("scan assessment warning: %w", err)
		}
		result.ValidationWarnings = append(result.ValidationWarnings, warning)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate assessment warnings: %w", err)
	}
	return nil
}
