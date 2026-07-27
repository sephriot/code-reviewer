package review

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type Reactor struct {
	cfg     *config.Config
	db      *db.DB
	gh      *gh.Client
	runner  *Runner
	onEvent func(event ReviewEvent)
	mu      sync.Mutex

	runReview func(ctx context.Context, pr db.PullRequest, promptPath string) (*ReviewResult, error)

	cancelMu     sync.Mutex
	activeCancel context.CancelFunc
	activeReqID  int64
}

type ReviewEvent struct {
	Type    string
	PR      db.PullRequest
	Review  *db.Review
	Message string
}

const (
	EventReviewStart       = "review_start"
	EventReviewSuccess     = "review_success"
	EventReviewFail        = "review_fail"
	EventReviewCancel      = "review_cancel"
	EventHumanReviewNeeded = "human_review_needed"
	EventChangesRequested  = "changes_requested"
)

func NewReactor(cfg *config.Config, d *db.DB, gh *gh.Client, runner *Runner, onEvent func(ReviewEvent)) *Reactor {
	return &Reactor{cfg: cfg, db: d, gh: gh, runner: runner, onEvent: onEvent}
}

func (r *Reactor) CancelRequest(id int64) error {
	rr, err := r.db.GetReviewRequest(id)
	if err != nil {
		return err
	}
	if rr == nil {
		log.Printf("reactor: cancel request %d: not found", id)
		return db.ErrNotFound
	}

	r.cancelMu.Lock()
	cancel := r.activeCancel
	match := r.activeReqID == id && cancel != nil
	r.cancelMu.Unlock()

	if match {
		log.Printf("reactor: canceling in-progress request %d (pr_id=%d)", id, rr.PullRequestID)
		cancel()
	} else {
		log.Printf("reactor: removing queue request %d (pr_id=%d, status=%s)", id, rr.PullRequestID, rr.Status)
	}

	if err := r.db.SoftDeleteReviewRequest(id); err != nil {
		log.Printf("reactor: soft-delete request %d failed: %v", id, err)
		return err
	}
	log.Printf("reactor: request %d removed from queue", id)
	return nil
}

func (r *Reactor) setActive(id int64, cancel context.CancelFunc) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	r.activeReqID = id
	r.activeCancel = cancel
}

func (r *Reactor) clearActive(id int64) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.activeReqID == id {
		r.activeReqID = 0
		r.activeCancel = nil
	}
}

func (r *Reactor) doRunReview(ctx context.Context, pr db.PullRequest, promptPath string) (*ReviewResult, error) {
	if r.runReview != nil {
		return r.runReview(ctx, pr, promptPath)
	}
	return r.runner.RunReview(ctx, pr, promptPath)
}

func (r *Reactor) ProcessQueue(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n, err := r.db.ResetStaleReviewRequests(r.cfg.ReviewTimeout); err != nil {
		log.Printf("reactor: stale reset error: %v", err)
	} else if n > 0 {
		log.Printf("reactor: reset %d stale in_progress requests to pending", n)
	}

	for {
		rr, err := r.db.GetNextPendingReviewRequest()
		if err != nil {
			return err
		}
		if rr == nil {
			return nil
		}

		if err := r.db.UpdateReviewRequestStatus(rr.ID, "in_progress"); err != nil {
			log.Printf("reactor: failed to mark request %d as in_progress: %v", rr.ID, err)
			continue
		}

		pr, err := r.db.GetPR(rr.PullRequestID)
		if err != nil {
			log.Printf("reactor: failed to get PR %d: %v", rr.PullRequestID, err)
			r.markFailed(rr.ID)
			continue
		}
		if pr == nil {
			r.markFailed(rr.ID)
			continue
		}

		r.emit(ReviewEvent{Type: EventReviewStart, PR: *pr})

		promptPath := r.cfg.ReviewPromptPath
		if promptPath == "" {
			promptPath = "prompts/review_prompt.txt"
		}

		reviewCtx, cancel := context.WithCancel(ctx)
		r.setActive(rr.ID, cancel)
		result, err := r.doRunReview(reviewCtx, *pr, promptPath)
		cancel()
		r.clearActive(rr.ID)

		if errors.Is(err, context.Canceled) {
			log.Printf("reactor: review canceled for PR %s#%d", pr.Repo, pr.PRNumber)
			if delErr := r.db.SoftDeleteReviewRequest(rr.ID); delErr != nil && !errors.Is(delErr, db.ErrNotFound) {
				log.Printf("reactor: failed to soft-delete canceled request %d: %v", rr.ID, delErr)
			}
			r.emit(ReviewEvent{Type: EventReviewCancel, PR: *pr, Message: "canceled"})
			continue
		}

		if err != nil {
			log.Printf("reactor: review failed for PR %s#%d: %v", pr.Repo, pr.PRNumber, err)
			review := &db.Review{
				PullRequestID:   pr.ID,
				ReviewRequestID: rr.ID,
				Outcome:         "tool_failed",
				Summary:         err.Error(),
			}
			reviewID, dbErr := r.db.CreateReview(*review)
			if dbErr != nil {
				log.Printf("reactor: failed to save failed review: %v", dbErr)
			}
			review.ID = reviewID
			r.markDone(rr.ID)
			r.emit(ReviewEvent{Type: EventReviewFail, PR: *pr, Review: review, Message: err.Error()})
			continue
		}

		review := result.Review
		review.ReviewRequestID = rr.ID
		reviewID, err := r.db.CreateReview(*review)
		if err != nil {
			log.Printf("reactor: failed to save review: %v", err)
			r.markFailed(rr.ID)
			continue
		}
		review.ID = reviewID

		for _, tc := range result.Comments {
			_, cerr := r.db.AddReviewComment(db.ReviewComment{
				ReviewID: review.ID,
				File:     tc.File,
				Line:     tc.Line,
				Message:  tc.Message,
			})
			if cerr != nil {
				log.Printf("reactor: failed to save comment: %v", cerr)
			}
		}

		if err := r.db.SetPRNeedsReview(pr.ID, false); err != nil {
			log.Printf("reactor: failed to mark PR as reviewed: %v", err)
		}

		r.markDone(rr.ID)

		eventType := EventReviewSuccess
		msg := ""
		switch review.Outcome {
		case db.ReviewOutcomeHumanReview:
			eventType = EventHumanReviewNeeded
			msg = "human review required"
		case db.ReviewOutcomeChangesRequested:
			eventType = EventChangesRequested
			msg = "changes requested"
		case db.ReviewOutcomeToolFailed:
			eventType = EventReviewFail
			msg = "tool returned unrecognizable output"
		}
		r.emit(ReviewEvent{Type: eventType, PR: *pr, Review: review, Message: msg})
	}
}

func (r *Reactor) markDone(id int64) {
	if err := r.db.UpdateReviewRequestStatus(id, "done"); err != nil {
		log.Printf("reactor: failed to mark request %d as done: %v", id, err)
	}
}

func (r *Reactor) markFailed(id int64) {
	if err := r.db.UpdateReviewRequestStatus(id, "failed"); err != nil {
		log.Printf("reactor: failed to mark request %d as failed: %v", id, err)
	}
}

func (r *Reactor) emit(event ReviewEvent) {
	if r.onEvent != nil {
		r.onEvent(event)
	}
}
