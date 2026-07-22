package sqlite

import (
	"context"
	"fmt"
)

// AnalyticsOverview contains durable control-plane totals. It deliberately
// reads immutable review, policy, proposal, decision, and publication facts
// instead of deriving metrics from mutable queues.
type AnalyticsOverview struct {
	ObservedPullRequests          int
	ReviewRuns                    int
	Assessments                   int
	PolicyEvaluations             int
	HumanReviewEvaluations        int
	Proposals                     int
	ProposalRevisions             int
	ProposalApprovals             int
	ProposalRejections            int
	PublicationEffects            int
	PublicationAttempts           int
	SimulatedPublicationAttempts  int
	SuccessfulPublicationAttempts int
	RetryablePublicationFailures  int
	TerminalPublicationFailures   int
	UncertainPublicationAttempts  int
}

// AnalyticsOverview returns whole-ledger totals for the initial analytics
// overview. It performs one SELECT and has no queue, GitHub, or mutation path.
func (s *Store) AnalyticsOverview(ctx context.Context) (AnalyticsOverview, error) {
	var overview AnalyticsOverview
	err := s.db.QueryRowContext(ctx, `
SELECT
 (SELECT COUNT(*) FROM pull_requests),
 (SELECT COUNT(*) FROM review_runs),
 (SELECT COUNT(*) FROM assessments),
 (SELECT COUNT(*) FROM policy_evaluations),
 (SELECT COUNT(*) FROM policy_evaluations WHERE disposition = 'require_human_review'),
 (SELECT COUNT(*) FROM proposals),
 (SELECT COUNT(*) FROM proposal_revisions),
 (SELECT COUNT(*) FROM decisions WHERE decision = 'approve'),
 (SELECT COUNT(*) FROM decisions WHERE decision = 'reject'),
 (SELECT COUNT(*) FROM publication_effects),
 (SELECT COUNT(*) FROM publication_attempts),
 (SELECT COUNT(*) FROM publication_attempts WHERE outcome = 'simulated'),
 (SELECT COUNT(*) FROM publication_attempts WHERE outcome = 'succeeded'),
 (SELECT COUNT(*) FROM publication_attempts WHERE outcome = 'failed_retryable'),
 (SELECT COUNT(*) FROM publication_attempts WHERE outcome = 'failed_terminal'),
 (SELECT COUNT(*) FROM publication_attempts WHERE outcome = 'uncertain')`).Scan(
		&overview.ObservedPullRequests,
		&overview.ReviewRuns,
		&overview.Assessments,
		&overview.PolicyEvaluations,
		&overview.HumanReviewEvaluations,
		&overview.Proposals,
		&overview.ProposalRevisions,
		&overview.ProposalApprovals,
		&overview.ProposalRejections,
		&overview.PublicationEffects,
		&overview.PublicationAttempts,
		&overview.SimulatedPublicationAttempts,
		&overview.SuccessfulPublicationAttempts,
		&overview.RetryablePublicationFailures,
		&overview.TerminalPublicationFailures,
		&overview.UncertainPublicationAttempts,
	)
	if err != nil {
		return AnalyticsOverview{}, fmt.Errorf("read analytics overview: %w", err)
	}
	return overview, nil
}
