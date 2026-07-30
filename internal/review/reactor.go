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

	if err := r.db.CancelActiveReviewRequest(id, db.ReviewRequestStatusSuppressed); err != nil {
		log.Printf("reactor: suppress request %d failed: %v", id, err)
		return err
	}
	r.cancelActive(id)
	log.Printf("reactor: request %d suppressed for commit %s", id, rr.CommitSHA)
	return nil
}

func (r *Reactor) CancelSystemRequest(id int64) {
	r.cancelActive(id)
}

func (r *Reactor) cancelActive(id int64) {
	r.cancelMu.Lock()
	cancel := r.activeCancel
	match := r.activeReqID == id && cancel != nil
	r.cancelMu.Unlock()
	if match {
		cancel()
	}
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

		claimed, err := r.db.ClaimReviewRequest(rr.ID)
		if err != nil {
			log.Printf("reactor: failed to claim request %d: %v", rr.ID, err)
			continue
		}
		if !claimed {
			continue
		}

		eligible, err := r.db.ReviewRequestEligible(rr.ID)
		if err != nil {
			log.Printf("reactor: failed to validate request %d: %v", rr.ID, err)
			r.markFailed(rr.ID)
			continue
		}
		if !eligible {
			if err := r.db.CancelActiveReviewRequest(rr.ID, db.ReviewRequestStatusCanceled); err != nil && !errors.Is(err, db.ErrNotFound) {
				log.Printf("reactor: failed to cancel ineligible request %d: %v", rr.ID, err)
			}
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
		pr.CommitSHA = rr.CommitSHA

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
			request, getErr := r.db.GetReviewRequest(rr.ID)
			if getErr == nil && request != nil {
				if cancelErr := r.db.CancelActiveReviewRequest(rr.ID, db.ReviewRequestStatusCanceled); cancelErr != nil && !errors.Is(cancelErr, db.ErrNotFound) {
					log.Printf("reactor: failed to cancel request %d: %v", rr.ID, cancelErr)
				}
			}
			r.emit(ReviewEvent{Type: EventReviewCancel, PR: *pr, Message: "canceled"})
			continue
		}

		if err != nil {
			log.Printf("reactor: review failed for PR %s#%d: %v", pr.Repo, pr.PRNumber, err)
			review := &db.Review{
				PullRequestID:   pr.ID,
				ReviewRequestID: rr.ID,
				Outcome:         db.ReviewOutcomeToolFailed,
				CommitSHA:       rr.CommitSHA,
				Summary:         err.Error(),
			}
			reviewID, saved, dbErr := r.db.SaveReviewResult(
				rr.ID, *review, nil, db.ReviewRequestStatusFailed,
			)
			if dbErr != nil {
				log.Printf("reactor: failed to save failed review: %v", dbErr)
			}
			if !saved {
				r.emit(ReviewEvent{Type: EventReviewCancel, PR: *pr, Message: "stale"})
				continue
			}
			review.ID = reviewID
			r.emit(ReviewEvent{Type: EventReviewFail, PR: *pr, Review: review, Message: err.Error()})
			continue
		}

		review := result.Review
		review.PullRequestID = pr.ID
		review.ReviewRequestID = rr.ID
		review.CommitSHA = rr.CommitSHA
		comments := make([]db.ReviewComment, 0, len(result.Comments))
		for _, comment := range result.Comments {
			comments = append(comments, db.ReviewComment{
				File: comment.File, Line: comment.Line, Message: comment.Message,
			})
		}
		reviewID, saved, err := r.db.SaveReviewResult(
			rr.ID, *review, comments, db.ReviewRequestStatusDone,
		)
		if err != nil {
			log.Printf("reactor: failed to save review: %v", err)
			r.markFailed(rr.ID)
			continue
		}
		if !saved {
			r.emit(ReviewEvent{Type: EventReviewCancel, PR: *pr, Message: "stale"})
			continue
		}
		review.ID = reviewID

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
