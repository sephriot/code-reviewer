// Package app wires the reviewd foundation service.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/adapters/localnotify"
	"github.com/sephriot/code-reviewer/internal/api"
	"github.com/sephriot/code-reviewer/internal/application/hydrate"
	"github.com/sephriot/code-reviewer/internal/application/hydrateworker"
	"github.com/sephriot/code-reviewer/internal/application/notificationdispatch"
	"github.com/sephriot/code-reviewer/internal/application/notificationoutbox"
	"github.com/sephriot/code-reviewer/internal/application/notificationworker"
	"github.com/sephriot/code-reviewer/internal/application/publishworker"
	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/application/reconcileworker"
	"github.com/sephriot/code-reviewer/internal/application/reviewbundle"
	"github.com/sephriot/code-reviewer/internal/application/reviewexecute"
	"github.com/sephriot/code-reviewer/internal/application/reviewworker"
	"github.com/sephriot/code-reviewer/internal/application/watchschedule"
	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/outbox"
	"github.com/sephriot/code-reviewer/internal/ownership"
	storagesqlite "github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// Service owns the control-plane database and HTTP server.
type Service struct {
	store            *storagesqlite.Store
	server           *http.Server
	jobRunner        runtimeRunner
	outboxRunner     runtimeRunner
	schedule         scheduleFunc
	scheduleInterval time.Duration
	publicationMode  config.PublicationMode
	ownershipGuard   *ownership.Guard
}

type runtimeRunner interface {
	Run(context.Context) error
}

type scheduleFunc func(context.Context) error

type reconciliationScheduler interface {
	Schedule(context.Context, reconcile.Config) (storagesqlite.EnsureJobResult, error)
}

type hydrationScheduler interface {
	Schedule(context.Context, string) ([]storagesqlite.EnsureJobResult, error)
}

