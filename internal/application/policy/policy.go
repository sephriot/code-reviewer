// Package policy deterministically maps a validated assessment and immutable
// pull-request facts into a publication-safe review disposition.
package policy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
)

// Disposition is a policy conclusion, not a GitHub operation.
type Disposition string

const (
	DispositionNoExternalAction    Disposition = "no_external_action"
	DispositionAutoPublishApproval Disposition = "auto_publish_approval"
	DispositionProposeApproval     Disposition = "propose_approval"
	DispositionProposeComment      Disposition = "propose_comment"
	DispositionProposeChanges      Disposition = "propose_changes"
	DispositionRequireHumanReview  Disposition = "require_human_review"
)

// SafetyOverride records a deterministic built-in policy gate.
type SafetyOverride string

const (
	// SafetyOverrideAuthoredPRBlocksApproval prevents self-approval.
	SafetyOverrideAuthoredPRBlocksApproval SafetyOverride = "authored_pr_blocks_approval"
	// SafetyOverrideAuthoredPRBlocksChanges prevents self-requested changes.
	SafetyOverrideAuthoredPRBlocksChanges SafetyOverride = "authored_pr_blocks_changes"
	// SafetyOverrideCoverageBlocksAutoApproval requires a human decision when
	// review coverage is incomplete or unknown.
	SafetyOverrideCoverageBlocksAutoApproval SafetyOverride = "coverage_blocks_auto_approval"
	// SafetyOverrideAutoApprovalRequirements records a failed silent-approval
	// precondition other than coverage.
	SafetyOverrideAutoApprovalRequirements SafetyOverride = "auto_approval_requirements_not_met"
	// SafetyOverrideAutomaticApprovalDisabled prevents a matrix from enabling
	// an automatic approval without an explicit policy opt-in.
	SafetyOverrideAutomaticApprovalDisabled SafetyOverride = "automatic_approval_not_enabled"
	// SafetyOverrideDraftPullRequest suppresses external review artifacts for a
	// draft pull request.
	SafetyOverrideDraftPullRequest SafetyOverride = "draft_pull_request"
)

// Facts are immutable pull-request facts captured with the assessment.
type Facts struct {
	AuthoredByMe    bool
	Terminal        bool
	Draft           bool
	EvidenceCurrent bool
	Coverage        assessment.Coverage
}

// PublicationPolicy selects a requested disposition for each assessment
// verdict. Omitted verdicts use the documented safe defaults.
type PublicationPolicy struct {
	Matrix                 map[assessment.Verdict]Disposition
	AllowAutomaticApproval bool
}

// Input is one pure policy evaluation request.
type Input struct {
	Assessment assessment.Result
	Facts      Facts
	Policy     PublicationPolicy
}

// InlineComment is an evidence-bound comment proposed for one diff range.
type InlineComment struct {
	Path      string
	StartLine int
	EndLine   int
	Side      assessment.Side
	SHA       string
	Body      string
}

// Proposal is deterministic review content for a disposition requiring a
// GitHub review artifact. It has no side effects.
type Proposal struct {
	Kind           string
	Body           string
	InlineComments []InlineComment
}

// Result is the complete outcome of one pure policy evaluation.
type Result struct {
	Disposition     Disposition
	SafetyOverrides []SafetyOverride
	Proposal        *Proposal
}

var (
	// ErrStaleEvidence means no proposal may be based on this evidence.
	ErrStaleEvidence = errors.New("policy evidence is not current")
	// ErrTerminalPullRequest means no proposal may target a terminal pull request.
	ErrTerminalPullRequest = errors.New("policy pull request is terminal")
)

