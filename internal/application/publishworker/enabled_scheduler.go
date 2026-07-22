package publishworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// EnabledScheduler queues one explicitly requested external publication. Jobs
// have one attempt because a later failure may follow an external GitHub write.
type EnabledScheduler struct {
	Store SchedulerStore
	Now   func() time.Time
}

// Schedule returns the matching active dispatch job or creates one. The effect
// ID is the sole job payload; all external facts remain immutable in storage.
func (s EnabledScheduler) Schedule(ctx context.Context, effectID string) (sqlite.EnsureJobResult, error) {
	if s.Store == nil {
		return sqlite.EnsureJobResult{}, errors.New("enabled publication job store is required")
	}
	if effectID == "" || effectID != strings.TrimSpace(effectID) || len(effectID) > maxEffectIDBytes || !validEffectID(effectID) {
		return sqlite.EnsureJobResult{}, errors.New("enabled publication effect ID is invalid")
	}
	payload, err := json.Marshal(jobPayload{EffectID: effectID})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("encode enabled publication job payload: %w", err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	result, err := s.Store.EnsureJob(ctx, sqlite.JobInput{
		Kind:         EnabledJobKind,
		ResourceType: "publication_effect",
		ResourceID:   effectID,
		DedupeKey:    EnabledJobKind + ":" + effectID,
		Payload:      payload,
		AvailableAt:  now,
		MaxAttempts:  1,
	})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("ensure enabled publication job: %w", err)
	}
	return result, nil
}