// New prepares a service and enforces migration policy before it can listen.
func New(ctx context.Context, cfg config.Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}
	legacy, err := storagesqlite.IsLegacyDatabase(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("inspect database identity: %w", err)
	}
	if legacy {
		return nil, errors.New("refusing to use a legacy database as the v2 control-plane database")
	}
	store, err := storagesqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	var ownershipGuard *ownership.Guard
	closeOnError := func(err error) (*Service, error) {
		if ownershipGuard != nil {
			_ = ownershipGuard.Close()
		}
		_ = store.Close()
		return nil, err
	}

	if cfg.MigrationMode == config.MigrationApply {
		if _, err := store.ApplyMigrations(ctx); err != nil {
			return closeOnError(fmt.Errorf("apply migrations: %w", err))
		}
	}
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		return closeOnError(fmt.Errorf("read schema status: %w", err))
	}
	if status.Pending != 0 || status.Current != status.Latest {
		return closeOnError(fmt.Errorf(
			"database schema is not current: current=%d latest=%d pending=%d",
			status.Current,
			status.Latest,
			status.Pending,
		))
	}
	if _, err := store.SetPublicationMode(ctx, storagesqlite.PublicationMode(cfg.PublicationMode), time.Now().UTC()); err != nil {
		return closeOnError(fmt.Errorf("set publication mode: %w", err))
	}

	if cfg.PublicationMode == config.PublicationEnabled {
		ownershipGuard, err = ownership.Acquire(ctx, filepath.Join(filepath.Dir(cfg.DatabasePath), "writer-ownership"), workerOwner(), "reviewd-enabled-publication", time.Now().UTC())
		if err != nil {
			return closeOnError(fmt.Errorf("acquire writer ownership: %w", err))
		}
	}
	mutationAuth, err := api.NewMutationAuth()
	if err != nil {
		return closeOnError(fmt.Errorf("create control mutation auth: %w", err))
	}
	webhookReconciliation := cfg.ShadowReconciliation
	if cfg.PublicationMode == config.PublicationEnabled {
		// Existing reconciliation writes projections only while the durable
		// publication gate is disabled. Enabled dispatch validates live diff
		// directly, so suppress webhook-triggered projection writes here.
		webhookReconciliation.Enabled = false
	}
	webhookOptions, err := githubWebhookOptions(cfg.GitHubWebhook, webhookReconciliation, store, os.LookupEnv)
	if err != nil {
		return closeOnError(fmt.Errorf("configure github webhook ingress: %w", err))
	}
	controlAPI := api.NewControlHandler(api.Readiness{
		Ping: store.Ping,
		SchemaStatus: func(ctx context.Context) (api.SchemaStatus, error) {
			status, err := store.SchemaStatus(ctx)
			return api.SchemaStatus{
				Current: status.Current,
				Latest:  status.Latest,
				Pending: status.Pending,
			}, err
		},
	}, api.ControlOptions{
		Reader:                  store,
		ProposalMutations:       api.ProposalMutationOptions{Revisions: store, Decisions: store},
		PublicationMutations:    publicationMutationOptions(store),
		PublicationStatuses:     api.PublicationEffectStatusOptions{Reader: store},
		NotificationPreferences: api.NotificationPreferencesOptions{Store: store},
		BrowserNotifications:    api.BrowserNotificationDeliveryOptions{Store: store},
		HydrationMutations:      dashboardHydrationMutationOptions(store),
		ReviewScheduling:        dashboardReviewSchedulingOptions(store, cfg.ReviewExecution.Enabled),
		ReviewExecutionEnabled:  cfg.ReviewExecution.Enabled,
		GitHubWebhooks:          webhookOptions,
	})
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           mutationAuth.Wrap(controlAPI),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	reconcileHandler := reconcileworker.Handler{
		Store:              store,
		NewReader:          environmentReaderFactory(os.LookupEnv),
		HydrationScheduler: hydrateworker.Scheduler{Store: store},
	}
	hydrateHandler := hydrateworker.Handler{
		Store: store,
		NewReader: environmentHydrationReaderFactory(os.LookupEnv, reconcile.Config{
			ConnectionID:      cfg.ShadowReconciliation.ConnectionID,
			APIBaseURL:        cfg.ShadowReconciliation.APIBaseURL,
			CredentialRefKind: "environment",
			CredentialLocator: "env:" + cfg.ShadowReconciliation.TokenEnvironment,
		}),
	}
	if cfg.ReviewExecution.Enabled {
		hydrateHandler.AutomaticScheduler = watchschedule.Service{Store: store}
		hydrateHandler.AutomaticRequest = watchschedule.Request{
			EngineKind: "cli", EngineConfigJSON: []byte(`{"engine_source":"reviewd_config"}`),
			AccessMode: "diff_only", CorrelationID: "reviewd-watch-rule",
		}
	}
	handlers := map[string]worker.Handler{
		reconcileworker.ReconcileJobKind:  reconcileHandler,
		hydrateworker.HydrateJobKind:      hydrateHandler,
		notificationworker.DeliverJobKind: notificationworker.Handler{Loader: store, Recorder: store, Preferences: store, LocalNotifier: localnotify.Local{}},
	}
	if cfg.ReviewExecution.Enabled {
		reviewHandler, err := newReviewExecutionHandler(cfg, store)
		if err != nil {
			return closeOnError(err)
		}
		reviewHandler.AutomaticPublication = automaticPublication{store: store}
		handlers[reviewworker.ExecuteJobKind] = reviewHandler
	}
	if cfg.PublicationMode == config.PublicationSimulated {
		handlers[publishworker.SimulateJobKind] = publishworker.Handler{Loader: store, Recorder: store}
	}
	if cfg.PublicationMode == config.PublicationEnabled {
		handler, err := newEnabledPublicationHandler(cfg, store, os.LookupEnv, ownershipGuard)
		if err != nil {
			return closeOnError(err)
		}
		handlers[publishworker.EnabledJobKind] = handler
	}
	router, err := worker.NewRouter(handlers)
	if err != nil {
		return closeOnError(fmt.Errorf("configure worker handlers: %w", err))
	}
	runner := &worker.Runner{
		Store:   store,
		Handler: router,
		Owner:   workerOwner(),
	}
	outboxRunner := &outbox.Runner{
		Store: store,
		Handler: notificationoutbox.Handler{Dispatcher: notificationdispatch.Service{
			Store: store, Scheduler: notificationworker.Scheduler{Store: store},
		}},
		Owner: workerOwner() + ":outbox",
	}
	service := &Service{store: store, server: server, jobRunner: runner, outboxRunner: outboxRunner, publicationMode: cfg.PublicationMode, ownershipGuard: ownershipGuard}
	if cfg.ShadowReconciliation.Enabled && cfg.PublicationMode != config.PublicationEnabled {
		reconciliationConfig := reconcile.Config{
			ConnectionID:      cfg.ShadowReconciliation.ConnectionID,
			APIBaseURL:        cfg.ShadowReconciliation.APIBaseURL,
			CredentialRefKind: "environment",
			CredentialLocator: "env:" + cfg.ShadowReconciliation.TokenEnvironment,
		}
		reconciliationScheduler := reconcileworker.Scheduler{Store: store}
		hydrationScheduler := hydrateworker.Scheduler{Store: store}
		service.schedule = func(ctx context.Context) error {
			return scheduleShadow(ctx, reconciliationScheduler, hydrationScheduler, reconciliationConfig)
		}
		service.scheduleInterval = cfg.ShadowReconciliation.Interval
	}
	return service, nil
}

