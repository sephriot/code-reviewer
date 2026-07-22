package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxPublicationPayloadBytes = 64 * 1024
)

var (
	// ErrPublicationEffectConflict means an idempotency key is already bound
	// to different immutable publication facts.
	ErrPublicationEffectConflict = errors.New("publication effect facts conflict")
	// ErrPublicationAuthorizationNotFound means no approved proposal revision
	// can safely authorize the requested publication effect.
	ErrPublicationAuthorizationNotFound = errors.New("approved proposal revision not found")
	// ErrPublicationModeUnsupported means persisted publication mode is unknown.
	ErrPublicationModeUnsupported = errors.New("publication mode is not safe for local effect creation")
)

// PublicationMode is the durable mode observed while authorizing an effect.
type PublicationMode string

const (
	PublicationModeDisabled  PublicationMode = "disabled"
	PublicationModeSimulated PublicationMode = "simulated"
	// PublicationModeEnabled records an explicitly enabled intent. Creating an
	// effect still performs no network activity; a separate bounded worker owns
	// GitHub write capability.
	PublicationModeEnabled PublicationMode = "enabled"
)

// CreatePublicationEffectInput identifies an approved proposal revision. The
// exact outbound payload is derived from that immutable revision; callers
// cannot substitute arbitrary GitHub content.
type CreatePublicationEffectInput struct {
	ProposalRevisionID string
	IdempotencyKey     string
	CreatedAt          time.Time
}

// CreatePublicationEffectResult identifies one durable publication effect.
type CreatePublicationEffectResult struct {
	EffectID        string
	PublicationMode PublicationMode
	Created         bool
}

// CreatePublicationEffect writes an approved proposal revision's exact
// publication effect only after re-validating its selected canonical evidence.
// It records no attempt: the separate publication worker revalidates current
// evidence before dispatching. This method never creates jobs, events, outbox
// rows, or GitHub traffic.
func (s *Store) CreatePublicationEffect(ctx context.Context, input CreatePublicationEffectInput) (CreatePublicationEffectResult, error) {
	normalized, err := normalizeCreatePublicationEffectInput(input)
	if err != nil {
		return CreatePublicationEffectResult{}, err
	}

	var result CreatePublicationEffectResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		mode, err := loadSafePublicationMode(ctx, conn)
		if err != nil {
			return err
		}
		authorization, err := loadApprovedPublicationAuthorization(ctx, conn, normalized.ProposalRevisionID)
		if err != nil {
			return err
		}
		target, err := loadCurrentCanonicalReviewTarget(ctx, conn, authorization.ConnectionID, authorization.PullRequestID)
		if err != nil {
			return err
		}
		if !authorization.matchesTarget(target) {
			return errors.New("approved proposal revision no longer matches current canonical evidence")
		}
		payload, payloadSHA256, err := authorization.payload()
		if err != nil {
			return err
		}
		effectType, err := publicationEffectType(authorization.ProposalKind)
		if err != nil {
			return err
		}

		idempotencyKey := normalized.IdempotencyKey
		if idempotencyKey == "" {
			idempotencyKey = publicationEffectIdempotencyKey(authorization, payloadSHA256)
		}
		existing, found, err := loadPublicationEffect(ctx, conn, idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(authorization, payload, payloadSHA256, mode) {
				return fmt.Errorf("%w: key=%q", ErrPublicationEffectConflict, idempotencyKey)
			}
			result = CreatePublicationEffectResult{EffectID: existing.ID, PublicationMode: mode}
			return nil
		}
		semanticEffect, found, err := loadPublicationEffectBySemanticIdentity(ctx, conn, authorization, effectType, payloadSHA256)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%w: effect=%q", ErrPublicationEffectConflict, semanticEffect.ID)
		}

		createdAt := normalized.CreatedAt.UnixMicro()
		effectID := stableID("publication-effect", idempotencyKey)
		if _, err := conn.ExecContext(ctx, `
INSERT INTO publication_effects(
 id, owner_kind, owner_id, proposal_revision_id, authorization_decision_id,
 connection_id, repository_id, pull_request_id, revision_id, observation_id,
 effect_type, payload_json, payload_sha256, idempotency_key,
 publication_mode_at_authorization, created_at_us)
VALUES (?, 'proposal_revision', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			effectID, authorization.ProposalRevisionID, authorization.ProposalRevisionID, authorization.DecisionID,
			authorization.ConnectionID, authorization.RepositoryID, authorization.PullRequestID,
			authorization.RevisionID, authorization.ObservationID, effectType,
			payload, payloadSHA256, idempotencyKey, mode, createdAt); err != nil {
			return fmt.Errorf("insert publication effect: %w", err)
		}
		result = CreatePublicationEffectResult{EffectID: effectID, PublicationMode: mode, Created: true}
		return nil
	})
	if err != nil {
		return CreatePublicationEffectResult{}, fmt.Errorf("create publication effect: %w", err)
	}
	return result, nil
}

type normalizedCreatePublicationEffectInput struct {
	ProposalRevisionID string
	IdempotencyKey     string
	CreatedAt          time.Time
}

func normalizeCreatePublicationEffectInput(input CreatePublicationEffectInput) (normalizedCreatePublicationEffectInput, error) {
	input.ProposalRevisionID = strings.TrimSpace(input.ProposalRevisionID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.ProposalRevisionID == "" || len(input.IdempotencyKey) > 512 {
		return normalizedCreatePublicationEffectInput{}, errors.New("publication effect input is invalid")
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	} else {
		input.CreatedAt = input.CreatedAt.UTC()
	}
	if input.CreatedAt.UnixMicro() < 0 {
		return normalizedCreatePublicationEffectInput{}, errors.New("publication effect time is invalid")
	}
	return normalizedCreatePublicationEffectInput{
		ProposalRevisionID: input.ProposalRevisionID, IdempotencyKey: input.IdempotencyKey, CreatedAt: input.CreatedAt,
	}, nil
}

func loadSafePublicationMode(ctx context.Context, conn *sql.Conn) (PublicationMode, error) {
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = 'publication_mode'`).Scan(&raw); err != nil {
		return "", fmt.Errorf("read publication mode: %w", err)
	}
	mode := PublicationMode(raw)
	if mode != PublicationModeDisabled && mode != PublicationModeSimulated && mode != PublicationModeEnabled {
		return "", ErrPublicationModeUnsupported
	}
	return mode, nil
}

