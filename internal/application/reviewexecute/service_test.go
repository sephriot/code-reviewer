package reviewexecute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/reviewbundle"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestExecuteRebuildsEvidenceBeforeEngineAndRecordsValidatedAssessment(t *testing.T) {
	var calls []string
	target := testExecutionTarget()
	bundle := reviewbundle.Result{
		Bundle:   json.RawMessage(`{"version":1,"review":"bounded"}`),
		Evidence: assessment.RevisionEvidence{HeadSHA: target.Canonical.HeadSHA, BaseSHA: target.Canonical.BaseSHA},
	}
	loader := targetLoaderFunc(func(_ context.Context, runID string) (sqlite.ReviewRunExecutionTarget, error) {
		calls = append(calls, "load")
		if runID != target.RunID {
			t.Fatalf("runID=%q", runID)
		}
		return target, nil
	})
	service := Service{
		Targets: loader,
		NewReader: func(_ context.Context, connectionID string) (reviewbundle.Reader, error) {
			calls = append(calls, "reader")
			if connectionID != target.ConnectionID {
				t.Fatalf("connectionID=%q", connectionID)
			}
			return readerStub{}, nil
		},
		Builder: bundleBuilderFunc(func(_ context.Context, _ reviewbundle.Reader, input reviewbundle.Input) (reviewbundle.Result, error) {
			calls = append(calls, "bundle")
			if input.Target.ConnectionID != target.Canonical.ConnectionID ||
				input.Profile.ProfileVersionID != target.Profile.ProfileVersionID ||
				input.Coordinate != (reviewbundle.Coordinate{Owner: target.Owner, Repository: target.Repository, Number: target.Number}) {
				t.Fatalf("input=%+v", input)
			}
			return bundle, nil
		}),
		Engine: engineAdapterFunc(func(_ context.Context, input json.RawMessage) (engine.Result, error) {
			calls = append(calls, "engine")
			if string(input) != string(bundle.Bundle) {
				t.Fatalf("engine input=%s", input)
			}
			return engine.Result{Stdout: validEngineAssessment()}, nil
		}),
		Recorder: assessmentRecorderFunc(func(_ context.Context, input sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error) {
			calls = append(calls, "record")
			if input.RunID != target.RunID || input.Result.Assessment.Verdict != assessment.VerdictPass {
				t.Fatalf("record input=%+v", input)
			}
			return sqlite.RecordAssessmentResult{AssessmentID: "assessment-1", Created: true}, nil
		}),
		Now: func() time.Time { return time.Unix(10, 0).UTC() },
	}

	result, err := service.Execute(context.Background(), target.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recorded.AssessmentID != "assessment-1" || result.Assessment.Assessment.Version != assessment.Version1 {
		t.Fatalf("result=%+v", result)
	}
	if got, want := strings.Join(calls, ","), "load,reader,bundle,engine,record"; got != want {
		t.Fatalf("calls=%s want=%s", got, want)
	}
}

func TestExecuteStopsBeforeEngineWhenEvidenceCannotBeRebuilt(t *testing.T) {
	target := testExecutionTarget()
	engineCalled, recorderCalled := false, false
	service := Service{
		Targets:   targetLoaderFunc(func(context.Context, string) (sqlite.ReviewRunExecutionTarget, error) { return target, nil }),
		NewReader: func(context.Context, string) (reviewbundle.Reader, error) { return readerStub{}, nil },
		Builder: bundleBuilderFunc(func(context.Context, reviewbundle.Reader, reviewbundle.Input) (reviewbundle.Result, error) {
			return reviewbundle.Result{}, errors.New("head changed")
		}),
		Engine: engineAdapterFunc(func(context.Context, json.RawMessage) (engine.Result, error) {
			engineCalled = true
			return engine.Result{}, nil
		}),
		Recorder: assessmentRecorderFunc(func(context.Context, sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error) {
			recorderCalled = true
			return sqlite.RecordAssessmentResult{}, nil
		}),
	}
	_, err := service.Execute(context.Background(), target.RunID)
	if err == nil || !strings.Contains(err.Error(), "rebuild review evidence") || engineCalled || recorderCalled {
		t.Fatalf("err=%v engine=%t recorder=%t", err, engineCalled, recorderCalled)
	}
}