type automaticPublication struct{ store *storagesqlite.Store }

func (p automaticPublication) PublishAutomaticApproval(ctx context.Context, proposalRevisionID string) error {
	if p.store == nil {
		return errors.New("automatic publication store is unavailable")
	}
	_, err := p.store.CreatePublicationEffectAndEnsureJob(ctx, storagesqlite.CreatePublicationEffectInput{ProposalRevisionID: proposalRevisionID}, automaticPublicationJob)
	if err != nil {
		return err
	}
	return nil
}

func automaticPublicationJob(effectID string, mode storagesqlite.PublicationMode) (storagesqlite.JobInput, error) {
	payload, err := json.Marshal(struct {
		EffectID string `json:"effect_id"`
	}{EffectID: effectID})
	if err != nil {
		return storagesqlite.JobInput{}, err
	}
	switch mode {
	case storagesqlite.PublicationModeSimulated:
		return storagesqlite.JobInput{Kind: publishworker.SimulateJobKind, ResourceType: "publication_effect", ResourceID: effectID, DedupeKey: publishworker.SimulateJobKind + ":" + effectID, Payload: payload, MaxAttempts: 3}, nil
	case storagesqlite.PublicationModeEnabled:
		return storagesqlite.JobInput{Kind: publishworker.EnabledJobKind, ResourceType: "publication_effect", ResourceID: effectID, DedupeKey: publishworker.EnabledJobKind + ":" + effectID, Payload: payload, MaxAttempts: 1}, nil
	default:
		return storagesqlite.JobInput{}, errors.New("automatic publication mode is unsupported")
	}
}

func githubWebhookOptions(cfg config.GitHubWebhookConfig, reconciliation config.ShadowReconciliationConfig, store *storagesqlite.Store, lookup func(string) (string, bool)) (api.GitHubWebhookOptions, error) {
	if !cfg.Enabled {
		return api.GitHubWebhookOptions{}, nil
	}
	if store == nil || lookup == nil {
		return api.GitHubWebhookOptions{}, errors.New("github webhook ingress dependencies are unavailable")
	}
	secret, found := lookup(cfg.SecretEnvironment)
	if !found || len(secret) < 16 || len(secret) > 4096 {
		return api.GitHubWebhookOptions{}, errors.New("github webhook signing secret is unavailable")
	}
	options := api.GitHubWebhookOptions{Enabled: true, Secret: []byte(secret), Store: store}
	if reconciliation.Enabled {
		scheduler := reconcileworker.Scheduler{Store: store}
		reconciliationConfig := reconcile.Config{ConnectionID: reconciliation.ConnectionID, APIBaseURL: reconciliation.APIBaseURL, CredentialRefKind: "environment", CredentialLocator: "env:" + reconciliation.TokenEnvironment}
		options.Scheduler = webhookReconciliationScheduler{scheduler: scheduler, config: reconciliationConfig}
	}
	return options, nil
}

type webhookReconciliationScheduler struct {
	scheduler reconcileworker.Scheduler
	config    reconcile.Config
}

func (s webhookReconciliationScheduler) Schedule(ctx context.Context) error {
	if _, err := s.scheduler.Schedule(ctx, s.config); err != nil {
		return fmt.Errorf("schedule webhook reconciliation: %w", err)
	}
	return nil
}

