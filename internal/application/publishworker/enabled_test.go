package publishworker

import (
	"testing"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
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