// Evaluate applies built-in safety gates and renders a deterministic proposal.
// It performs no I/O and does not retain input references in its result.
func Evaluate(input Input) (Result, error) {
	if !input.Facts.EvidenceCurrent {
		return Result{}, ErrStaleEvidence
	}
	if input.Facts.Terminal {
		return Result{}, ErrTerminalPullRequest
	}
	if err := validateInput(input); err != nil {
		return Result{}, err
	}

	if input.Facts.Draft {
		return Result{
			Disposition:     DispositionNoExternalAction,
			SafetyOverrides: []SafetyOverride{SafetyOverrideDraftPullRequest},
		}, nil
	}

	disposition := requestedDisposition(input.Policy, input.Assessment.Assessment.Verdict)
	overrides := make([]SafetyOverride, 0, 2)
	if input.Facts.AuthoredByMe {
		switch disposition {
		case DispositionAutoPublishApproval, DispositionProposeApproval:
			disposition = DispositionNoExternalAction
			overrides = append(overrides, SafetyOverrideAuthoredPRBlocksApproval)
		case DispositionProposeChanges:
			disposition = DispositionNoExternalAction
			overrides = append(overrides, SafetyOverrideAuthoredPRBlocksChanges)
		}
	}
	if disposition == DispositionAutoPublishApproval {
		if !input.Policy.AllowAutomaticApproval {
			disposition = DispositionProposeApproval
			overrides = append(overrides, SafetyOverrideAutomaticApprovalDisabled)
		} else if coverageBlocksAutomaticApproval(input.Assessment.Assessment.Coverage) {
			disposition = DispositionProposeApproval
			overrides = append(overrides, SafetyOverrideCoverageBlocksAutoApproval)
		} else if !meetsAutomaticApprovalRequirements(input.Assessment.Assessment) {
			disposition = DispositionProposeApproval
			overrides = append(overrides, SafetyOverrideAutoApprovalRequirements)
		}
	}

	proposal, err := renderProposal(disposition, input.Assessment.Assessment)
	if err != nil {
		return Result{}, err
	}
	return Result{Disposition: disposition, SafetyOverrides: overrides, Proposal: proposal}, nil
}

func validateInput(input Input) error {
	value := input.Assessment.Assessment
	if value.Version != assessment.Version1 || !validVerdict(value.Verdict) ||
		!validConfidence(value.Confidence) || strings.TrimSpace(value.Summary) == "" ||
		value.Limitations == nil || value.Findings == nil || value.Coverage.Omitted == nil {
		return errors.New("policy assessment is not a validated v1 result")
	}
	if !validCoverage(value.Coverage) || !reflect.DeepEqual(value.Coverage, input.Facts.Coverage) {
		return errors.New("policy coverage facts do not match assessment")
	}
	for _, warning := range input.Assessment.ValidationWarnings {
		if strings.TrimSpace(warning.Code) == "" || strings.TrimSpace(warning.Message) == "" {
			return errors.New("policy assessment warning is invalid")
		}
	}
	for _, finding := range value.Findings {
		if !validSeverity(finding.Severity) || strings.TrimSpace(finding.ClientID) == "" || strings.TrimSpace(finding.Message) == "" {
			return errors.New("policy assessment finding is invalid")
		}
		if finding.Anchor != nil && (strings.TrimSpace(finding.Anchor.Path) == "" ||
			finding.Anchor.StartLine <= 0 || finding.Anchor.EndLine < finding.Anchor.StartLine ||
			(finding.Anchor.Side != assessment.SideLeft && finding.Anchor.Side != assessment.SideRight) ||
			strings.TrimSpace(finding.Anchor.SHA) == "") {
			return errors.New("policy assessment finding anchor is invalid")
		}
	}
	for verdict, disposition := range input.Policy.Matrix {
		if !validVerdict(verdict) || !validDisposition(disposition) {
			return errors.New("policy publication matrix is invalid")
		}
		if disposition == DispositionAutoPublishApproval && verdict != assessment.VerdictPass {
			return errors.New("policy publication matrix auto approval requires pass verdict")
		}
	}
	return nil
}

func requestedDisposition(policy PublicationPolicy, verdict assessment.Verdict) Disposition {
	if configured, found := policy.Matrix[verdict]; found {
		return configured
	}
	switch verdict {
	case assessment.VerdictPass:
		return DispositionProposeApproval
	case assessment.VerdictConcerns:
		return DispositionProposeComment
	case assessment.VerdictChangesRequired:
		return DispositionProposeChanges
	default:
		return DispositionRequireHumanReview
	}
}

func coverageBlocksAutomaticApproval(value assessment.Coverage) bool {
	return value.Status != assessment.CoverageComplete || value.ReviewedFiles != value.ChangedFilesTotal || len(value.Omitted) != 0
}

func meetsAutomaticApprovalRequirements(value assessment.Assessment) bool {
	if value.Verdict != assessment.VerdictPass || value.Confidence != assessment.ConfidenceHigh ||
		value.Coverage.Status != assessment.CoverageComplete || len(value.Findings) != 0 {
		return false
	}
	return true
}

func renderProposal(disposition Disposition, value assessment.Assessment) (*Proposal, error) {
	kind, needsProposal := proposalKind(disposition)
	if !needsProposal {
		return nil, nil
	}
	if disposition == DispositionAutoPublishApproval {
		return &Proposal{Kind: kind, Body: "", InlineComments: []InlineComment{}}, nil
	}

	anchored, unanchored := splitFindings(value.Findings)
	body := renderBody(value, unanchored)
	inline := make([]InlineComment, 0, len(anchored))
	for _, finding := range anchored {
		anchor := finding.Anchor
		inline = append(inline, InlineComment{
			Path: anchor.Path, StartLine: anchor.StartLine, EndLine: anchor.EndLine,
			Side: anchor.Side, SHA: anchor.SHA, Body: renderFinding(finding),
		})
	}
	return &Proposal{Kind: kind, Body: body, InlineComments: inline}, nil
}