func publicationMutationOptions(store *storagesqlite.Store) api.PublicationMutationOptions {
	return api.PublicationMutationOptions{AtomicEffects: store, UncertaintyResolver: store}
}

func dashboardHydrationMutationOptions(store *storagesqlite.Store) api.HydrationMutationOptions {
	return api.HydrationMutationOptions{Scheduler: dashboardHydrationScheduler{scheduler: hydrateworker.TargetScheduler{Store: store}}}
}

func dashboardReviewSchedulingOptions(store *storagesqlite.Store, enabled bool) api.ReviewSchedulingOptions {
	if !enabled {
		return api.ReviewSchedulingOptions{}
	}
	return api.ReviewSchedulingOptions{Scheduler: dashboardReviewScheduler{service: watchschedule.Service{Store: store}}}
}

type dashboardReviewScheduler struct {
	service watchschedule.Service
}

func (s dashboardReviewScheduler) ScheduleEligibleReview(ctx context.Context, connectionID, pullRequestID string) (api.EligibleReviewScheduleResult, error) {
	result, err := s.service.Schedule(ctx, watchschedule.Request{
		ConnectionID: connectionID, PullRequestID: pullRequestID,
		EngineKind: "cli", EngineConfigJSON: []byte(`{"engine_source":"reviewd_config"}`),
		AccessMode: "diff_only", CorrelationID: "reviewd-dashboard", RequestedAt: time.Now().UTC(),
	})
	if errors.Is(err, storagesqlite.ErrAutomaticWatchRuleTargetNotFound) || errors.Is(err, storagesqlite.ErrCanonicalReviewTargetNotFound) {
		return api.EligibleReviewScheduleResult{}, api.ErrReviewEvidenceNotReady
	}
	if err != nil {
		return api.EligibleReviewScheduleResult{}, err
	}
	response := api.EligibleReviewScheduleResult{Matched: result.Matched, RuleID: result.RuleID, RuleVersionID: result.RuleVersionID, TriggerKind: result.TriggerKind}
	if result.Queued != nil {
		response.RunID, response.JobID, response.Created = result.Queued.RunID, result.Queued.JobID, result.Queued.Created
	}
	return response, nil
}

type dashboardHydrationScheduler struct {
	scheduler hydrateworker.TargetScheduler
}

func (s dashboardHydrationScheduler) SchedulePullRequest(ctx context.Context, connectionID, pullRequestID string) (api.HydrationScheduleResult, error) {
	result, err := s.scheduler.SchedulePullRequest(ctx, connectionID, pullRequestID)
	if errors.Is(err, storagesqlite.ErrCanonicalHydrationTargetNotFound) {
		return api.HydrationScheduleResult{}, api.ErrHydrationTargetNotFound
	}
	if err != nil {
		return api.HydrationScheduleResult{}, err
	}
	return api.HydrationScheduleResult{JobID: result.ID, Created: result.Created}, nil
}

func newReviewExecutionHandler(cfg config.Config, store *storagesqlite.Store) (reviewworker.Handler, error) {
	adapter, err := engine.New(engine.Config{Argv: cfg.ReviewExecution.EngineArgv})
	if err != nil {
		return reviewworker.Handler{}, fmt.Errorf("configure review engine: %w", err)
	}
	reconciliationConfig := reconcile.Config{
		ConnectionID:      cfg.ShadowReconciliation.ConnectionID,
		APIBaseURL:        cfg.ShadowReconciliation.APIBaseURL,
		CredentialRefKind: "environment",
		CredentialLocator: "env:" + cfg.ShadowReconciliation.TokenEnvironment,
	}
	executor := reviewexecute.Service{
		Targets:   store,
		NewReader: environmentReviewReaderFactory(os.LookupEnv, reconciliationConfig),
		Engine:    adapter,
		Recorder:  store,
	}
	return reviewworker.Handler{Executor: executor, Events: store, AutomaticPolicyStore: store}, nil
}

