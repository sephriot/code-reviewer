package assessment

import (
	"strings"
	"testing"
)

func TestValidateAcceptsStrictV1Assessment(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	raw := []byte(`{
"version":1,
"verdict":"concerns",
"summary":"A nil check is missing.",
"confidence":"high",
"limitations":["Generated files were not reviewed."],
"coverage":{"status":"partial","changed_files_total":2,"reviewed_files":1,"omitted":[{"path":"generated.lock","reason":"generated file"}]},
"findings":[{"client_id":"nil-guard","severity":"high","category":"correctness","message":"Dereference can panic.","evidence":"Input is optional.","suggestion":"Check before use.","anchor":{"path":"internal/service.go","start_line":18,"end_line":20,"side":"RIGHT","sha":"` + head + `"}}]
}`)

	result, err := Validate(raw, RevisionEvidence{
		HeadSHA: head,
		BaseSHA: base,
		Files: []FileEvidence{
			{Path: "internal/service.go", Right: []LineRange{{Start: 1, End: 40}}},
			{Path: "generated.lock", Right: []LineRange{{Start: 1, End: 2}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assessment.Version != Version1 || result.Assessment.Findings[0].Anchor == nil {
		t.Fatalf("unexpected assessment: %#v", result.Assessment)
	}
	if got := result.Assessment.Findings[0].Anchor.SHA; got != head {
		t.Fatalf("anchor SHA = %q, want %q", got, head)
	}
	if len(result.ValidationWarnings) != 0 {
		t.Fatalf("unexpected validation warnings: %#v", result.ValidationWarnings)
	}
}

func TestValidateRejectsMalformedAndUnknownJSON(t *testing.T) {
	evidence := testEvidence()
	for name, raw := range map[string]string{
		"malformed": `{"version":`,
		"trailing":  validAssessmentJSON() + ` true`,
		"unknown":   strings.Replace(validAssessmentJSON(), `"summary"`, `"unknown":true,"summary"`, 1),
		"duplicate": strings.Replace(validAssessmentJSON(), `"summary":"Looks good."`, `"summary":"Looks good.","summary":"twice"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate([]byte(raw), evidence); err == nil {
				t.Fatalf("Validate accepted %s JSON", name)
			}
		})
	}
}

func TestValidateRejectsUnsupportedEnumsAndVersions(t *testing.T) {
	evidence := testEvidence()
	tests := map[string]string{
		"version":    strings.Replace(validAssessmentJSON(), `"version":1`, `"version":2`, 1),
		"verdict":    strings.Replace(validAssessmentJSON(), `"verdict":"pass"`, `"verdict":"approve"`, 1),
		"confidence": strings.Replace(validAssessmentJSON(), `"confidence":"high"`, `"confidence":"certain"`, 1),
		"coverage":   strings.Replace(validAssessmentJSON(), `"status":"complete"`, `"status":"all"`, 1),
		"severity":   strings.Replace(validAssessmentJSON(), `"severity":"low"`, `"severity":"urgent"`, 1),
		"category":   strings.Replace(validAssessmentJSON(), `"category":"testing"`, `"category":"style"`, 1),
		"side":       strings.Replace(validAssessmentJSON(), `"side":"RIGHT"`, `"side":"MIDDLE"`, 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate([]byte(raw), evidence); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestValidateRejectsBadSHAPathsAndAnchorRanges(t *testing.T) {
	evidence := testEvidence()
	head := evidence.HeadSHA
	tests := map[string]string{
		"bad evidence SHA": strings.Replace(validAssessmentJSON(), head, strings.Repeat("A", 40), 1),
		"bad anchor SHA":   strings.Replace(validAssessmentJSON(), `"sha":"`+head+`"`, `"sha":"`+strings.Repeat("c", 40)+`"`, 1),
		"unknown path":     strings.Replace(validAssessmentJSON(), `"internal/service.go"`, `"../secret.go"`, 1),
		"bad path":         strings.Replace(validAssessmentJSON(), `"internal/service.go"`, `"internal\\service.go"`, 1),
		"outside range":    strings.Replace(validAssessmentJSON(), `"end_line":20`, `"end_line":41`, 1),
		"reversed range":   strings.Replace(validAssessmentJSON(), `"start_line":18,"end_line":20`, `"start_line":21,"end_line":20`, 1),
		"wrong side SHA":   strings.Replace(validAssessmentJSON(), `"side":"RIGHT","sha":"`+head+`"`, `"side":"LEFT","sha":"`+head+`"`, 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate([]byte(raw), evidence); err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
		})
	}
}

func TestValidateRejectsInvalidEvidence(t *testing.T) {
	raw := []byte(validAssessmentJSON())
	for name, evidence := range map[string]RevisionEvidence{
		"missing head": {BaseSHA: strings.Repeat("b", 40)},
		"bad base":     {HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("B", 40)},
		"bad path": {
			HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
			Files: []FileEvidence{{Path: "../secret.go", Right: []LineRange{{Start: 1, End: 2}}}},
		},
		"bad range": {
			HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
			Files: []FileEvidence{{Path: "internal/service.go", Right: []LineRange{{Start: 2, End: 1}}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate(raw, evidence); err == nil {
				t.Fatalf("Validate accepted %s evidence", name)
			}
		})
	}
}

func testEvidence() RevisionEvidence {
	return RevisionEvidence{
		HeadSHA: strings.Repeat("a", 40),
		BaseSHA: strings.Repeat("b", 40),
		Files: []FileEvidence{
			{Path: "internal/service.go", Left: []LineRange{{Start: 1, End: 30}}, Right: []LineRange{{Start: 1, End: 40}}},
			{Path: "generated.lock", Right: []LineRange{{Start: 1, End: 2}}},
		},
	}
}

func validAssessmentJSON() string {
	head := strings.Repeat("a", 40)
	return `{"version":1,"verdict":"pass","summary":"Looks good.","confidence":"high","limitations":[],"coverage":{"status":"complete","changed_files_total":1,"reviewed_files":1,"omitted":[]},"findings":[{"client_id":"test-coverage","severity":"low","category":"testing","message":"Add one boundary test.","anchor":{"path":"internal/service.go","start_line":18,"end_line":20,"side":"RIGHT","sha":"` + head + `"}}]}`
}
