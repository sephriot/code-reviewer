package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	defaultReadPageLimit = 50
	maxReadPageLimit     = 100
)

// TimelineState says whether an immutable record still matches the selected
// canonical projection. Historical records remain visible and are never
// silently presented as current.
type TimelineState string

const (
	TimelineStateCurrent    TimelineState = "current"
	TimelineStateHistorical TimelineState = "historical"
)

// AttentionKind classifies a current item that needs an operator's attention.
type AttentionKind string

const (
	AttentionKindPendingProposal AttentionKind = "pending_proposal"
	AttentionKindHumanReview     AttentionKind = "human_review"
	AttentionKindFailedRun       AttentionKind = "failed_run"
)

// AttentionQuery bounds a current, read-only operational inbox page. An empty
// ConnectionID includes current items from all connections.
type AttentionQuery struct {
	ConnectionID string
	Limit        int
	Cursor       string
}

// AttentionItem is a durable, evidence-bound inbox record. Current and State
// are deliberately redundant for straightforward UI and API consumers.
type AttentionItem struct {
	Kind          AttentionKind
	ID            string
	ConnectionID  string
	PullRequestID string
	RevisionID    string
	ObservationID string
	OccurredAt    time.Time
	State         TimelineState
	Current       bool
	Detail        string
}

// AttentionPage is one bounded page of current operational attention.
type AttentionPage struct {
	Items      []AttentionItem
	NextCursor string
}

// TimelineKind identifies an immutable pull-request fact or ledger record.
type TimelineKind string

const (
	TimelineKindObservation           TimelineKind = "observation"
	TimelineKindRevision              TimelineKind = "revision"
	TimelineKindRun                   TimelineKind = "review_run"
	TimelineKindAssessment            TimelineKind = "assessment"
	TimelineKindPolicyEvaluation      TimelineKind = "policy_evaluation"
	TimelineKindProposal              TimelineKind = "proposal"
	TimelineKindProposalRevision      TimelineKind = "proposal_revision"
	TimelineKindDecision              TimelineKind = "decision"
	TimelineKindPublicationEffect     TimelineKind = "publication_effect"
	TimelineKindPublicationResolution TimelineKind = "publication_resolution"
)

// PullRequestTimelineQuery bounds the immutable history of one local pull
// request. Both IDs are local control-plane identities, never GitHub input.
type PullRequestTimelineQuery struct {
	ConnectionID  string
	PullRequestID string
	Limit         int
	Cursor        string
}

// TimelineItem is a fact or ledger record in chronological order. State and
// Current make selected evidence versus retained history explicit.
type TimelineItem struct {
	Kind          TimelineKind
	ID            string
	ConnectionID  string
	PullRequestID string
	RevisionID    string
	ObservationID string
	OccurredAt    time.Time
	State         TimelineState
	Current       bool
	Detail        string
}

// PullRequestTimelinePage is one bounded page of an immutable PR timeline.
type PullRequestTimelinePage struct {
	Items      []TimelineItem
	NextCursor string
}

// HistoryKind identifies a completed review execution or an immutable human
// decision/publication outcome. It intentionally excludes in-flight jobs and
// mutable operational state.
type HistoryKind string

const (
	HistoryKindCompletedRun          HistoryKind = "completed_review_run"
	HistoryKindDecision              HistoryKind = "decision"
	HistoryKindPublicationAttempt    HistoryKind = "publication_attempt"
	HistoryKindPublicationResolution HistoryKind = "publication_resolution"
)

// HistoryQuery bounds the durable control-plane history. ConnectionID is an
// optional local partition filter; it never selects an external credential.
type HistoryQuery struct {
	ConnectionID string
	Limit        int
	Cursor       string
}

// HistoryItem is a terminal review outcome, operator decision, or completed
// publication attempt retained by the append-only ledger.
type HistoryItem struct {
	Kind          HistoryKind
	ID            string
	ConnectionID  string
	PullRequestID string
	RevisionID    string
	ObservationID string
	OccurredAt    time.Time
	State         TimelineState
	Current       bool
	Detail        string
}