func newEnabledPublicationHandler(cfg config.Config, store *storagesqlite.Store, lookup func(string) (string, bool), guard *ownership.Guard) (publishworker.EnabledHandler, error) {
	if store == nil || lookup == nil || guard == nil {
		return publishworker.EnabledHandler{}, errors.New("enabled publication dependencies are unavailable")
	}
	if err := guard.Valid(context.Background()); err != nil {
		return publishworker.EnabledHandler{}, fmt.Errorf("validate writer ownership before loading GitHub credential: %w", err)
	}
	token, found := lookup(cfg.ShadowReconciliation.TokenEnvironment)
	if !found || strings.TrimSpace(token) == "" {
		return publishworker.EnabledHandler{}, errors.New("enabled publication GitHub token is unavailable")
	}
	reader, err := githubadapter.NewClient(cfg.ShadowReconciliation.APIBaseURL, token, nil)
	if err != nil {
		return publishworker.EnabledHandler{}, fmt.Errorf("configure enabled publication read client: %w", err)
	}
	publisher, err := githubadapter.NewPublisher(cfg.ShadowReconciliation.APIBaseURL, token, nil)
	if err != nil {
		return publishworker.EnabledHandler{}, fmt.Errorf("configure enabled publication writer: %w", err)
	}
	return publishworker.EnabledHandler{Claimer: store, Recorder: store, Reader: reader, Publisher: ownedPublisher{publisher: publisher, guard: guard}}, nil
}

type ownedPublisher struct {
	publisher publishworker.EnabledPublisher
	guard     *ownership.Guard
}

func (p ownedPublisher) SubmitReview(ctx context.Context, submission githubadapter.ReviewSubmission) (githubadapter.SubmittedReview, error) {
	if p.guard == nil {
		return githubadapter.SubmittedReview{}, errors.New("writer ownership guard is unavailable")
	}
	if err := p.guard.Valid(ctx); err != nil {
		return githubadapter.SubmittedReview{}, fmt.Errorf("validate writer ownership before GitHub write: %w", err)
	}
	return p.publisher.SubmitReview(ctx, submission)
}

// Run listens until the context is canceled or the server fails.
func (s *Service) Run(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}
	logger.Info("reviewd listening", "address", listener.Addr().String(), "publication_mode", s.publicationMode)

	runtimeCtx, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	backgroundErrors := make(chan error, 2)
	var background sync.WaitGroup
	s.startBackground(runtimeCtx, logger, &background, backgroundErrors)

	serveError := make(chan error, 1)
	go func() {
		serveError <- s.server.Serve(listener)
	}()

	select {
	case err := <-serveError:
		cancelRuntime()
		background.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve control API: %w", err)
	case err := <-backgroundErrors:
		cancelRuntime()
		shutdownErr := s.shutdownServer()
		serveErr := <-serveError
		background.Wait()
		if shutdownErr != nil {
			return fmt.Errorf("runtime failed: %w; shut down control API: %v", err, shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("runtime failed: %w; serve control API during shutdown: %v", err, serveErr)
		}
		return fmt.Errorf("run control-plane runtime: %w", err)
	case <-ctx.Done():
		cancelRuntime()
		if err := s.shutdownServer(); err != nil {
			background.Wait()
			return fmt.Errorf("shut down control API: %w", err)
		}
		if err := <-serveError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			background.Wait()
			return fmt.Errorf("serve control API during shutdown: %w", err)
		}
		background.Wait()
		return nil
	}
}

func (s *Service) startBackground(ctx context.Context, logger *slog.Logger, group *sync.WaitGroup, errorsCh chan<- error) {
	if s.jobRunner != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.jobRunner.Run(ctx); err != nil && ctx.Err() == nil {
				errorsCh <- fmt.Errorf("run worker: %w", err)
			}
		}()
	}
	if s.outboxRunner != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.outboxRunner.Run(ctx); err != nil && ctx.Err() == nil {
				errorsCh <- fmt.Errorf("run outbox: %w", err)
			}
		}()
	}
	if s.schedule != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			logger.Info("shadow reconciliation scheduler started", "interval", s.scheduleInterval)
			schedule := func(ctx context.Context) error {
				logger.Info("scheduling shadow reconciliation and hydration")
				err := s.schedule(ctx)
				if err != nil {
					logger.Error("could not schedule shadow reconciliation", "error", err)
					return err
				}
				logger.Info("shadow reconciliation and hydration scheduled")
				return nil
			}
			if err := runScheduler(ctx, s.scheduleInterval, schedule); err != nil && ctx.Err() == nil {
				errorsCh <- err
			}
		}()
	}
}

