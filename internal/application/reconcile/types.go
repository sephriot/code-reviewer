// Package reconcile coordinates read-only GitHub discovery and factual projection.
package reconcile

import (
	"context"
	"time"
)

type ScopeKind string

const (
	ScopeReviewRequested ScopeKind = "review_requested_search"
	ScopeAuthored        ScopeKind = "authored_search"
)

type RelationshipKind string

const (
	RelationshipReviewRequested RelationshipKind = "review_requested"
	RelationshipAuthored        RelationshipKind = "authored_by_me"
)

type ObservationSource string

const (
	ObservationReconciliation ObservationSource = "reconciliation"
	ObservationDirectRefresh  ObservationSource = "direct_refresh"
)

type GenerationState string

const (
	GenerationComplete    GenerationState = "complete"
	GenerationPartial     GenerationState = "partial"
	GenerationCapped      GenerationState = "capped"
	GenerationRateLimited GenerationState = "rate_limited"
	GenerationFailed      GenerationState = "failed"
)

type ConnectionInput struct {
	ID                string
	APIBaseURL        string
	AccountLogin      string
	AccountNodeID     string
	AccountDatabaseID int64
	CredentialRefKind string
	CredentialLocator string
	PermissionsJSON   []byte
	CheckedAt         time.Time
}

type Scope struct {
	ConnectionID   string
	Kind           ScopeKind
	Key            string
	QueryPartition string
}

type Generation struct {
	ID     string
	Number int64
	Scope  Scope
}

type RepositoryFacts struct {
	GitHubID int64
	NodeID   string
	FullName string
}

type PullRequestFacts struct {
	GitHubID               int64
	NodeID                 string
	Number                 int
	Title                  string
	AuthorLogin            string
	AuthorDatabaseID       int64
	URL                    string
	State                  string
	IsDraft                bool
	HeadSHA                string
	BaseSHA                string
	BaseRef                string
	BodySHA256             string
	LabelsJSON             []byte
	RequestedReviewersJSON []byte
	RelationshipSetJSON    []byte
	FactsSHA256            string
	GitHubUpdatedAt        time.Time
}

type ProjectionItem struct {
	Repository        RepositoryFacts
	PullRequest       PullRequestFacts
	RelationshipKind  RelationshipKind
	SubjectLogin      string
	SubjectDatabaseID int64
	ObservationSource ObservationSource
}

type ActiveRelationship struct {
	ID                     string
	Kind                   RelationshipKind
	SubjectLogin           string
	SubjectDatabaseID      int64
	RepositoryOwner        string
	RepositoryName         string
	PullRequestNumber      int
	CurrentGitHubUpdatedAt time.Time
}

type RelationshipClosure struct {
	RelationshipID string
}

type ApplyGenerationInput struct {
	Generation         Generation
	State              GenerationState
	PagesExpected      *int
	PagesReceived      int
	ProviderTotal      *int
	ProviderIncomplete bool
	ErrorClass         string
	ErrorMessage       string
	FinishedAt         time.Time
	Items              []ProjectionItem
	Closures           []RelationshipClosure
}

type ApplyGenerationResult struct {
	NewRepositories     int
	NewPullRequests     int
	NewObservations     int
	OpenedRelationships int
	ClosedRelationships int
}

// Store is the transactional persistence boundary for shadow reconciliation.
type Store interface {
	UpsertGitHubConnection(context.Context, ConnectionInput) error
	NextReconciliationGeneration(context.Context, Scope, time.Time) (Generation, error)
	ListActiveRelationships(context.Context, Scope) ([]ActiveRelationship, error)
	ApplyReconciliationGeneration(context.Context, ApplyGenerationInput) (ApplyGenerationResult, error)
}

type ScopeReport struct {
	Scope            ScopeKind             `json:"scope"`
	GenerationID     string                `json:"generation_id"`
	GenerationNumber int64                 `json:"generation_number"`
	State            GenerationState       `json:"state"`
	PagesReceived    int                   `json:"pages_received"`
	ProviderTotal    int                   `json:"provider_total"`
	Candidates       int                   `json:"candidates"`
	Projected        int                   `json:"projected"`
	DetailFailures   int                   `json:"detail_failures"`
	SearchGaps       int                   `json:"search_gaps"`
	StaleDetails     int                   `json:"stale_details"`
	Persistence      ApplyGenerationResult `json:"persistence"`
}

type Report struct {
	ConnectionID string        `json:"connection_id"`
	AccountLogin string        `json:"account_login"`
	Scopes       []ScopeReport `json:"scopes"`
}