type approvedPublicationAuthorization struct {
	DecisionID         string
	ProposalRevisionID string
	ConnectionID       string
	RepositoryID       string
	PullRequestID      string
	RevisionID         string
	ObservationID      string
	ProposalKind       string
	Body               string
	InlineCommentsJSON []byte
	ProposalContentSHA string
}

func loadApprovedPublicationAuthorization(ctx context.Context, conn *sql.Conn, proposalRevisionID string) (approvedPublicationAuthorization, error) {
	var authorization approvedPublicationAuthorization
	err := conn.QueryRowContext(ctx, `
SELECT decision.id, proposal_revision.id, decision.connection_id, decision.repository_id,
       decision.pull_request_id, decision.revision_id, decision.observation_id,
       proposal.proposal_kind, proposal_revision.body,
       proposal_revision.inline_comments_json, proposal_revision.content_sha256
FROM decisions AS decision
JOIN proposal_revisions AS proposal_revision ON proposal_revision.id = decision.proposal_revision_id
JOIN proposals AS proposal ON proposal.id = proposal_revision.proposal_id
WHERE decision.proposal_revision_id = ? AND decision.decision = 'approve'`, proposalRevisionID).Scan(
		&authorization.DecisionID, &authorization.ProposalRevisionID, &authorization.ConnectionID,
		&authorization.RepositoryID, &authorization.PullRequestID, &authorization.RevisionID,
		&authorization.ObservationID, &authorization.ProposalKind, &authorization.Body,
		&authorization.InlineCommentsJSON, &authorization.ProposalContentSHA,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return approvedPublicationAuthorization{}, ErrPublicationAuthorizationNotFound
	}
	if err != nil {
		return approvedPublicationAuthorization{}, fmt.Errorf("load approved proposal revision: %w", err)
	}
	return authorization, nil
}

func (authorization approvedPublicationAuthorization) matchesTarget(target CanonicalReviewTarget) bool {
	return authorization.ConnectionID == target.ConnectionID &&
		authorization.RepositoryID == target.RepositoryID &&
		authorization.PullRequestID == target.PullRequestID &&
		authorization.RevisionID == target.RevisionID &&
		authorization.ObservationID == target.ObservationID
}

