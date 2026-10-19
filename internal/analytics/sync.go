package analytics

import (
	"context"
	"log"
	"time"

	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type githubAPI interface {
	WalkTimingPRs(context.Context, string, time.Time, time.Time, *time.Time, func(gh.TimingCandidate) error) error
	FirstReviewTime(context.Context, gh.TimingCandidate, string) (*time.Time, error)
}

type Syncer struct {
	db       *db.DB
	gh       githubAPI
	username string
}

func New(database *db.DB, client githubAPI, username string) *Syncer {
	return &Syncer{db: database, gh: client, username: username}
}

// Run refreshes daily and retries interrupted backfills hourly without overlapping runs.
func (s *Syncer) Run(ctx context.Context) {
	for {
		if err := s.Sync(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("analytics: sync error: %v", err)
		}
		timer := time.NewTimer(time.Hour)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Syncer) Sync(ctx context.Context, now time.Time) (syncErr error) {
	state, err := s.db.TimingSyncState(s.username, now)
	if err != nil {
		return err
	}
	defer func() {
		if syncErr != nil {
			state.LastError = syncErr.Error()
			if err := s.db.SaveTimingSyncState(s.username, state); err != nil {
				log.Printf("analytics: save failure status: %v", err)
			}
		}
	}()
	if state.LastSyncedAt != nil && now.Sub(*state.LastSyncedAt) < 24*time.Hour && !state.BackfillBefore.After(state.BackfillSince) {
		return nil
	}
	log.Println("analytics: syncing GitHub review timing")
	for state.BackfillBefore.After(state.BackfillSince) {
		start := state.BackfillBefore.AddDate(0, 0, -30)
		if start.Before(state.BackfillSince) {
			start = state.BackfillSince
		}
		if err := s.syncRange(ctx, start, state.BackfillBefore, nil); err != nil {
			return err
		}
		state.BackfillBefore = start
		state.LastError = ""
		if err := s.db.SaveTimingSyncState(s.username, state); err != nil {
			return err
		}
		log.Printf("analytics: backfilled review timing through %s", start.Format("2006-01-02"))
	}
	var updated *time.Time
	if state.LastSyncedAt != nil {
		t := state.LastSyncedAt.Add(-24 * time.Hour)
		updated = &t
	}
	since := now.UTC().AddDate(0, 0, -365)
	if err := s.syncRange(ctx, since, now.UTC().Truncate(24*time.Hour).Add(24*time.Hour), updated); err != nil {
		return err
	}
	state.LastSyncedAt = &now
	state.LastError = ""
	if err := s.db.SaveTimingSyncState(s.username, state); err != nil {
		return err
	}
	log.Println("analytics: review timing sync complete")
	return nil
}

func (s *Syncer) syncRange(ctx context.Context, since, before time.Time, updated *time.Time) error {
	for _, direction := range []string{"given", "received"} {
		err := s.gh.WalkTimingPRs(ctx, direction, since, before, updated, func(pr gh.TimingCandidate) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			repo := pr.Owner + "/" + pr.Repo
			current, err := s.db.ReviewTimingCurrent(s.username, repo, pr.Number, direction, pr.UpdatedAt)
			if err != nil {
				return err
			}
			if current {
				return nil
			}
			first, err := s.gh.FirstReviewTime(ctx, pr, direction)
			if err != nil {
				return err
			}
			if direction == "given" && first == nil {
				return nil
			}
			return s.db.UpsertReviewTiming(s.username, db.ReviewTiming{Repo: repo, PRNumber: pr.Number, Direction: direction, OpenedAt: pr.CreatedAt, ReviewedAt: first, UpdatedAt: pr.UpdatedAt})
		})
		if err != nil {
			return err
		}
	}
	return nil
}
