package policy

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
)

func TestEvaluateUsesDefaultsAndRendersStableProposal(t *testing.T) {
	input := validInput()
	input.Assessment.Assessment.Verdict = assessment.VerdictConcerns
	input.Assessment.Assessment.Findings = []assessment.Finding{
		{ClientID: "later", Severity: assessment.SeverityLow, Category: assessment.CategoryTesting, Message: "Add a boundary test."},
		{ClientID: "first", Severity: assessment.SeverityHigh, Category: assessment.CategoryCorrectness, Message: "Nil input can panic.", Anchor: &assessment.Anchor{Path: "internal/service.go", StartLine: 18, EndLine: 20, Side: assessment.SideRight, SHA: strings.Repeat("a", 40)}},
	}

	got, err := Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != DispositionProposeComment || got.Proposal == nil || got.Proposal.Kind != "comment" {
		t.Fatalf("unexpected result: %#v", got)
	}
	if len(got.Proposal.InlineComments) != 1 || got.Proposal.InlineComments[0].Body != "[high/correctness] Nil input can panic." {
		t.Fatalf("unexpected inline proposal: %#v", got.Proposal.InlineComments)
	}
	if want := "Review summary:\nLooks good.\n\nUnanchored findings:\n- [low/testing] Add a boundary test."; got.Proposal.Body != want {
		t.Fatalf("proposal body = %q, want %q", got.Proposal.Body, want)
	}

	again, err := Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("policy output is not deterministic:\nfirst=%#v\nagain=%#v", got, again)
	}
}

func TestEvaluateSafetyGates(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Input)
		want      Disposition
		overrides []SafetyOverride
		proposal  bool
	}{
		{
			name: "own PR blocks approval", mutate: func(input *Input) {
				input.Facts.AuthoredByMe = true
			}, want: DispositionNoExternalAction, overrides: []SafetyOverride{SafetyOverrideAuthoredPRBlocksApproval},
		},
		{
			name: "own PR blocks changes", mutate: func(input *Input) {
				input.Facts.AuthoredByMe = true
				input.Assessment.Assessment.Verdict = assessment.VerdictChangesRequired
			}, want: DispositionNoExternalAction, overrides: []SafetyOverride{SafetyOverrideAuthoredPRBlocksChanges},
		},
		{
			name: "partial coverage downgrades auto approval", mutate: func(input *Input) {
				input.Facts.Coverage.Status = assessment.CoveragePartial
				input.Assessment.Assessment.Coverage.Status = assessment.CoveragePartial
			}, want: DispositionProposeApproval, overrides: []SafetyOverride{SafetyOverrideCoverageBlocksAutoApproval}, proposal: true,
		},
		{
			name: "incomplete complete coverage downgrades auto approval", mutate: func(input *Input) {
				input.Facts.Coverage.ReviewedFiles = 0
				input.Assessment.Assessment.Coverage.ReviewedFiles = 0
			}, want: DispositionProposeApproval, overrides: []SafetyOverride{SafetyOverrideCoverageBlocksAutoApproval}, proposal: true,
		},
		{
			name: "note finding downgrades auto approval", mutate: func(input *Input) {
				input.Assessment.Assessment.Findings = []assessment.Finding{{ClientID: "note", Severity: assessment.SeverityNote, Category: assessment.CategoryOther, Message: "Optional cleanup."}}
			}, want: DispositionProposeApproval, overrides: []SafetyOverride{SafetyOverrideAutoApprovalRequirements}, proposal: true,
		},
		{
			name: "draft suppresses proposal", mutate: func(input *Input) {
				input.Facts.Draft = true
			}, want: DispositionNoExternalAction, overrides: []SafetyOverride{SafetyOverrideDraftPullRequest},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput()
			test.mutate(&input)
			got, err := Evaluate(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Disposition != test.want || !reflect.DeepEqual(got.SafetyOverrides, test.overrides) || (got.Proposal != nil) != test.proposal {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestEvaluateAutoApprovalHasEmptyApprovalArtifact(t *testing.T) {
	got, err := Evaluate(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != DispositionAutoPublishApproval || got.Proposal == nil {
		t.Fatalf("result = %#v", got)
	}
	if got.Proposal.Kind != "approval" || got.Proposal.Body != "" || len(got.Proposal.InlineComments) != 0 {
		t.Fatalf("unsafe automatic approval artifact: %#v", got.Proposal)
	}
}

func TestEvaluateNoActionAndHumanReviewDoNotCreateProposal(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no external action": func(input *Input) {
			input.Policy.Matrix[assessment.VerdictPass] = DispositionNoExternalAction
		},
		"human review": func(input *Input) {
			input.Assessment.Assessment.Verdict = assessment.VerdictInconclusive
			input.Policy.Matrix = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := validInput()
			mutate(&input)
			got, err := Evaluate(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Proposal != nil {
				t.Fatalf("unexpected proposal: %#v", got.Proposal)
			}
		})
	}
}

func TestEvaluateRejectsStaleAndTerminalFacts(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"stale":    func(input *Input) { input.Facts.EvidenceCurrent = false },
		"terminal": func(input *Input) { input.Facts.Terminal = true },
	} {
		t.Run(name, func(t *testing.T) {
			input := validInput()
			mutate(&input)
			_, err := Evaluate(input)
			if name == "stale" && !errors.Is(err, ErrStaleEvidence) {
				t.Fatalf("error = %v", err)
			}
			if name == "terminal" && !errors.Is(err, ErrTerminalPullRequest) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEvaluateRejectsMalformedPolicyInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{"coverage differs", func(input *Input) { input.Facts.Coverage.Status = assessment.CoveragePartial }},
		{"unsupported disposition", func(input *Input) { input.Policy.Matrix[assessment.VerdictPass] = "post_now" }},
		{"auto approval for concern", func(input *Input) { input.Policy.Matrix[assessment.VerdictConcerns] = DispositionAutoPublishApproval }},
		{"invalid assessment", func(input *Input) { input.Assessment.Assessment.Verdict = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput()
			test.mutate(&input)
			if _, err := Evaluate(input); err == nil {
				t.Fatal("Evaluate accepted malformed input")
			}
		})
	}
}

func validInput() Input {
	coverage := assessment.Coverage{Status: assessment.CoverageComplete, ChangedFilesTotal: 1, ReviewedFiles: 1, Omitted: []assessment.Omission{}}
	return Input{
		Assessment: assessment.Result{Assessment: assessment.Assessment{
			Version: assessment.Version1, Verdict: assessment.VerdictPass, Summary: "Looks good.", Confidence: assessment.ConfidenceHigh,
			Limitations: []string{}, Coverage: coverage, Findings: []assessment.Finding{},
		}, ValidationWarnings: []assessment.ValidationWarning{}},
		Facts:  Facts{EvidenceCurrent: true, Coverage: coverage},
		Policy: PublicationPolicy{AllowAutomaticApproval: true, Matrix: map[assessment.Verdict]Disposition{assessment.VerdictPass: DispositionAutoPublishApproval}},
	}
}