func (authorization approvedPublicationAuthorization) payload() ([]byte, string, error) {
	inlineComments, err := normalizeBoundedJSONArray(authorization.InlineCommentsJSON, maxPublicationPayloadBytes)
	if err != nil {
		return nil, "", fmt.Errorf("approved proposal inline comments: %w", err)
	}
	payload, err := json.Marshal(struct {
		Body           string          `json:"body"`
		InlineComments json.RawMessage `json:"inline_comments"`
	}{Body: authorization.Body, InlineComments: inlineComments})
	if err != nil {
		return nil, "", fmt.Errorf("encode approved proposal payload: %w", err)
	}
	if len(payload) > maxPublicationPayloadBytes {
		return nil, "", errors.New("approved proposal payload exceeds maximum size")
	}
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	if authorization.ProposalContentSHA != digestText {
		return nil, "", errors.New("approved proposal payload digest is invalid")
	}
	return payload, digestText, nil
}

func publicationEffectType(proposalKind string) (string, error) {
	switch proposalKind {
	case "approval":
		return "review_approval", nil
	case "comment":
		return "review_comment", nil
	case "changes":
		return "review_changes", nil
	default:
		return "", errors.New("approved proposal kind is invalid")
	}
}

func publicationEffectIdempotencyKey(authorization approvedPublicationAuthorization, payloadSHA256 string) string {
	return "publication-effect:v1:" + stableID(
		"publication-effect-key", authorization.ProposalRevisionID, authorization.DecisionID,
		authorization.RevisionID, authorization.ObservationID, payloadSHA256,
	)
}

type storedPublicationEffect struct {
	ID                 string
	ProposalRevisionID string
	DecisionID         string
	ConnectionID       string
	RepositoryID       string
	PullRequestID      string
	RevisionID         string
	ObservationID      string
	EffectType         string
	PayloadJSON        []byte
	PayloadSHA256      string
	PublicationMode    PublicationMode
}

func loadPublicationEffect(ctx context.Context, conn *sql.Conn, idempotencyKey string) (storedPublicationEffect, bool, error) {
	var effect storedPublicationEffect
	err := conn.QueryRowContext(ctx, `
SELECT effect.id, effect.proposal_revision_id, effect.authorization_decision_id,
       effect.connection_id, effect.repository_id, effect.pull_request_id,
       effect.revision_id, effect.observation_id, effect.effect_type,
       effect.payload_json, effect.payload_sha256, effect.publication_mode_at_authorization
FROM publication_effects AS effect
WHERE effect.idempotency_key = ?`, idempotencyKey).Scan(
		&effect.ID, &effect.ProposalRevisionID, &effect.DecisionID, &effect.ConnectionID,
		&effect.RepositoryID, &effect.PullRequestID, &effect.RevisionID, &effect.ObservationID,
		&effect.EffectType, &effect.PayloadJSON, &effect.PayloadSHA256, &effect.PublicationMode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPublicationEffect{}, false, nil
	}
	if err != nil {
		return storedPublicationEffect{}, false, fmt.Errorf("load publication effect: %w", err)
	}
	return effect, true, nil
}

func loadPublicationEffectBySemanticIdentity(ctx context.Context, conn *sql.Conn, authorization approvedPublicationAuthorization, effectType, payloadSHA256 string) (storedPublicationEffect, bool, error) {
	var effect storedPublicationEffect
	err := conn.QueryRowContext(ctx, `
SELECT id FROM publication_effects
WHERE owner_kind = 'proposal_revision'
  AND owner_id = ?
  AND revision_id = ?
  AND observation_id = ?
  AND effect_type = ?
  AND payload_sha256 = ?`, authorization.ProposalRevisionID, authorization.RevisionID,
		authorization.ObservationID, effectType, payloadSHA256).Scan(&effect.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPublicationEffect{}, false, nil
	}
	if err != nil {
		return storedPublicationEffect{}, false, fmt.Errorf("load semantic publication effect: %w", err)
	}
	return effect, true, nil
}

func (effect storedPublicationEffect) matches(authorization approvedPublicationAuthorization, payload []byte, payloadSHA256 string, mode PublicationMode) bool {
	effectType, err := publicationEffectType(authorization.ProposalKind)
	return err == nil && effect.ProposalRevisionID == authorization.ProposalRevisionID &&
		effect.DecisionID == authorization.DecisionID && effect.ConnectionID == authorization.ConnectionID &&
		effect.RepositoryID == authorization.RepositoryID && effect.PullRequestID == authorization.PullRequestID &&
		effect.RevisionID == authorization.RevisionID && effect.ObservationID == authorization.ObservationID &&
		effect.EffectType == effectType && bytes.Equal(effect.PayloadJSON, payload) &&
		effect.PayloadSHA256 == payloadSHA256 && effect.PublicationMode == mode
}
