package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrProposalRevisionNotFound = errors.New("proposal revision not found")

// ProposalRevisionDetail is the exact immutable feedback an operator can
// approve. Diff text is intentionally not retained by the control plane.
type ProposalRevisionDetail struct {
	ProposalID         string
	ProposalRevisionID string
	Body               string
	InlineCommentsJSON []byte
}

func (s *Store) ProposalRevisionDetail(ctx context.Context, id string) (ProposalRevisionDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 512 {
		return ProposalRevisionDetail{}, ErrProposalRevisionNotFound
	}
	var detail ProposalRevisionDetail
	err := s.db.QueryRowContext(ctx, `SELECT proposal_id, id, body, inline_comments_json FROM proposal_revisions WHERE id = ?`, id).Scan(&detail.ProposalID, &detail.ProposalRevisionID, &detail.Body, &detail.InlineCommentsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposalRevisionDetail{}, ErrProposalRevisionNotFound
	}
	if err != nil {
		return ProposalRevisionDetail{}, fmt.Errorf("load proposal revision detail: %w", err)
	}
	return detail, nil
}