func proposalKind(disposition Disposition) (string, bool) {
	switch disposition {
	case DispositionAutoPublishApproval, DispositionProposeApproval:
		return "approval", true
	case DispositionProposeComment:
		return "comment", true
	case DispositionProposeChanges:
		return "changes", true
	default:
		return "", false
	}
}

func splitFindings(findings []assessment.Finding) ([]assessment.Finding, []assessment.Finding) {
	ordered := append([]assessment.Finding(nil), findings...)
	sort.SliceStable(ordered, func(i, j int) bool { return findingKey(ordered[i]) < findingKey(ordered[j]) })
	anchored := make([]assessment.Finding, 0, len(ordered))
	unanchored := make([]assessment.Finding, 0, len(ordered))
	for _, finding := range ordered {
		if finding.Anchor == nil {
			unanchored = append(unanchored, finding)
			continue
		}
		anchored = append(anchored, finding)
	}
	return anchored, unanchored
}

func findingKey(finding assessment.Finding) string {
	severity := map[assessment.Severity]string{
		assessment.SeverityBlocker: "0", assessment.SeverityHigh: "1", assessment.SeverityMedium: "2",
		assessment.SeverityLow: "3", assessment.SeverityNote: "4",
	}[finding.Severity]
	path, start, end, side := "", 0, 0, ""
	if finding.Anchor != nil {
		path, start, end, side = finding.Anchor.Path, finding.Anchor.StartLine, finding.Anchor.EndLine, string(finding.Anchor.Side)
	}
	return fmt.Sprintf("%s\x00%s\x00%010d\x00%010d\x00%s\x00%s\x00%s\x00%s", severity, path, start, end, side, finding.Category, finding.Message, finding.ClientID)
}

func renderBody(value assessment.Assessment, unanchored []assessment.Finding) string {
	sections := []string{"Review summary:\n" + strings.TrimSpace(value.Summary)}
	if len(value.Limitations) > 0 {
		lines := make([]string, 0, len(value.Limitations))
		for _, limitation := range value.Limitations {
			lines = append(lines, "- "+strings.TrimSpace(limitation))
		}
		sort.Strings(lines)
		sections = append(sections, "Limitations:\n"+strings.Join(lines, "\n"))
	}
	if len(unanchored) > 0 {
		lines := make([]string, 0, len(unanchored))
		for _, finding := range unanchored {
			lines = append(lines, "- "+renderFinding(finding))
		}
		sections = append(sections, "Unanchored findings:\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func renderFinding(finding assessment.Finding) string {
	body := fmt.Sprintf("[%s/%s] %s", finding.Severity, finding.Category, strings.TrimSpace(finding.Message))
	if strings.TrimSpace(finding.Evidence) != "" {
		body += "\nEvidence: " + strings.TrimSpace(finding.Evidence)
	}
	if strings.TrimSpace(finding.Suggestion) != "" {
		body += "\nSuggestion: " + strings.TrimSpace(finding.Suggestion)
	}
	return body
}

func validVerdict(value assessment.Verdict) bool {
	return value == assessment.VerdictPass || value == assessment.VerdictConcerns ||
		value == assessment.VerdictChangesRequired || value == assessment.VerdictInconclusive
}

func validConfidence(value assessment.Confidence) bool {
	return value == assessment.ConfidenceHigh || value == assessment.ConfidenceMedium || value == assessment.ConfidenceLow
}

func validSeverity(value assessment.Severity) bool {
	return value == assessment.SeverityBlocker || value == assessment.SeverityHigh || value == assessment.SeverityMedium ||
		value == assessment.SeverityLow || value == assessment.SeverityNote
}

func validCoverage(value assessment.Coverage) bool {
	return (value.Status == assessment.CoverageComplete || value.Status == assessment.CoveragePartial || value.Status == assessment.CoverageUnknown) &&
		value.ChangedFilesTotal >= 0 && value.ReviewedFiles >= 0 && value.ReviewedFiles <= value.ChangedFilesTotal
}

func validDisposition(value Disposition) bool {
	switch value {
	case DispositionNoExternalAction, DispositionAutoPublishApproval, DispositionProposeApproval,
		DispositionProposeComment, DispositionProposeChanges, DispositionRequireHumanReview:
		return true
	default:
		return false
	}
}