// HistoryPage is one bounded page of durable control-plane history.
type HistoryPage struct {
	Items      []HistoryItem
	NextCursor string
}

type readCursor struct {
	OccurredAtUS int64  `json:"occurred_at_us"`
	Kind         string `json:"kind"`
	ID           string `json:"id"`
}

func normalizeReadPage(limit int, cursor string) (int, readCursor, error) {
	if limit == 0 {
		limit = defaultReadPageLimit
	}
	if limit < 1 || limit > maxReadPageLimit {
		return 0, readCursor{}, fmt.Errorf("read page limit must be between 1 and %d", maxReadPageLimit)
	}
	if cursor == "" {
		return limit, readCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(decoded) == 0 || len(decoded) > 512 {
		return 0, readCursor{}, errors.New("read page cursor is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	var value readCursor
	if err := decoder.Decode(&value); err != nil || value.OccurredAtUS < 0 || value.Kind == "" || value.ID == "" {
		return 0, readCursor{}, errors.New("read page cursor is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, readCursor{}, errors.New("read page cursor is invalid")
	}
	return limit, value, nil
}

func encodeReadCursor(value readCursor) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode read page cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func timelineState(current bool) TimelineState {
	if current {
		return TimelineStateCurrent
	}
	return TimelineStateHistorical
}

// ListCurrentAttention lists only facts matching each PR's selected canonical
// evidence: latest undecided proposal revisions, human-review outcomes, and
// failed/canceled runs. It performs SELECT queries only.
func (s *Store) ListCurrentAttention(ctx context.Context, query AttentionQuery) (AttentionPage, error) {
	connectionID := strings.TrimSpace(query.ConnectionID)
	limit, cursor, err := normalizeReadPage(query.Limit, query.Cursor)
	if err != nil {
		return AttentionPage{}, err
	}

	hasCursor := 0
	if query.Cursor != "" {
		hasCursor = 1
	}
	rows, err := s.db.QueryContext(ctx, `
WITH latest_run_events AS (
 SELECT event.run_id, event.event_kind, event.occurred_at_us
 FROM review_run_events AS event
 WHERE event.sequence = (SELECT MAX(candidate.sequence) FROM review_run_events AS candidate WHERE candidate.run_id = event.run_id)
), attention AS (
 SELECT 'pending_proposal' AS kind, proposal_revision.id, evaluation.connection_id,
        proposal_revision.pull_request_id, proposal_revision.revision_id, proposal_revision.observation_id,
        proposal_revision.created_at_us AS occurred_at_us, proposal.proposal_kind AS detail
 FROM proposal_revisions AS proposal_revision
 JOIN proposals AS proposal ON proposal.id = proposal_revision.proposal_id
 JOIN policy_evaluations AS evaluation ON evaluation.id = proposal_revision.policy_evaluation_id
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = proposal_revision.pull_request_id
  AND projection.connection_id = evaluation.connection_id
 WHERE proposal_revision.revision_id = projection.current_revision_id
   AND proposal_revision.observation_id = projection.current_observation_id
   AND NOT EXISTS (SELECT 1 FROM proposal_revisions AS newer
                   WHERE newer.proposal_id = proposal_revision.proposal_id
                     AND newer.revision_number > proposal_revision.revision_number)
   AND NOT EXISTS (SELECT 1 FROM decisions AS decision WHERE decision.proposal_revision_id = proposal_revision.id)
 UNION ALL
 SELECT 'human_review', evaluation.id, evaluation.connection_id, evaluation.pull_request_id,
        evaluation.revision_id, evaluation.observation_id, evaluation.created_at_us, evaluation.disposition
 FROM policy_evaluations AS evaluation
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = evaluation.pull_request_id
  AND projection.connection_id = evaluation.connection_id
 WHERE evaluation.revision_id = projection.current_revision_id
   AND evaluation.observation_id = projection.current_observation_id
   AND evaluation.disposition = 'require_human_review'
 UNION ALL
 SELECT 'failed_run', run.id, run.connection_id, run.pull_request_id, run.revision_id,
        run.observation_id, event.occurred_at_us, event.event_kind
 FROM review_runs AS run
 JOIN latest_run_events AS event ON event.run_id = run.id
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = run.pull_request_id
  AND projection.connection_id = run.connection_id
 WHERE run.revision_id = projection.current_revision_id
   AND run.observation_id = projection.current_observation_id
   AND event.event_kind IN ('failed_retryable', 'failed_terminal', 'canceled', 'superseded')
)
SELECT kind, id, connection_id, pull_request_id, revision_id, observation_id, occurred_at_us, detail
FROM attention
WHERE (? = '' OR connection_id = ?)
  AND (? = 0 OR occurred_at_us < ? OR (occurred_at_us = ? AND (kind > ? OR (kind = ? AND id > ?))))
ORDER BY occurred_at_us DESC, kind, id
LIMIT ?`, connectionID, connectionID, hasCursor, cursor.OccurredAtUS, cursor.OccurredAtUS, cursor.Kind, cursor.Kind, cursor.ID, limit+1)
	if err != nil {
		return AttentionPage{}, fmt.Errorf("list current attention: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := AttentionPage{Items: make([]AttentionItem, 0, limit)}
	for rows.Next() {
		var item AttentionItem
		var occurredAtUS int64
		if err := rows.Scan(&item.Kind, &item.ID, &item.ConnectionID, &item.PullRequestID, &item.RevisionID, &item.ObservationID, &occurredAtUS, &item.Detail); err != nil {
			return AttentionPage{}, fmt.Errorf("scan current attention: %w", err)
		}
		item.OccurredAt, item.State, item.Current = time.UnixMicro(occurredAtUS).UTC(), TimelineStateCurrent, true
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return AttentionPage{}, fmt.Errorf("iterate current attention: %w", err)
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		next, err := encodeReadCursor(readCursor{OccurredAtUS: last.OccurredAt.UnixMicro(), Kind: string(last.Kind), ID: last.ID})
		if err != nil {
			return AttentionPage{}, err
		}
		page.Items, page.NextCursor = page.Items[:limit], next
	}
	return page, nil
}

// ListHistory returns a bounded, read-only page across immutable terminal
// review outcomes, decisions, and publication attempts. It does not expose
// mutable jobs or start any publication work.
func (s *Store) ListHistory(ctx context.Context, query HistoryQuery) (HistoryPage, error) {
	connectionID := strings.TrimSpace(query.ConnectionID)
	limit, cursor, err := normalizeReadPage(query.Limit, query.Cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	hasCursor := 0
	if query.Cursor != "" {
		hasCursor = 1
	}
	rows, err := s.db.QueryContext(ctx, `
WITH latest_run_events AS (
 SELECT event.run_id, event.event_kind, event.occurred_at_us
 FROM review_run_events AS event
 WHERE event.sequence = (
   SELECT MAX(candidate.sequence) FROM review_run_events AS candidate WHERE candidate.run_id = event.run_id
 )
), history AS (
 SELECT 'completed_review_run' AS kind, run.id, run.connection_id, run.pull_request_id,
        run.revision_id, run.observation_id, event.occurred_at_us,
        run.engine_kind || ':' || event.event_kind AS detail,
        CASE WHEN run.revision_id = projection.current_revision_id
               AND run.observation_id = projection.current_observation_id THEN 1 ELSE 0 END AS current
 FROM review_runs AS run
 JOIN latest_run_events AS event ON event.run_id = run.id
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = run.pull_request_id AND projection.connection_id = run.connection_id
 WHERE event.event_kind IN ('succeeded', 'failed_terminal', 'canceled', 'superseded')
 UNION ALL
 SELECT 'decision', decision.id, decision.connection_id, decision.pull_request_id,
        decision.revision_id, decision.observation_id, decision.created_at_us,
        decision.decision || ':' || decision.actor_kind AS detail,
        CASE WHEN decision.revision_id = projection.current_revision_id
               AND decision.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM decisions AS decision
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = decision.pull_request_id AND projection.connection_id = decision.connection_id
 UNION ALL
 SELECT 'publication_attempt', attempt.id, effect.connection_id, effect.pull_request_id,
        effect.revision_id, effect.observation_id, attempt.completed_at_us,
        effect.effect_type || ':' || attempt.publication_mode || ':' || attempt.outcome AS detail,
        CASE WHEN effect.revision_id = projection.current_revision_id
               AND effect.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM publication_attempts AS attempt
 JOIN publication_effects AS effect ON effect.id = attempt.effect_id
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = effect.pull_request_id AND projection.connection_id = effect.connection_id
	UNION ALL
 SELECT 'publication_resolution', resolution.id, effect.connection_id, effect.pull_request_id,
        effect.revision_id, effect.observation_id, resolution.resolved_at_us,
        effect.effect_type || ':resolution:' || resolution.resolution AS detail,
        CASE WHEN effect.revision_id = projection.current_revision_id
               AND effect.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM publication_uncertainty_resolutions AS resolution
 JOIN publication_effects AS effect ON effect.id = resolution.effect_id
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = effect.pull_request_id AND projection.connection_id = effect.connection_id
)
SELECT kind, id, connection_id, pull_request_id, revision_id, observation_id, occurred_at_us, detail, current
FROM history
WHERE (? = '' OR connection_id = ?)
  AND (? = 0 OR occurred_at_us < ? OR (occurred_at_us = ? AND (kind > ? OR (kind = ? AND id > ?))))
ORDER BY occurred_at_us DESC, kind, id
LIMIT ?`, connectionID, connectionID, hasCursor, cursor.OccurredAtUS, cursor.OccurredAtUS, cursor.Kind, cursor.Kind, cursor.ID, limit+1)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("list durable history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := HistoryPage{Items: make([]HistoryItem, 0, limit)}
	for rows.Next() {
		var item HistoryItem
		var occurredAtUS, current int64
		if err := rows.Scan(&item.Kind, &item.ID, &item.ConnectionID, &item.PullRequestID, &item.RevisionID, &item.ObservationID, &occurredAtUS, &item.Detail, &current); err != nil {
			return HistoryPage{}, fmt.Errorf("scan durable history: %w", err)
		}
		item.OccurredAt, item.Current = time.UnixMicro(occurredAtUS).UTC(), current != 0
		item.State = timelineState(item.Current)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, fmt.Errorf("iterate durable history: %w", err)
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		next, err := encodeReadCursor(readCursor{OccurredAtUS: last.OccurredAt.UnixMicro(), Kind: string(last.Kind), ID: last.ID})
		if err != nil {
			return HistoryPage{}, err
		}
		page.Items, page.NextCursor = page.Items[:limit], next
	}
	return page, nil
}

// PullRequestTimeline returns selected and retained historical facts for one
// PR. It is a bounded, read-only projection over immutable ledger tables.
func (s *Store) PullRequestTimeline(ctx context.Context, query PullRequestTimelineQuery) (PullRequestTimelinePage, error) {
	connectionID, pullRequestID := strings.TrimSpace(query.ConnectionID), strings.TrimSpace(query.PullRequestID)
	if connectionID == "" || pullRequestID == "" {
		return PullRequestTimelinePage{}, errors.New("timeline connection and pull request IDs are required")
	}
	limit, cursor, err := normalizeReadPage(query.Limit, query.Cursor)
	if err != nil {
		return PullRequestTimelinePage{}, err
	}
	hasCursor := 0
	if query.Cursor != "" {
		hasCursor = 1
	}
	rows, err := s.db.QueryContext(ctx, `
WITH timeline AS (
 SELECT 'observation' AS kind, observation.id, observation.connection_id, observation.pull_request_id,
        COALESCE(observation.revision_id, '') AS revision_id, observation.id AS observation_id,
        observation.observed_at_us AS occurred_at_us,
        observation.source_kind || ':' || observation.github_state AS detail,
        CASE WHEN observation.id = projection.current_observation_id THEN 1 ELSE 0 END AS current
 FROM pull_request_observations AS observation
 JOIN pull_request_projection_state AS projection
   ON projection.pull_request_id = observation.pull_request_id AND projection.connection_id = observation.connection_id
 WHERE observation.connection_id = ? AND observation.pull_request_id = ?
 UNION ALL
 SELECT 'revision', revision.id, projection.connection_id, revision.pull_request_id, revision.id, '',
        revision.observed_at_us, revision.identity_kind,
        CASE WHEN revision.id = projection.current_revision_id THEN 1 ELSE 0 END
 FROM revisions AS revision
 JOIN pull_request_projection_state AS projection ON projection.pull_request_id = revision.pull_request_id
 WHERE projection.connection_id = ? AND revision.pull_request_id = ?
 UNION ALL
 SELECT 'review_run', run.id, run.connection_id, run.pull_request_id, run.revision_id, run.observation_id,
        run.started_at_us, run.engine_kind,
        CASE WHEN run.revision_id = projection.current_revision_id AND run.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM review_runs AS run JOIN pull_request_projection_state AS projection ON projection.pull_request_id = run.pull_request_id AND projection.connection_id = run.connection_id
 WHERE run.connection_id = ? AND run.pull_request_id = ?
 UNION ALL
 SELECT 'assessment', assessment.id, intent.connection_id, assessment.pull_request_id, assessment.revision_id, assessment.observation_id,
        assessment.created_at_us, assessment.verdict,
        CASE WHEN assessment.revision_id = projection.current_revision_id AND assessment.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM assessments AS assessment JOIN review_intents AS intent ON intent.id = assessment.intent_id
 JOIN pull_request_projection_state AS projection ON projection.pull_request_id = assessment.pull_request_id AND projection.connection_id = intent.connection_id
 WHERE intent.connection_id = ? AND assessment.pull_request_id = ?
 UNION ALL
 SELECT 'policy_evaluation', evaluation.id, evaluation.connection_id, evaluation.pull_request_id, evaluation.revision_id, evaluation.observation_id,
        evaluation.created_at_us, evaluation.disposition,
        CASE WHEN evaluation.revision_id = projection.current_revision_id AND evaluation.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM policy_evaluations AS evaluation JOIN pull_request_projection_state AS projection ON projection.pull_request_id = evaluation.pull_request_id AND projection.connection_id = evaluation.connection_id
 WHERE evaluation.connection_id = ? AND evaluation.pull_request_id = ?
 UNION ALL
 SELECT 'proposal', proposal.id, evaluation.connection_id, proposal.pull_request_id, proposal.revision_id, proposal.observation_id,
        proposal.created_at_us, proposal.proposal_kind,
        CASE WHEN proposal.revision_id = projection.current_revision_id AND proposal.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM proposals AS proposal JOIN policy_evaluations AS evaluation ON evaluation.id = proposal.policy_evaluation_id
 JOIN pull_request_projection_state AS projection ON projection.pull_request_id = proposal.pull_request_id AND projection.connection_id = evaluation.connection_id
 WHERE evaluation.connection_id = ? AND proposal.pull_request_id = ?
 UNION ALL
 SELECT 'proposal_revision', proposal_revision.id, evaluation.connection_id, proposal_revision.pull_request_id, proposal_revision.revision_id, proposal_revision.observation_id,
        proposal_revision.created_at_us, proposal_revision.editor_kind,
        CASE WHEN proposal_revision.revision_id = projection.current_revision_id AND proposal_revision.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM proposal_revisions AS proposal_revision JOIN policy_evaluations AS evaluation ON evaluation.id = proposal_revision.policy_evaluation_id
 JOIN pull_request_projection_state AS projection ON projection.pull_request_id = proposal_revision.pull_request_id AND projection.connection_id = evaluation.connection_id
 WHERE evaluation.connection_id = ? AND proposal_revision.pull_request_id = ?
 UNION ALL
 SELECT 'decision', decision.id, decision.connection_id, decision.pull_request_id, decision.revision_id, decision.observation_id,
        decision.created_at_us, decision.decision,
        CASE WHEN decision.revision_id = projection.current_revision_id AND decision.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM decisions AS decision JOIN pull_request_projection_state AS projection ON projection.pull_request_id = decision.pull_request_id AND projection.connection_id = decision.connection_id
 WHERE decision.connection_id = ? AND decision.pull_request_id = ?
 UNION ALL
 SELECT 'publication_effect', effect.id, effect.connection_id, effect.pull_request_id, effect.revision_id, effect.observation_id,
        effect.created_at_us, effect.effect_type || ':' || effect.publication_mode_at_authorization,
        CASE WHEN effect.revision_id = projection.current_revision_id AND effect.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM publication_effects AS effect JOIN pull_request_projection_state AS projection ON projection.pull_request_id = effect.pull_request_id AND projection.connection_id = effect.connection_id
 WHERE effect.connection_id = ? AND effect.pull_request_id = ?
 UNION ALL
 SELECT 'publication_resolution', resolution.id, effect.connection_id, effect.pull_request_id, effect.revision_id, effect.observation_id,
        resolution.resolved_at_us, effect.effect_type || ':resolution:' || resolution.resolution,
        CASE WHEN effect.revision_id = projection.current_revision_id AND effect.observation_id = projection.current_observation_id THEN 1 ELSE 0 END
 FROM publication_uncertainty_resolutions AS resolution
 JOIN publication_effects AS effect ON effect.id = resolution.effect_id
 JOIN pull_request_projection_state AS projection ON projection.pull_request_id = effect.pull_request_id AND projection.connection_id = effect.connection_id
 WHERE effect.connection_id = ? AND effect.pull_request_id = ?
)
SELECT kind, id, connection_id, pull_request_id, revision_id, observation_id, occurred_at_us, detail, current
FROM timeline
WHERE (? = 0 OR occurred_at_us < ? OR (occurred_at_us = ? AND (kind > ? OR (kind = ? AND id > ?))))
ORDER BY occurred_at_us DESC, kind, id
LIMIT ?`,
		connectionID, pullRequestID, connectionID, pullRequestID, connectionID, pullRequestID,
		connectionID, pullRequestID, connectionID, pullRequestID, connectionID, pullRequestID,
		connectionID, pullRequestID, connectionID, pullRequestID, connectionID, pullRequestID,
		connectionID, pullRequestID,
		hasCursor, cursor.OccurredAtUS, cursor.OccurredAtUS, cursor.Kind, cursor.Kind, cursor.ID, limit+1)
	if err != nil {
		return PullRequestTimelinePage{}, fmt.Errorf("list pull request timeline: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := PullRequestTimelinePage{Items: make([]TimelineItem, 0, limit)}
	for rows.Next() {
		var item TimelineItem
		var occurredAtUS, current int64
		if err := rows.Scan(&item.Kind, &item.ID, &item.ConnectionID, &item.PullRequestID, &item.RevisionID, &item.ObservationID, &occurredAtUS, &item.Detail, &current); err != nil {
			return PullRequestTimelinePage{}, fmt.Errorf("scan pull request timeline: %w", err)
		}
		item.OccurredAt, item.Current = time.UnixMicro(occurredAtUS).UTC(), current != 0
		item.State = timelineState(item.Current)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return PullRequestTimelinePage{}, fmt.Errorf("iterate pull request timeline: %w", err)
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		next, err := encodeReadCursor(readCursor{OccurredAtUS: last.OccurredAt.UnixMicro(), Kind: string(last.Kind), ID: last.ID})
		if err != nil {
			return PullRequestTimelinePage{}, err
		}
		page.Items, page.NextCursor = page.Items[:limit], next
	}
	return page, nil
}
