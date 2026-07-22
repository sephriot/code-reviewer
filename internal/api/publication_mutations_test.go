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

type fakePublicationMutations struct {
	effectInput     sqlite.CreatePublicationEffectInput
	effect          sqlite.CreatePublicationEffectResult
	effectErr       error
	resolutionInput sqlite.ResolvePublicationUncertaintyInput
	resolution      sqlite.ResolvePublicationUncertaintyResult
	resolutionErr   error
	scheduled       string
	job             sqlite.EnsureJobResult
	scheduleErr     error
}

func (f *fakePublicationMutations) ResolvePublicationUncertainty(_ context.Context, input sqlite.ResolvePublicationUncertaintyInput) (sqlite.ResolvePublicationUncertaintyResult, error) {
	f.resolutionInput = input
	if f.resolutionErr != nil {
		return sqlite.ResolvePublicationUncertaintyResult{}, f.resolutionErr
	}
	return f.resolution, nil
}

func (f *fakePublicationMutations) CreatePublicationEffect(_ context.Context, input sqlite.CreatePublicationEffectInput) (sqlite.CreatePublicationEffectResult, error) {
	f.effectInput = input
	if f.effectErr != nil {
		return sqlite.CreatePublicationEffectResult{}, f.effectErr
	}
	return f.effect, nil
}

func (f *fakePublicationMutations) Schedule(_ context.Context, effectID string) (sqlite.EnsureJobResult, error) {
	f.scheduled = effectID
	if f.scheduleErr != nil {
		return sqlite.EnsureJobResult{}, f.scheduleErr
	}
	return f.job, nil
}