func (s *Service) shutdownServer() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
}

func runScheduler(ctx context.Context, interval time.Duration, schedule scheduleFunc) error {
	if schedule == nil {
		return nil
	}
	if interval <= 0 {
		return errors.New("shadow reconciliation schedule interval must be positive")
	}
	if err := schedule(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := schedule(ctx); err != nil {
				return err
			}
		}
	}
}

func scheduleShadow(
	ctx context.Context,
	reconciliation reconciliationScheduler,
	hydration hydrationScheduler,
	reconciliationConfig reconcile.Config,
) error {
	if _, err := reconciliation.Schedule(ctx, reconciliationConfig); err != nil {
		return fmt.Errorf("schedule shadow reconciliation: %w", err)
	}
	if _, err := hydration.Schedule(ctx, reconciliationConfig.ConnectionID); err != nil {
		return fmt.Errorf("schedule canonical hydration: %w", err)
	}
	return nil
}

func environmentReaderFactory(lookup func(string) (string, bool)) reconcileworker.ReaderFactory {
	return func(_ context.Context, reconciliationConfig reconcile.Config) (githubadapter.Reader, error) {
		if reconciliationConfig.CredentialRefKind != "environment" {
			return nil, worker.Permanent(fmt.Errorf("unsupported GitHub credential reference kind %q", reconciliationConfig.CredentialRefKind))
		}
		if lookup == nil {
			return nil, worker.Permanent(errors.New("GitHub environment credential lookup is required"))
		}
		locator := strings.TrimPrefix(reconciliationConfig.CredentialLocator, "env:")
		token, ok := lookup(locator)
		if !ok || strings.TrimSpace(token) == "" {
			return nil, worker.Permanent(fmt.Errorf("GitHub token environment %q is not set", locator))
		}
		reader, err := githubadapter.NewClient(reconciliationConfig.APIBaseURL, token, nil)
		if err != nil {
			return nil, worker.Permanent(fmt.Errorf("create GitHub read client: %w", err))
		}
		return reader, nil
	}
}

func environmentHydrationReaderFactory(
	lookup func(string) (string, bool),
	reconciliationConfig reconcile.Config,
) hydrateworker.ReaderFactory {
	reconciliationReader := environmentReaderFactory(lookup)
	return func(ctx context.Context, connectionID string) (hydrate.Reader, error) {
		if connectionID != reconciliationConfig.ConnectionID {
			return nil, worker.Permanent(fmt.Errorf("GitHub hydration connection %q is not configured", connectionID))
		}
		reader, err := reconciliationReader(ctx, reconciliationConfig)
		if err != nil {
			return nil, err
		}
		hydrationReader, ok := reader.(hydrate.Reader)
		if !ok {
			return nil, worker.Permanent(errors.New("GitHub read client does not support canonical hydration"))
		}
		return hydrationReader, nil
	}
}

func environmentReviewReaderFactory(
	lookup func(string) (string, bool),
	reconciliationConfig reconcile.Config,
) reviewexecute.ReaderFactory {
	reconciliationReader := environmentReaderFactory(lookup)
	return func(ctx context.Context, connectionID string) (reviewbundle.Reader, error) {
		if connectionID != reconciliationConfig.ConnectionID {
			return nil, worker.Permanent(fmt.Errorf("GitHub review connection %q is not configured", connectionID))
		}
		reader, err := reconciliationReader(ctx, reconciliationConfig)
		if err != nil {
			return nil, err
		}
		reviewReader, ok := reader.(reviewbundle.Reader)
		if !ok {
			return nil, worker.Permanent(errors.New("GitHub read client does not support review evidence"))
		}
		return reviewReader, nil
	}
}

func workerOwner() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "unknown-host"
	}
	return "reviewd:" + hostname + ":" + strconv.Itoa(os.Getpid())
}

// Close releases service resources.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	err := s.store.Close()
	if s.ownershipGuard != nil {
		err = errors.Join(err, s.ownershipGuard.Close())
	}
	return err
}
