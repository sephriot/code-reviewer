package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

type fakeProposalMutations struct {
	revisionInput sqlite.CreateHumanProposalRevisionInput
	decisionID    string
	decisionInput sqlite.RecordProposalDecisionInput
	decisionOwner string
	revisionErr   error
	decisionErr   error
}

func (f *fakeProposalMutations) CreateHumanProposalRevision(_ context.Context, input sqlite.CreateHumanProposalRevisionInput) (sqlite.CreateHumanProposalRevisionResult, error) {
	f.revisionInput = input
	if f.revisionErr != nil {
		return sqlite.CreateHumanProposalRevisionResult{}, f.revisionErr
	}
	return sqlite.CreateHumanProposalRevisionResult{ProposalRevisionID: "revision-2", RevisionNumber: 2}, nil
}

func (f *fakeProposalMutations) RecordProposalDecisionForProposal(_ context.Context, proposalID string, input sqlite.RecordProposalDecisionInput) (sqlite.RecordProposalDecisionResult, error) {
	f.decisionOwner, f.decisionInput = proposalID, input
	if f.decisionErr != nil {
		return sqlite.RecordProposalDecisionResult{}, f.decisionErr
	}
	return sqlite.RecordProposalDecisionResult{DecisionID: "decision-1", Created: f.decisionID == ""}, nil
}

func TestProposalMutationRoutesAppendRevisionAndRecordDecision(t *testing.T) {
	t.Parallel()
	mutations := &fakeProposalMutations{}
	now := time.Unix(200, 0).UTC()
	handler := NewProposalMutationHandler(ProposalMutationOptions{Revisions: mutations, Decisions: mutations, Now: func() time.Time { return now }})

	response := serveProposalMutation(handler, http.MethodPost, "/api/v1/mutate/proposals/proposal-1/revisions", `{"body":"edited","inline_comments":[{"path":"a.go"}]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("revision status = %d, body=%s", response.Code, response.Body.String())
	}
	if mutations.revisionInput.ProposalID != "proposal-1" || mutations.revisionInput.Body != "edited" || string(mutations.revisionInput.InlineCommentsJSON) != `[{"path":"a.go"}]` || !mutations.revisionInput.EditedAt.Equal(now) {
		t.Fatalf("revision input = %+v", mutations.revisionInput)
	}
	var revision createProposalRevisionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &revision); err != nil || revision.ProposalRevisionID != "revision-2" || revision.RevisionNumber != 2 {
		t.Fatalf("revision response = %+v, %v", revision, err)
	}

	response = serveProposalMutation(handler, http.MethodPost, "/api/v1/mutate/proposals/proposal-1/decisions", `{"proposal_revision_id":"revision-2","decision":"approve","actor_id":"local-human","idempotency_key":"request-1","reason":"ready"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("decision status = %d, body=%s", response.Code, response.Body.String())
	}
	if mutations.decisionOwner != "proposal-1" || mutations.decisionInput.ProposalRevisionID != "revision-2" || mutations.decisionInput.Decision != sqlite.ProposalDecisionApprove || mutations.decisionInput.ActorKind != sqlite.ProposalDecisionActorHuman || mutations.decisionInput.ActorID != "local-human" || mutations.decisionInput.IdempotencyKey != "request-1" || mutations.decisionInput.Reason != "ready" || !mutations.decisionInput.DecidedAt.Equal(now) {
		t.Fatalf("decision input = %+v owner=%q", mutations.decisionInput, mutations.decisionOwner)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != jsonContentType {
		t.Fatalf("decision headers = %v", response.Header())
	}
}

func TestProposalMutationRoutesRejectMalformedOrUnavailableRequests(t *testing.T) {
	t.Parallel()
	mutations := &fakeProposalMutations{}
	handler := NewProposalMutationHandler(ProposalMutationOptions{Revisions: mutations, Decisions: mutations})
	for _, request := range []struct {
		path, contentType, body string
		want                    int
	}{
		{"/api/v1/mutate/proposals/proposal-1/revisions", "text/plain", `{}`, http.StatusBadRequest},
		{"/api/v1/mutate/proposals/proposal-1/revisions", jsonContentType, `{"unknown":true}`, http.StatusBadRequest},
		{"/api/v1/mutate/proposals/proposal-1/revisions", jsonContentType, `{} {}`, http.StatusBadRequest},
		{"/api/v1/mutate/proposals/proposal-1/decisions", jsonContentType, `{"decision":"approve"}`, http.StatusBadRequest},
		{"/api/v1/mutate/proposals/%20/decisions", jsonContentType, `{}`, http.StatusBadRequest},
	} {
		request := request
		t.Run(request.path+request.body, func(t *testing.T) {
			response := httptest.NewRecorder()
			httpRequest := httptest.NewRequest(http.MethodPost, request.path, strings.NewReader(request.body))
			httpRequest.Header.Set("Content-Type", request.contentType)
			handler.ServeHTTP(response, httpRequest)
			if response.Code != request.want {
				t.Fatalf("status = %d, want %d, body=%s", response.Code, request.want, response.Body.String())
			}
		})
	}

	unavailable := NewProposalMutationHandler(ProposalMutationOptions{})
	response := serveProposalMutation(unavailable, http.MethodPost, "/api/v1/mutate/proposals/proposal-1/revisions", `{}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", response.Code)
	}
}

func TestProposalMutationRoutesMapStoreErrorsAndRequireOuterGuard(t *testing.T) {
	t.Parallel()
	mutations := &fakeProposalMutations{decisionErr: sqlite.ErrProposalNotFound}
	inner := NewProposalMutationHandler(ProposalMutationOptions{Revisions: mutations, Decisions: mutations})
	guarded := newMutationAuth(time.Now).Wrap(inner)
	path := "/api/v1/mutate/proposals/proposal-1/decisions"
	body := `{"proposal_revision_id":"revision-1","decision":"reject","actor_id":"local-human","idempotency_key":"request-1"}`

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	request.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unguarded mutation status = %d", response.Code)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	request.RemoteAddr = "127.0.0.1:1234"
	session := httptest.NewRecorder()
	sessionRequest := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	sessionRequest.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(session, sessionRequest)
	if session.Code != http.StatusNoContent || len(session.Result().Cookies()) != 1 {
		t.Fatalf("session response = %d cookies=%v", session.Code, session.Result().Cookies())
	}
	request.AddCookie(session.Result().Cookies()[0])
	guarded.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "proposal_not_found") {
		t.Fatalf("not found status = %d, body=%s", response.Code, response.Body.String())
	}

	mutations.decisionErr = errors.New("database unavailable")
	response = serveProposalMutation(inner, http.MethodPost, path, body)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "mutation_failed") {
		t.Fatalf("internal failure status = %d, body=%s", response.Code, response.Body.String())
	}
}

func serveProposalMutation(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(response, request)
	return response
}