func TestPublicationMutationCreatesDisabledEffectWithoutJob(t *testing.T) {
	t.Parallel()
	mutations := &fakePublicationMutations{effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeDisabled, Created: true}}
	now := time.Unix(200, 0).UTC()
	handler := NewPublicationMutationHandler(PublicationMutationOptions{Effects: mutations, Scheduler: mutations, Now: func() time.Time { return now }})

	response := servePublicationMutation(handler, `{"idempotency_key":"publish:one"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if mutations.effectInput.ProposalRevisionID != "revision-1" || mutations.effectInput.IdempotencyKey != "publish:one" || !mutations.effectInput.CreatedAt.Equal(now) || mutations.scheduled != "" {
		t.Fatalf("effect=%+v scheduled=%q", mutations.effectInput, mutations.scheduled)
	}
	var body publicationResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.EffectID != "effect-1" || body.PublicationMode != "disabled" || !body.Created || body.Job != nil {
		t.Fatalf("body = %+v", body)
	}
}

func TestPublicationMutationSchedulesOnlySimulatedEffect(t *testing.T) {
	t.Parallel()
	mutations := &fakePublicationMutations{
		effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeSimulated},
		job:    sqlite.EnsureJobResult{ID: "job-1", Created: true},
	}
	handler := NewPublicationMutationHandler(PublicationMutationOptions{Effects: mutations, Scheduler: mutations})
	response := servePublicationMutation(handler, `{}`)
	if response.Code != http.StatusOK || mutations.scheduled != "effect-1" {
		t.Fatalf("status=%d scheduled=%q body=%s", response.Code, mutations.scheduled, response.Body.String())
	}
	var body publicationResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Job == nil || body.Job.ID != "job-1" || !body.Job.Created {
		t.Fatalf("body = %+v", body)
	}
}

func TestPublicationDispatchSchedulesOnlyEnabledEffect(t *testing.T) {
	t.Parallel()
	mutations := &fakePublicationMutations{
		effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeEnabled},
		job:    sqlite.EnsureJobResult{ID: "job-1", Created: true},
	}
	handler := NewPublicationMutationHandler(PublicationMutationOptions{Effects: mutations, EnabledScheduler: mutations})
	response := servePublicationDispatch(handler, `{}`)
	if response.Code != http.StatusOK || mutations.scheduled != "effect-1" {
		t.Fatalf("status=%d scheduled=%q body=%s", response.Code, mutations.scheduled, response.Body.String())
	}
	var body publicationResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Job == nil || body.Job.ID != "job-1" || !body.Job.Created {
		t.Fatalf("body = %+v", body)
	}
}

func TestPublicationDispatchFailsClosedOutsideEnabledMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []sqlite.PublicationMode{sqlite.PublicationModeDisabled, sqlite.PublicationModeSimulated} {
		t.Run(string(mode), func(t *testing.T) {
			mutations := &fakePublicationMutations{effectErr: sqlite.ErrPublicationModeNotAllowed}
			response := servePublicationDispatch(NewPublicationMutationHandler(PublicationMutationOptions{Effects: mutations, EnabledScheduler: mutations}), `{}`)
			if response.Code != http.StatusConflict || mutations.scheduled != "" || !strings.Contains(response.Body.String(), "enabled_publication_required") || len(mutations.effectInput.AllowedModes) != 1 || mutations.effectInput.AllowedModes[0] != sqlite.PublicationModeEnabled {
				t.Fatalf("mode=%q status=%d scheduled=%q body=%s", mode, response.Code, mutations.scheduled, response.Body.String())
			}
		})
	}
	missing := &fakePublicationMutations{effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeEnabled}}
	if response := servePublicationDispatch(NewPublicationMutationHandler(PublicationMutationOptions{Effects: missing}), `{}`); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "publication_unavailable") {
		t.Fatalf("missing scheduler status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPublicationMutationFailsClosedForUnavailableSimulationOrStoreConflict(t *testing.T) {
	t.Parallel()
	base := &fakePublicationMutations{effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeSimulated}}
	noScheduler := NewPublicationMutationHandler(PublicationMutationOptions{Effects: base})
	if response := servePublicationMutation(noScheduler, `{}`); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("no scheduler status=%d body=%s", response.Code, response.Body.String())
	}
	conflict := NewPublicationMutationHandler(PublicationMutationOptions{Effects: &fakePublicationMutations{effectErr: sqlite.ErrPublicationAuthorizationNotFound}})
	if response := servePublicationMutation(conflict, `{}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "publication_not_authorized") {
		t.Fatalf("authorization status=%d body=%s", response.Code, response.Body.String())
	}
	failure := NewPublicationMutationHandler(PublicationMutationOptions{Effects: &fakePublicationMutations{effectErr: errors.New("database unavailable")}})
	if response := servePublicationMutation(failure, `{}`); response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "mutation_failed") {
		t.Fatalf("store failure status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPublicationUncertaintyResolutionRequiresExplicitHumanFacts(t *testing.T) {
	t.Parallel()
	now := time.Unix(250, 0).UTC()
	mutations := &fakePublicationMutations{resolution: sqlite.ResolvePublicationUncertaintyResult{ResolutionID: "resolution-1", Created: true}}
	handler := NewPublicationMutationHandler(PublicationMutationOptions{UncertaintyResolver: mutations, Now: func() time.Time { return now }})
	response := servePublicationUncertaintyResolution(handler, `{"resolution":"externally_completed","actor_id":"operator-1","idempotency_key":"resolve:one","reason":"Verified in GitHub."}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if mutations.resolutionInput.EffectID != "effect-1" || mutations.resolutionInput.Resolution != sqlite.PublicationUncertaintyExternallyCompleted || mutations.resolutionInput.ActorID != "operator-1" || mutations.resolutionInput.IdempotencyKey != "resolve:one" || mutations.resolutionInput.Reason != "Verified in GitHub." || !mutations.resolutionInput.ResolvedAt.Equal(now) {
		t.Fatalf("resolution input = %+v", mutations.resolutionInput)
	}
	var body publicationUncertaintyResolutionResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ResolutionID != "resolution-1" || !body.Created {
		t.Fatalf("body = %+v", body)
	}
}

func TestPublicationUncertaintyResolutionFailsClosed(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		store *fakePublicationMutations
		body  string
		want  int
	}{
		{name: "missing resolver", store: &fakePublicationMutations{}, body: `{"resolution":"abandoned","actor_id":"operator-1","idempotency_key":"resolve:one"}`, want: http.StatusServiceUnavailable},
		{name: "not uncertain", store: &fakePublicationMutations{resolutionErr: sqlite.ErrPublicationUncertaintyNotResolvable}, body: `{"resolution":"abandoned","actor_id":"operator-1","idempotency_key":"resolve:one"}`, want: http.StatusConflict},
		{name: "conflict", store: &fakePublicationMutations{resolutionErr: sqlite.ErrPublicationUncertaintyResolutionConflict}, body: `{"resolution":"abandoned","actor_id":"operator-1","idempotency_key":"resolve:one"}`, want: http.StatusConflict},
		{name: "missing idempotency", store: &fakePublicationMutations{}, body: `{"resolution":"abandoned","actor_id":"operator-1"}`, want: http.StatusBadRequest},
		{name: "invalid", store: &fakePublicationMutations{}, body: `{"resolution":"retry","actor_id":"operator-1","idempotency_key":"resolve:one"}`, want: http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := PublicationMutationOptions{}
			if testCase.name != "missing resolver" {
				options.UncertaintyResolver = testCase.store
			}
			handler := NewPublicationMutationHandler(options)
			response := servePublicationUncertaintyResolution(handler, testCase.body)
			if response.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}

func TestPublicationMutationRejectsMalformedRequestsAndNeedsOuterGuard(t *testing.T) {
	t.Parallel()
	mutations := &fakePublicationMutations{effect: sqlite.CreatePublicationEffectResult{EffectID: "effect-1", PublicationMode: sqlite.PublicationModeDisabled}}
	inner := NewPublicationMutationHandler(PublicationMutationOptions{Effects: mutations})
	for _, testCase := range []struct{ path, contentType, body string }{
		{"/api/v1/mutate/proposal-revisions/%20/publication/simulate", jsonContentType, `{}`},
		{"/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", "text/plain", `{}`},
		{"/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", jsonContentType, `{"unknown":true}`},
		{"/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", jsonContentType, `{} {}`},
		{"/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", jsonContentType, `{"idempotency_key":"bad key"}`},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Content-Type", testCase.contentType)
		inner.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s %s status=%d body=%s", testCase.path, testCase.body, response.Code, response.Body.String())
		}
	}
	guarded := newMutationAuth(time.Now).Wrap(inner)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", jsonContentType)
	request.RemoteAddr = "127.0.0.1:1234"
	guarded.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("guarded status=%d", response.Code)
	}
}

func servePublicationMutation(handler http.Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/proposal-revisions/revision-1/publication/simulate", strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(response, request)
	return response
}

func servePublicationDispatch(handler http.Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/proposal-revisions/revision-1/publication/dispatch", strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(response, request)
	return response
}

func servePublicationUncertaintyResolution(handler http.Handler, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/publication-effects/effect-1/uncertainty-resolution", strings.NewReader(body))
	request.Header.Set("Content-Type", jsonContentType)
	handler.ServeHTTP(response, request)
	return response
}
