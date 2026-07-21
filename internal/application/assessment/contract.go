// Package assessment defines review-engine judgment independently from policy
// and GitHub publication actions.
package assessment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// Version1 is the only supported assessment document version.
	Version1 = 1

	maxDocumentBytes  = 1 << 20
	maxSummaryBytes   = 8 << 10
	maxFindings       = 200
	maxLimitations    = 100
	maxTextBytes      = 16 << 10
	maxClientIDBytes  = 256
	maxOmissionReason = 2 << 10
)

// Verdict is an engine conclusion, never a publication instruction.
type Verdict string

const (
	VerdictPass            Verdict = "pass"
	VerdictConcerns        Verdict = "concerns"
	VerdictChangesRequired Verdict = "changes_required"
	VerdictInconclusive    Verdict = "inconclusive"
)

// Confidence is an engine's certainty about its assessment.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// CoverageStatus describes review coverage claimed by an engine.
type CoverageStatus string

const (
	CoverageComplete CoverageStatus = "complete"
	CoveragePartial  CoverageStatus = "partial"
	CoverageUnknown  CoverageStatus = "unknown"
)

// Severity expresses relative finding impact.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityHigh    Severity = "high"
	SeverityMedium  Severity = "medium"
	SeverityLow     Severity = "low"
	SeverityNote    Severity = "note"
)

// Category classifies a finding without selecting any external action.
type Category string

const (
	CategoryCorrectness     Category = "correctness"
	CategorySecurity        Category = "security"
	CategoryPerformance     Category = "performance"
	CategoryTesting         Category = "testing"
	CategoryMaintainability Category = "maintainability"
	CategoryOther           Category = "other"
)

// Side identifies source (LEFT) or target (RIGHT) diff content.
type Side string

const (
	SideLeft  Side = "LEFT"
	SideRight Side = "RIGHT"
)

// Assessment is a normalized v1 review-engine judgment.
type Assessment struct {
	Version     int        `json:"version"`
	Verdict     Verdict    `json:"verdict"`
	Summary     string     `json:"summary"`
	Confidence  Confidence `json:"confidence"`
	Limitations []string   `json:"limitations"`
	Coverage    Coverage   `json:"coverage"`
	Findings    []Finding  `json:"findings"`
}

// Coverage records declared review completeness and explicit omissions.
type Coverage struct {
	Status            CoverageStatus `json:"status"`
	ChangedFilesTotal int            `json:"changed_files_total"`
	ReviewedFiles     int            `json:"reviewed_files"`
	Omitted           []Omission     `json:"omitted"`
}

// Omission describes one changed file excluded from assessment.
type Omission struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Finding is one concrete engine observation. Anchor is optional.
type Finding struct {
	ClientID   string   `json:"client_id"`
	Severity   Severity `json:"severity"`
	Category   Category `json:"category"`
	Message    string   `json:"message"`
	Evidence   string   `json:"evidence,omitempty"`
	Suggestion string   `json:"suggestion,omitempty"`
	Anchor     *Anchor  `json:"anchor,omitempty"`
}

// Anchor pinpoints a contiguous range in immutable revision evidence.
type Anchor struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Side      Side   `json:"side"`
	SHA       string `json:"sha"`
}

// ValidationWarning is produced by later normalization stages; engines cannot
// provide it through the strict assessment document.
type ValidationWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is an accepted assessment plus validation warnings. Strict v1 input
// creates no warnings; warning-producing normalization remains a separate step.
type Result struct {
	Assessment         Assessment
	ValidationWarnings []ValidationWarning
}

// RevisionEvidence is provider-neutral immutable evidence used to validate
// paths, anchor ranges, and side-specific commit SHAs.
type RevisionEvidence struct {
	HeadSHA string
	BaseSHA string
	Files   []FileEvidence
}

// FileEvidence exposes only changed paths and valid line ranges for each side.
type FileEvidence struct {
	Path  string
	Left  []LineRange
	Right []LineRange
}

// LineRange is an inclusive line range present in a diff side.
type LineRange struct {
	Start int
	End   int
}

