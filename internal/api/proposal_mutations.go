package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const maxProposalMutationRequestBytes = 128 * 1024

// ProposalRevisionCreator is the only write capability needed to append a
// human proposal revision.
type ProposalRevisionCreator interface {
	CreateHumanProposalRevision(context.Context, sqlite.CreateHumanProposalRevisionInput) (sqlite.CreateHumanProposalRevisionResult, error)
}

// ProposalDecisionRecorder is the only write capability needed to record a
// human decision that is scoped by the route's proposal identity.
type ProposalDecisionRecorder interface {
	RecordProposalDecisionForProposal(context.Context, string, sqlite.RecordProposalDecisionInput) (sqlite.RecordProposalDecisionResult, error)
}

// ProposalMutationOptions supplies deliberately narrow persistence
// capabilities. It has no GitHub client, publication capability, or worker.
type ProposalMutationOptions struct {
	Revisions ProposalRevisionCreator
	Decisions ProposalDecisionRecorder
	Now       func() time.Time
}

// NewProposalMutationHandler exposes versioned, local proposal edits and
// decisions. It does not authenticate requests itself: callers must mount it
// beneath MutationAuth.Wrap.
func NewProposalMutationHandler(options ProposalMutationOptions) http.Handler {
	mux := http.NewServeMux()
	handler := proposalMutationHandler{revisions: options.Revisions, decisions: options.Decisions, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposals/{id}/revisions", handler.createRevision)
	mux.HandleFunc("POST /api/v1/mutate/proposals/{id}/decisions", handler.recordDecision)
	return mux
}

func registerProposalMutationRoutes(mux *http.ServeMux, options ProposalMutationOptions) {
	if mux == nil {
		return
	}
	handler := proposalMutationHandler{revisions: options.Revisions, decisions: options.Decisions, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposals/{id}/revisions", handler.createRevision)
	mux.HandleFunc("POST /api/v1/mutate/proposals/{id}/decisions", handler.recordDecision)
}

type proposalMutationHandler struct {
	revisions ProposalRevisionCreator
	decisions ProposalDecisionRecorder
	now       func() time.Time
}

type createProposalRevisionRequest struct {
	Body           string          `json:"body"`
	InlineComments json.RawMessage `json:"inline_comments"`
}

type createProposalRevisionResponse struct {
	ProposalRevisionID string `json:"proposal_revision_id"`
	RevisionNumber     int    `json:"revision_number"`
}

func (h proposalMutationHandler) createRevision(response http.ResponseWriter, request *http.Request) {
	proposalID, ok := validProposalPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "proposal ID is invalid", false)
		return
	}
	if h.revisions == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "proposal mutation service is unavailable", true)
		return
	}
	var input createProposalRevisionRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	created, err := h.revisions.CreateHumanProposalRevision(request.Context(), sqlite.CreateHumanProposalRevisionInput{
		ProposalID: proposalID, Body: input.Body, InlineCommentsJSON: input.InlineComments, EditedAt: h.currentTime(),
	})
	if err != nil {
		writeProposalMutationStoreError(response, err)
		return
	}
	writeControlJSON(response, http.StatusCreated, createProposalRevisionResponse{ProposalRevisionID: created.ProposalRevisionID, RevisionNumber: created.RevisionNumber})
}

type recordProposalDecisionRequest struct {
	ProposalRevisionID string `json:"proposal_revision_id"`
	Decision           string `json:"decision"`
	ActorID            string `json:"actor_id"`
	IdempotencyKey     string `json:"idempotency_key"`
	Reason             string `json:"reason"`
}

type recordProposalDecisionResponse struct {
	DecisionID string `json:"decision_id"`
	Created    bool   `json:"created"`
}

func (h proposalMutationHandler) recordDecision(response http.ResponseWriter, request *http.Request) {
	proposalID, ok := validProposalPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "proposal ID is invalid", false)
		return
	}
	if h.decisions == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "proposal mutation service is unavailable", true)
		return
	}
	var input recordProposalDecisionRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if strings.TrimSpace(input.ProposalRevisionID) == "" || strings.TrimSpace(input.ActorID) == "" || strings.TrimSpace(input.IdempotencyKey) == "" ||
		(input.Decision != string(sqlite.ProposalDecisionApprove) && input.Decision != string(sqlite.ProposalDecisionReject)) {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "proposal decision fields are invalid", false)
		return
	}
	recorded, err := h.decisions.RecordProposalDecisionForProposal(request.Context(), proposalID, sqlite.RecordProposalDecisionInput{
		ProposalRevisionID: input.ProposalRevisionID,
		Decision:           sqlite.ProposalDecision(input.Decision),
		ActorKind:          sqlite.ProposalDecisionActorHuman,
		ActorID:            input.ActorID,
		IdempotencyKey:     input.IdempotencyKey,
		Reason:             input.Reason,
		DecidedAt:          h.currentTime(),
	})
	if err != nil {
		writeProposalMutationStoreError(response, err)
		return
	}
	status := http.StatusOK
	if recorded.Created {
		status = http.StatusCreated
	}
	writeControlJSON(response, status, recordProposalDecisionResponse{DecisionID: recorded.DecisionID, Created: recorded.Created})
}

func validProposalPathID(request *http.Request) (string, bool) {
	proposalID := strings.TrimSpace(request.PathValue("id"))
	return proposalID, proposalID != "" && len(proposalID) <= 512
}

func (h proposalMutationHandler) currentTime() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func decodeProposalMutationJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != jsonContentType {
		return errors.New("Content-Type must be application/json")
	}
	if request.ContentLength > maxProposalMutationRequestBytes {
		return errMutationBodyTooLarge
	}
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, maxProposalMutationRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			return errMutationBodyTooLarge
		}
		return fmt.Errorf("decode JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

var errMutationBodyTooLarge = errors.New("request body exceeds maximum size")

func writeMutationDecodeError(response http.ResponseWriter, err error) {
	if errors.Is(err, errMutationBodyTooLarge) {
		writeControlError(response, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds maximum size", false)
		return
	}
	writeControlError(response, http.StatusBadRequest, "invalid_request", "request body must be one strict JSON object", false)
}

func writeProposalMutationStoreError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sqlite.ErrProposalNotFound):
		writeControlError(response, http.StatusNotFound, "proposal_not_found", "proposal or proposal revision was not found", false)
	case errors.Is(err, sqlite.ErrCanonicalReviewTargetNotFound), errors.Is(err, sqlite.ErrProposalDecisionConflict), strings.Contains(err.Error(), "no longer matches current canonical evidence"):
		writeControlError(response, http.StatusConflict, "proposal_not_current", "proposal no longer matches current canonical evidence", false)
	default:
		writeControlError(response, http.StatusInternalServerError, "mutation_failed", "could not record proposal mutation", true)
	}
}
