package reviewworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/reviewexecute"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestHandlerExecutesPreparedRun(t *testing.T) {
	var executed string
	events := &eventRecorder{}
	handler := Handler{
		Executor: executorFunc(func(_ context.Context, runID string) (reviewexecute.Result, error) {
			executed = runID
			return reviewexecute.Result{}, nil
		}),
		Events: events,
	}
	if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if executed != "run-1" || len(events.inputs) != 0 {
		t.Fatalf("executed=%q events=%+v", executed, events.inputs)
	}
}

func TestHandlerRejectsMalformedPayloadBeforeExecution(t *testing.T) {
	executed := false
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			executed = true
			return reviewexecute.Result{}, nil
		}),
		Events: &eventRecorder{},
	}
	for _, payload := range []string{
		`{}`, `{"run_id":"run-1","extra":true}`, `{"run_id":" run-1"}`, `{"run_id":"run/1"}`, `{"run_id":"run-1"} null`,
	} {
		err := handler.Handle(context.Background(), reviewJob(payload))
		if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	if executed {
		t.Fatal("executor called for malformed payload")
	}
}

func TestHandlerRecordsOnlySafeClassifiedFailures(t *testing.T) {
	testCases := []struct {
		name      string
		execution error
		kind      string
		code      string
		permanent bool
	}{
		{name: "engine", execution: fmtError(engine.ErrEngineExit), kind: sqlite.ReviewRunEventFailedRetryable, code: "engine_exit"},
		{name: "provider rate", execution: fmtError(&githubadapter.HTTPError{StatusCode: 429, Message: "token=never-store-this"}), kind: sqlite.ReviewRunEventFailedRetryable, code: "rate_limited"},
		{name: "stale", execution: fmtError(sqlite.ErrReviewRunExecutionTargetNotFound), kind: sqlite.ReviewRunEventFailedTerminal, code: "stale_evidence", permanent: true},
		{name: "invalid output", execution: errors.New("validate review engine assessment: raw stdout must not persist"), kind: sqlite.ReviewRunEventFailedTerminal, code: "validation_failed", permanent: true},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			events := &eventRecorder{}
			handler := Handler{
				Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
					return reviewexecute.Result{}, test.execution
				}),
				Events: events,
				Now:    func() time.Time { return time.Unix(7, 0).UTC() },
			}
			err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`))
			if err == nil || worker.IsPermanent(err) != test.permanent {
				t.Fatalf("err=%v permanent=%t", err, worker.IsPermanent(err))
			}
			if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "stdout") {
				t.Fatalf("unsafe returned error: %v", err)
			}
			if len(events.inputs) != 1 {
				t.Fatalf("events=%+v", events.inputs)
			}
			input := events.inputs[0]
			if input.RunID != "run-1" || input.EventKind != test.kind || input.DiagnosticCode != test.code || !input.OccurredAt.Equal(time.Unix(7, 0).UTC()) {
				t.Fatalf("input=%+v", input)
			}
		})
	}
}

func TestHandlerRetriesLifecycleWriteFailure(t *testing.T) {
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return reviewexecute.Result{}, engine.ErrEngineExit
		}),
		Events: &eventRecorder{err: errors.New("database offline")},
	}
	err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`))
	if err == nil || worker.IsPermanent(err) || err.Error() != "review run lifecycle recording failed" {
		t.Fatalf("err=%v", err)
	}
}

func reviewJob(payload string) sqlite.Job {
	return sqlite.Job{Kind: ExecuteJobKind, Payload: []byte(payload)}
}

type executorFunc func(context.Context, string) (reviewexecute.Result, error)

func (f executorFunc) Execute(ctx context.Context, runID string) (reviewexecute.Result, error) {
	return f(ctx, runID)
}

type eventRecorder struct {
	inputs []sqlite.AppendReviewRunEventInput
	err    error
}

func (r *eventRecorder) AppendReviewRunEvent(_ context.Context, input sqlite.AppendReviewRunEventInput) (sqlite.AppendReviewRunEventResult, error) {
	r.inputs = append(r.inputs, input)
	return sqlite.AppendReviewRunEventResult{}, r.err
}

func fmtError(err error) error { return errors.Join(errors.New("review execution failed"), err) }
