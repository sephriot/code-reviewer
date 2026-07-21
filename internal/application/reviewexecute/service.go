// Package reviewexecute runs one prepared review against freshly rebuilt
// read-only evidence. It owns no job, publication, or GitHub write capability.
package reviewexecute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/reviewbundle"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// ExecutionTarget is the immutable input for one prepared run. It is a
// read-only SQLite projection; the service maps it into the bounded bundle.
type ExecutionTarget = sqlite.ReviewRunExecutionTarget

// TargetLoader returns the immutable facts selected for a prepared run.
type TargetLoader interface {
	LoadReviewRunExecutionTarget(context.Context, string) (sqlite.ReviewRunExecutionTarget, error)
}

// ReaderFactory resolves a connection to a GET-only GitHub reader. It must
// never return a reader with GitHub publication capability.
type ReaderFactory func(context.Context, string) (reviewbundle.Reader, error)

// BundleBuilder independently rebuilds canonical evidence immediately before
// engine execution.
type BundleBuilder interface {
	Build(context.Context, reviewbundle.Reader, reviewbundle.Input) (reviewbundle.Result, error)
}

type standardBundleBuilder struct{}

func (standardBundleBuilder) Build(ctx context.Context, reader reviewbundle.Reader, input reviewbundle.Input) (reviewbundle.Result, error) {
	return reviewbundle.Build(ctx, reader, input)
}

// AssessmentRecorder persists only a previously validated assessment.
type AssessmentRecorder interface {
	RecordAssessment(context.Context, sqlite.RecordAssessmentInput) (sqlite.RecordAssessmentResult, error)
}

// Service executes the bounded, evidence-verified review path for one run.
type Service struct {
	Targets   TargetLoader
	NewReader ReaderFactory
	Builder   BundleBuilder
	Engine    engine.Adapter
	Recorder  AssessmentRecorder
	Now       func() time.Time
}

// Result contains the immutable and bounded artifacts produced during an
// execution. It does not contain any GitHub publication result.
type Result struct {
	Target     ExecutionTarget
	Bundle     reviewbundle.Result
	Engine     engine.Result
	Assessment assessment.Result
	Recorded   sqlite.RecordAssessmentResult
}

// Execute loads a prepared run, rebuilds canonical evidence using GET-only
// reads, invokes the bounded engine, validates its output against the rebuilt
// evidence, then records the immutable assessment.
func (s Service) Execute(ctx context.Context, runID string) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("review execution context is required")
	}
	if strings.TrimSpace(runID) == "" {
		return Result{}, errors.New("review execution run ID is required")
	}
	if s.Targets == nil || s.NewReader == nil || s.Engine == nil || s.Recorder == nil {
		return Result{}, errors.New("review execution dependencies are required")
	}
	builder := s.Builder
	if builder == nil {
		builder = standardBundleBuilder{}
	}

	target, err := s.Targets.LoadReviewRunExecutionTarget(ctx, runID)
	if err != nil {
		return Result{}, fmt.Errorf("load review execution target: %w", err)
	}
	if target.RunID != runID || strings.TrimSpace(target.ConnectionID) == "" {
		return Result{}, errors.New("review execution target does not match requested run")
	}
	reader, err := s.NewReader(ctx, target.ConnectionID)
	if err != nil {
		return Result{}, fmt.Errorf("create review evidence reader: %w", err)
	}
	bundle, err := builder.Build(ctx, reader, reviewbundle.Input{
		Target: target.Canonical,
		Profile: reviewbundle.VerifiedProfile{
			ProfileID: target.Profile.ProfileID, ProfileVersionID: target.Profile.ProfileVersionID,
			Name: target.Profile.Name, Description: target.Profile.Description,
			Instructions: target.Profile.Instructions, SettingsJSON: target.Profile.SettingsJSON,
		},
		Coordinate: reviewbundle.Coordinate{Owner: target.Owner, Repository: target.Repository, Number: target.Number},
	})
	if err != nil {
		return Result{}, fmt.Errorf("rebuild review evidence: %w", err)
	}
	engineResult, err := s.Engine.Review(ctx, bundle.Bundle)
	if err != nil {
		return Result{}, fmt.Errorf("run review engine: %w", err)
	}
	validated, err := assessment.Validate(engineResult.Stdout, bundle.Evidence)
	if err != nil {
		return Result{}, fmt.Errorf("validate review engine assessment: %w", err)
	}
	recordedAt := time.Now().UTC()
	if s.Now != nil {
		recordedAt = s.Now().UTC()
	}
	recorded, err := s.Recorder.RecordAssessment(ctx, sqlite.RecordAssessmentInput{
		RunID: runID, Result: validated, RecordedAt: recordedAt,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record review assessment: %w", err)
	}
	return Result{Target: target, Bundle: bundle, Engine: engineResult, Assessment: validated, Recorded: recorded}, nil
}