func TestExecuteRejectsInvalidEngineAssessmentBeforeRecord(t *testing.T) {
	target := testExecutionTarget()
	recorderCalled := false
	service := Service{
		Targets:   targetLoaderFunc(func(context.Context, string) (sqlite.ReviewRunExecutionTarget, error) { return target, nil }),
		NewReader: func(context.Context, string) (reviewbundle.Reader, error) { return readerStub{}, nil },
		Builder: bundleBuilderFunc(func(context.Context, reviewbundle.Reader, reviewbundle.Input) (reviewbundle.Result, error) {
			return reviewbundle.Result{Bundle: json.RawMessage(`{"version":1}`), Evidence: assessment.RevisionEvidence{HeadSHA: target.Canonical.HeadSHA, BaseSHA: target.Canonical.BaseSHA}}, nil
		}),
		Engine: engineAdapterFunc(func(context.Context, json.RawMessage) (engine.Result, error) {
			return engine.Result{Stdout: []byte(`{"version":2}`)}, nil
		}),
		Recorder: assessmentRecorderFunc(func(context.Context, sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error) {
			recorderCalled = true
			return sqlite.RecordAssessmentResult{}, nil
		}),
	}
	_, err := service.Execute(context.Background(), target.RunID)
	if err == nil || !strings.Contains(err.Error(), "validate review engine assessment") || recorderCalled {
		t.Fatalf("err=%v recorder=%t", err, recorderCalled)
	}
}

func TestExecuteRejectsMismatchedTargetBeforeReader(t *testing.T) {
	readerCalled := false
	service := Service{
		Targets: targetLoaderFunc(func(context.Context, string) (sqlite.ReviewRunExecutionTarget, error) {
			return ExecutionTarget{RunID: "other"}, nil
		}),
		NewReader: func(context.Context, string) (reviewbundle.Reader, error) {
			readerCalled = true
			return readerStub{}, nil
		},
		Engine: engineAdapterFunc(func(context.Context, json.RawMessage) (engine.Result, error) { return engine.Result{}, nil }),
		Recorder: assessmentRecorderFunc(func(context.Context, sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error) {
			return sqlite.RecordAssessmentResult{}, nil
		}),
	}
	_, err := service.Execute(context.Background(), "run-1")
	if err == nil || !strings.Contains(err.Error(), "does not match") || readerCalled {
		t.Fatalf("err=%v reader=%t", err, readerCalled)
	}
}

func testExecutionTarget() ExecutionTarget {
	return ExecutionTarget{
		RunID: "run-1", ConnectionID: "connection-1", Owner: "owner", Repository: "repo", Number: 1,
		Canonical: sqlite.CanonicalReviewTarget{ConnectionID: "connection-1", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)},
		Profile:   sqlite.ReviewExecutionProfile{ProfileID: "profile-1", ProfileVersionID: "profile-version-1", Name: "Default", Instructions: "Review."},
	}
}

func validEngineAssessment() []byte {
	return []byte(`{"version":1,"verdict":"pass","summary":"Looks good.","confidence":"high","limitations":[],"coverage":{"status":"complete","changed_files_total":0,"reviewed_files":0,"omitted":[]},"findings":[]}`)
}

type targetLoaderFunc func(context.Context, string) (sqlite.ReviewRunExecutionTarget, error)

func (f targetLoaderFunc) LoadReviewRunExecutionTarget(ctx context.Context, runID string) (sqlite.ReviewRunExecutionTarget, error) {
	return f(ctx, runID)
}

type bundleBuilderFunc func(context.Context, reviewbundle.Reader, reviewbundle.Input) (reviewbundle.Result, error)

func (f bundleBuilderFunc) Build(ctx context.Context, reader reviewbundle.Reader, input reviewbundle.Input) (reviewbundle.Result, error) {
	return f(ctx, reader, input)
}

type engineAdapterFunc func(context.Context, json.RawMessage) (engine.Result, error)

func (f engineAdapterFunc) Review(ctx context.Context, input json.RawMessage) (engine.Result, error) {
	return f(ctx, input)
}

type assessmentRecorderFunc func(context.Context, sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error)

func (f assessmentRecorderFunc) RecordAssessment(ctx context.Context, input sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error) {
	return f(ctx, input)
}

type readerStub struct{}

func (readerStub) GetPullRequest(context.Context, string, string, int, string) (github.PullRequestResult, error) {
	return github.PullRequestResult{}, errors.New("unexpected GitHub read")
}

func (readerStub) GetPullRequestDiff(context.Context, string, string, int, string) (github.PullRequestDiffResult, error) {
	return github.PullRequestDiffResult{}, errors.New("unexpected GitHub read")
}

func (readerStub) GetPullRequestFiles(context.Context, string, string, int, int) (github.PullRequestFilesPage, error) {
	return github.PullRequestFilesPage{}, errors.New("unexpected GitHub read")
}

func (readerStub) GetGitTree(context.Context, string, string, string) (github.GitTreeResult, error) {
	return github.GitTreeResult{}, errors.New("unexpected GitHub read")
}
