package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

type ProposalRevisionReader interface {
	ProposalRevisionDetail(context.Context, string) (sqlite.ProposalRevisionDetail, error)
}
type ProposalDetailOptions struct{ Reader ProposalRevisionReader }

func registerProposalDetailRoutes(mux *http.ServeMux, options ProposalDetailOptions) {
	if mux == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/proposal-revisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemoteAddress(r.RemoteAddr) {
			writeControlError(w, http.StatusForbidden, "loopback_required", "proposal feedback is available only on loopback", false)
			return
		}
		if options.Reader == nil {
			writeControlError(w, http.StatusServiceUnavailable, "proposal_feedback_unavailable", "proposal feedback is unavailable", true)
			return
		}
		id := strings.TrimSpace(r.PathValue("id"))
		detail, err := options.Reader.ProposalRevisionDetail(r.Context(), id)
		if errors.Is(err, sqlite.ErrProposalRevisionNotFound) {
			writeControlError(w, http.StatusNotFound, "proposal_revision_not_found", "proposal revision was not found", false)
			return
		}
		if err != nil {
			writeControlError(w, http.StatusServiceUnavailable, "proposal_feedback_unavailable", "proposal feedback is unavailable", true)
			return
		}
		var comments json.RawMessage = detail.InlineCommentsJSON
		if len(comments) == 0 {
			comments = json.RawMessage(`[]`)
		}
		writeControlJSON(w, http.StatusOK, struct {
			ProposalID         string          `json:"proposal_id"`
			ProposalRevisionID string          `json:"proposal_revision_id"`
			Body               string          `json:"body"`
			InlineComments     json.RawMessage `json:"inline_comments"`
		}{detail.ProposalID, detail.ProposalRevisionID, detail.Body, comments})
	})
}
