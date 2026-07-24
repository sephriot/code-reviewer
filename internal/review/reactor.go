package review

import (
	"context"
	"log"
	"sync"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type Reactor struct {
	cfg    *config.Config
	db     *db.DB
	gh     *gh.Client
	runner *Runner
	onEvent func(event ReviewEvent)
	mu     sync.Mutex
}

type ReviewEvent struct {
	Type      string
	PR        db.PullRequest
	Review    *db.Review
	Message   string
}

const (
	EventReviewStart        = "review_start"
	EventReviewSuccess      = "review_success"
	EventReviewFail         = "review_fail"
	EventHumanReviewNeeded  = "human_review_needed"
)

func NewReactor(cfg *config.Config, d *db.DB, gh *gh.Client, runner *Runner, onEvent func(ReviewEvent)) *Reactor {
	return &Reactor{cfg: cfg, db: d, gh: gh, runner: runner, onEvent: onEvent}
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

		review, err := r.runner.RunReview(ctx, *pr, promptPath)
		if err != nil {
			log.Printf("reactor: review failed for PR %s#%d: %v", pr.Repo, pr.PRNumber, err)
			review = &db.Review{
				PullRequestID:  pr.ID,
				ReviewRequestID: rr.ID,
				Outcome:        "tool_failed",
				Summary:        err.Error(),
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

		review.ReviewRequestID = rr.ID
		reviewID, err := r.db.CreateReview(*review)
		if err != nil {
			log.Printf("reactor: failed to save review: %v", err)
			r.markFailed(rr.ID)
			continue
		}
		review.ID = reviewID

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
		case db.ReviewOutcomeToolFailed:
			eventType = EventReviewFail
			msg = "tool returned unrecognizable output"
		}
		r.emit(ReviewEvent{Type: eventType, PR: *pr, Review: review, Message: msg})
	}
}

func (r *Reactor) markDone(requestID int64) {
	if err := r.db.UpdateReviewRequestStatus(requestID, "done"); err != nil {
		log.Printf("reactor: failed to mark request %d as done: %v", requestID, err)
	}
}

func (r *Reactor) markFailed(requestID int64) {
	if err := r.db.UpdateReviewRequestStatus(requestID, "failed"); err != nil {
		log.Printf("reactor: failed to mark request %d as failed: %v", requestID, err)
	}
}

func (r *Reactor) emit(event ReviewEvent) {
	if r.onEvent != nil {
		r.onEvent(event)
	}
}