// Validate decodes one strict v1 assessment document and verifies all anchors
// against provider-neutral immutable revision evidence.
func Validate(raw []byte, evidence RevisionEvidence) (Result, error) {
	if len(raw) == 0 || len(raw) > maxDocumentBytes {
		return Result{}, errors.New("assessment document size is invalid")
	}
	indexed, err := validateEvidence(evidence)
	if err != nil {
		return Result{}, fmt.Errorf("assessment revision evidence: %w", err)
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Result{}, fmt.Errorf("assessment document: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var assessment Assessment
	if err := decoder.Decode(&assessment); err != nil {
		return Result{}, fmt.Errorf("decode assessment document: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Result{}, errors.New("assessment document must contain one JSON object")
	}
	if err := validateAssessment(assessment, evidence, indexed); err != nil {
		return Result{}, err
	}
	return Result{Assessment: assessment, ValidationWarnings: []ValidationWarning{}}, nil
}

func validateAssessment(value Assessment, evidence RevisionEvidence, files map[string]FileEvidence) error {
	if value.Version != Version1 {
		return fmt.Errorf("unsupported assessment version %d", value.Version)
	}
	if !validVerdict(value.Verdict) || !validConfidence(value.Confidence) {
		return errors.New("assessment verdict or confidence is invalid")
	}
	if !validText(value.Summary, maxSummaryBytes) {
		return errors.New("assessment summary is invalid")
	}
	if value.Limitations == nil || len(value.Limitations) > maxLimitations {
		return errors.New("assessment limitations are invalid")
	}
	for _, limitation := range value.Limitations {
		if !validText(limitation, maxTextBytes) {
			return errors.New("assessment limitation is invalid")
		}
	}
	if err := validateCoverage(value.Coverage, files); err != nil {
		return err
	}
	if value.Findings == nil || len(value.Findings) > maxFindings {
		return errors.New("assessment findings are invalid")
	}
	clientIDs := make(map[string]struct{}, len(value.Findings))
	for index, finding := range value.Findings {
		if !validText(finding.ClientID, maxClientIDBytes) {
			return fmt.Errorf("finding %d client ID is invalid", index)
		}
		if _, duplicate := clientIDs[finding.ClientID]; duplicate {
			return fmt.Errorf("finding %d duplicates client ID", index)
		}
		clientIDs[finding.ClientID] = struct{}{}
		if !validSeverity(finding.Severity) || !validCategory(finding.Category) {
			return fmt.Errorf("finding %d severity or category is invalid", index)
		}
		if !validText(finding.Message, maxTextBytes) || !validOptionalText(finding.Evidence) || !validOptionalText(finding.Suggestion) {
			return fmt.Errorf("finding %d text is invalid", index)
		}
		if finding.Anchor != nil {
			if err := validateAnchor(*finding.Anchor, evidence, files); err != nil {
				return fmt.Errorf("finding %d anchor: %w", index, err)
			}
		}
	}
	return nil
}

func validateCoverage(value Coverage, files map[string]FileEvidence) error {
	if !validCoverageStatus(value.Status) || value.ChangedFilesTotal < 0 || value.ReviewedFiles < 0 || value.ReviewedFiles > value.ChangedFilesTotal || value.Omitted == nil {
		return errors.New("assessment coverage is invalid")
	}
	omitted := make(map[string]struct{}, len(value.Omitted))
	for _, omission := range value.Omitted {
		if !validPath(omission.Path) || !validText(omission.Reason, maxOmissionReason) {
			return errors.New("assessment coverage omission is invalid")
		}
		if _, found := files[omission.Path]; !found {
			return fmt.Errorf("assessment coverage omission path %q is not in revision", omission.Path)
		}
		if _, duplicate := omitted[omission.Path]; duplicate {
			return fmt.Errorf("assessment coverage omission path %q is duplicated", omission.Path)
		}
		omitted[omission.Path] = struct{}{}
	}
	return nil
}

func validateAnchor(anchor Anchor, evidence RevisionEvidence, files map[string]FileEvidence) error {
	if !validPath(anchor.Path) || anchor.StartLine <= 0 || anchor.EndLine < anchor.StartLine || !validSide(anchor.Side) || !validSHA(anchor.SHA) {
		return errors.New("shape is invalid")
	}
	file, found := files[anchor.Path]
	if !found {
		return fmt.Errorf("path %q is not in revision", anchor.Path)
	}
	wantSHA := evidence.HeadSHA
	ranges := file.Right
	if anchor.Side == SideLeft {
		wantSHA, ranges = evidence.BaseSHA, file.Left
	}
	if anchor.SHA != wantSHA {
		return errors.New("SHA does not match anchor side")
	}
	for _, lineRange := range ranges {
		if anchor.StartLine >= lineRange.Start && anchor.EndLine <= lineRange.End {
			return nil
		}
	}
	return errors.New("range is not present in revision diff")
}

func validateEvidence(value RevisionEvidence) (map[string]FileEvidence, error) {
	if !validSHA(value.HeadSHA) || !validSHA(value.BaseSHA) {
		return nil, errors.New("head and base SHAs must be lowercase 40-character hex")
	}
	files := make(map[string]FileEvidence, len(value.Files))
	for _, file := range value.Files {
		if !validPath(file.Path) {
			return nil, fmt.Errorf("path %q is invalid", file.Path)
		}
		if _, duplicate := files[file.Path]; duplicate {
			return nil, fmt.Errorf("path %q is duplicated", file.Path)
		}
		if err := validateRanges(file.Left); err != nil {
			return nil, fmt.Errorf("left ranges for %q: %w", file.Path, err)
		}
		if err := validateRanges(file.Right); err != nil {
			return nil, fmt.Errorf("right ranges for %q: %w", file.Path, err)
		}
		files[file.Path] = file
	}
	return files, nil
}

func validateRanges(ranges []LineRange) error {
	for _, lineRange := range ranges {
		if lineRange.Start <= 0 || lineRange.End < lineRange.Start {
			return errors.New("must be positive inclusive ranges")
		}
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, isString := keyToken.(string)
			if !isString {
				return errors.New("object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate key %q", key)
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}

func validVerdict(value Verdict) bool {
	return value == VerdictPass || value == VerdictConcerns || value == VerdictChangesRequired || value == VerdictInconclusive
}

func validConfidence(value Confidence) bool {
	return value == ConfidenceHigh || value == ConfidenceMedium || value == ConfidenceLow
}

func validCoverageStatus(value CoverageStatus) bool {
	return value == CoverageComplete || value == CoveragePartial || value == CoverageUnknown
}

func validSeverity(value Severity) bool {
	return value == SeverityBlocker || value == SeverityHigh || value == SeverityMedium || value == SeverityLow || value == SeverityNote
}

func validCategory(value Category) bool {
	return value == CategoryCorrectness || value == CategorySecurity || value == CategoryPerformance || value == CategoryTesting || value == CategoryMaintainability || value == CategoryOther
}

func validSide(value Side) bool { return value == SideLeft || value == SideRight }

func validPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && !strings.Contains(value, "\x00") && !strings.Contains(value, "../") && value != ".."
}

func validSHA(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit
}

func validOptionalText(value string) bool { return value == "" || validText(value, maxTextBytes) }
