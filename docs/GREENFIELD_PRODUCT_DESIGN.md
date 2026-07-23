# Code Reviewer 2.0: Greenfield Product and Technical Design

**Status:** Proposed  
**Date:** 2026-07-21  
**Audience:** Product, design, backend, frontend, platform, and test engineering  
**Scope:** Ground-up replacement of the current application, preserving valuable behavior while redesigning the product as one coherent review control plane

## 1. Executive summary

Code Reviewer 2.0 is a local-first pull-request review control plane. It discovers work from GitHub, creates immutable PR revision snapshots, runs one or more review engines, validates their findings, applies user-owned automation policy, asks for human decisions when required, and publishes only explicitly authorized effects to GitHub.

The central redesign is conceptual, not cosmetic:

1. **GitHub facts** describe what exists.
2. **Review assessments** describe what an engine concluded.
3. **Policy evaluations** decide what the product is allowed to do.
4. **Human decisions** record user intent.
5. **Publications** record external GitHub effects.

The current product mixes these concerns into action enums, several overlapping tables, polling loops, and UI tabs. Version 2 models them separately and presents every actionable item through one inbox and one PR timeline.

The recommended implementation is a modular monolith:

- Go 1.26 backend and worker runtime
- React and TypeScript browser UI built with Vite
- SQLite in local/single-instance mode, with WAL enabled
- PostgreSQL-compatible repository boundary for future team or multi-instance mode
- GitHub webhooks where reachable, plus mandatory periodic reconciliation
- GitHub user credentials for local mode and GitHub App installation credentials for server mode
- Provider-neutral review engine adapters supporting installed CLIs and direct APIs
- Durable database-backed jobs, outbox events, leases, retries, and idempotency
- One embedded web distribution and one service binary per target platform

This remains intentionally one deployable service. Microservices, Kafka, Temporal, Electron, and Kubernetes are not required for the product described here.

## 2. Assumptions

This design proceeds with the following assumptions because product direction was intentionally unconstrained:

- Primary user is one developer running the product on a trusted workstation.
- Architecture must not block a future small self-hosted team deployment.
- Existing local Claude, Codex, and Agent CLI subscriptions remain useful.
- Direct LLM APIs are also acceptable, especially for unattended server deployment.
- Existing SQLite history should be migrated, not discarded.
- GitHub is the only source-control provider in version 2.
- The product may analyze untrusted pull-request content but must not execute it by default.
- Automated GitHub approval remains available, but only through explicit user policy.
- Browser UI remains the main control surface. CLI provides setup, diagnostics, and automation.

If the target becomes hosted multi-tenant SaaS, this document remains a domain blueprint, but authentication, tenancy, billing, isolation, secret management, and PostgreSQL become phase-one requirements rather than extension points.

## 3. Product problem

Review automation has two distinct jobs:

- analyze code accurately;
- safely coordinate attention and GitHub actions.

The current application performs both, but features grew as separate paths: assigned reviews, own PRs, pending approvals, human reviews, cached review requests, history, analytics, sounds, and re-review actions. Each path owns some state and some UI. Users must understand implementation categories to understand their work.

Version 2 should answer four questions immediately:

1. What needs attention now?
2. What did the reviewer conclude for this exact commit?
3. What will happen if I approve this proposal?
4. What happened previously, and why?

### 3.1 Current-state evidence

The rewrite is justified by boundary and lifecycle problems, not by file size alone:

- Assigned-PR orchestration combines stale-state cleanup, optional GitHub marker comments, audio, engine execution, failure classification, action selection, and persistence in one flow (`src/code_reviewer/github_monitor.py`, `_process_pr`).
- The web server defines transport, background task creation, GitHub publication, state transitions, re-review behavior, analytics, and error mapping inside one route-registration method (`src/code_reviewer/web_server.py`, `_setup_routes`). Some accepted work exists only as an in-memory `asyncio` task and disappears on restart.
- `pr_reviews`, `pending_approvals`, `own_prs`, `review_requests`, and `review_started_comments` describe overlapping parts of one PR lifecycle with different status vocabularies.
- One global review lock converts valid concurrent demand into “busy” responses and pauses both discovery loops during a long review.
- Timeout, parser failure, unexpected runtime error, and genuine model uncertainty all become `requires_human_review`; operational availability and code judgment are therefore indistinguishable.
- A crash after GitHub accepts a review but before local commit can reset `posting` to `pending`, permitting duplicate publication because the GitHub review ID and uncertain outcome are not reconciled.
- Several same-SHA re-review paths delete prior rows before replacement, weakening the claimed audit trail.
- Configuration, dry-run, and re-review behavior have path-specific differences because routes and monitor loops each implement parts of policy.

A disciplined Python refactor could correct these problems. Greenfield Go is recommended because the requested scope allows a clean domain reset and values single-binary distribution, but the domain model in this document is the essential design.

## 4. Goals and non-goals

### 4.1 Goals

- Present one trustworthy operational inbox.
- Prevent duplicate GitHub effects in every locally controllable path; ambiguous external outcomes stop for verification rather than retrying blindly.
- Never apply a decision made for an outdated commit.
- Preserve a complete audit trail from trigger through publication.
- Separate engine assessment from automation policy.
- Support assigned PRs and the user's own PRs through the same lifecycle.
- Support automatic, manual, and on-demand review policies.
- Allow users to edit proposed body and inline comments before publication.
- Support review continuity across new commits without treating old findings as current facts.
- Support CLI and API review engines through one contract.
- Make failures visible, retryable, and diagnostically useful.
- Keep local installation simple and resource use modest.
- Preserve responsive, accessible desktop and mobile browser workflows.

### 4.2 Non-goals for version 2

- General-purpose CI execution.
- Running arbitrary PR tests or build scripts by default.
- Replacing GitHub's review UI or branch protection.
- Autonomous code changes or merge execution.
- Multiple source-control providers.
- Hosted multi-tenant SaaS.
- Organization-wide policy administration and RBAC.
- Chat, Slack, or Teams as primary interfaces.
- Training or fine-tuning models from private source code.

### 4.3 User roles and jobs

Version 2 has one authenticated local actor, but distinguishes three roles for product design:

- **Reviewer:** wants assigned PRs assessed promptly, evidence grouped by risk, and exact GitHub action preview before commitment.
- **Author:** wants an advisory review of own PR, clear ready/attention signal, and no impossible self-approval workflow.
- **Operator:** wants trustworthy queue state, safe automation rules, engine/GitHub health, recoverable failures, cost/latency visibility, backup, and rollback.

Core jobs:

- “Show everything needing my judgment without making me understand internal queues.”
- “Review this exact revision now, with my additional context and chosen profile.”
- “Explain what changed since previous assessment and whether old findings remain relevant.”
- “Let me edit, approve, reject, retry, acknowledge, or abandon work without losing history.”
- “Prove what the system sent to GitHub, under which policy, and for which revision.”
- “Stop all GitHub writes immediately without stopping discovery or analysis.”

## 5. Product principles

### 5.1 Revision is the unit of truth

Every review, proposal, decision, and publication belongs to one immutable revision identified by head SHA, base SHA, and canonical diff hash. A changed head, changed base, or changed canonical diff creates a new revision. Old data remains historical and can be referenced, but cannot be published as current.

### 5.2 Facts, judgment, policy, intent, and effects stay separate

An engine can conclude `pass`, `concerns`, `changes_required`, or `inconclusive`. It does not decide whether the product may approve, request changes, or wait for a human. Policy makes that decision. A human may then confirm or reject it. Publication is a final, independently tracked effect.

### 5.3 Every asynchronous command is durable

Clicking “Review,” receiving a webhook, or approving a proposal creates a durable record before background work begins. Process restart must not lose accepted work.

### 5.4 External actions are explicit and locally deduplicated

Only the publication module holds GitHub write capability. Review engines never receive GitHub write credentials. Every publication effect uses a stable local idempotency key and verifies revision freshness immediately before posting. External exactly-once delivery is not claimed where GitHub offers no idempotency key.

### 5.5 Queues are views, not storage models

“Needs decision,” “human review,” “review requested,” and “my PR needs attention” are derived inbox reasons over common entities. They are not separate lifecycle tables.

### 5.6 Reconciliation repairs missed events

Webhooks improve latency but never become the sole correctness mechanism. Periodic reconciliation compares GitHub state with local state and repairs gaps.

### 5.7 Safe defaults, visible automation

Default policy requires confirmation for any comment or change request. Automatic silent approval must be explicitly enabled and visible on every affected repository/profile.

## 6. Capability inventory and target disposition

| Current capability | Version 2 disposition |
|---|---|
| Discover PRs requesting review | Preserve through webhook plus reconciler |
| Repository and author filters | Replace with ordered watch rules |
| Commit-SHA duplicate prevention | Preserve as revision/run uniqueness |
| Review new commits automatically | Preserve through revision policy |
| Claude, Codex, and Agent CLIs | Preserve as CLI engine adapters |
| Claude model and effort override | Generalize as engine parameters validated by adapter |
| Custom Agent argv | Preserve in trusted engine configuration with capability and isolation validation |
| `SHOW_THINKING` streams | Replace with adapter progress events; do not require or retain private chain-of-thought |
| Output-format prompt override | Replace with profile-owned versioned schema/instructions |
| Prompt packs | Replace with versioned review profiles |
| Optional Atlas prompt context | Preserve as a context-provider extension |
| Four model actions | Replace with assessment verdict plus policy disposition |
| Auto-approve without comment | Preserve behind explicit policy |
| Human-gated approval/change request | Preserve as proposals and decisions |
| Requires-human-review path | Preserve as an inbox reason produced by policy or failure |
| Inline comments | Preserve with stronger anchors and validation |
| Dropped invalid inline feedback | Preserve as non-inline findings with explanation |
| Re-review with user context | Preserve as run instructions stored with the run |
| Previous pending-review context | Preserve as selected prior-run context, never implicit mutation |
| Global single-flight execution | Replace with configurable leased concurrency |
| Review timeout | Preserve per profile/engine |
| Invalid output fallback parsing | Keep only for CLI adapters; prefer schema-constrained API output |
| Dry-run | Replace with explicit publication mode: disabled, simulated, enabled |
| Review-started eye comment | Preserve as optional ephemeral publication artifact |
| Own PR off/auto/manual | Replace with watch-rule policy and `relationship=authored_by_me` |
| Own PR ready/attention states | Derive from latest assessment and PR state |
| Pending content editing | Preserve with proposal revisions |
| Approve/reject history | Preserve as immutable decisions and audit events |
| Merged/closed and expired cleanup | Preserve through PR/revision state projection |
| Review-request cache | Replace with canonical PR projection plus last-reconciled metadata |
| Sound files, TTS, templates, mute | Preserve as notification preferences and channel adapter |
| Startup sound demo | Replace automatic demo with explicit notification preview/diagnostic command |
| New-review discovery sound | Preserve as `review_observed` notification event |
| Dashboard operational counts | Preserve as inbox facets |
| Completed/approved/rejected history | Consolidate into PR timeline and global history |
| Analytics by time, action, repository, author | Preserve and extend from normalized events |
| Localhost web UI without authentication | Replace with authenticated loopback session; refuse unsecured non-loopback binding |

## 7. Core domain model

### 7.1 Terms

- **Pull Request:** Mutable GitHub work item.
- **Revision:** Immutable reviewable snapshot identified by repository, PR number, head SHA, base SHA, and canonical diff hash.
- **Relationship:** Why the PR matters to this installation: `review_requested`, `authored_by_me`, `watched`, or any combination.
- **Review Profile:** Versioned instructions, context rules, engine defaults, timeout, and validation requirements.
- **Review Intent:** Durable request to assess one revision, including trigger source, user context, selected profile, and idempotency key.
- **Review Run:** One execution attempt under an intent. Retrying creates another run rather than overwriting the failed attempt.
- **Assessment:** Normalized engine conclusion and limitations.
- **Finding:** One concrete observation, optionally anchored to a diff line.
- **Policy Evaluation:** Deterministic mapping from facts and assessment to a proposed disposition.
- **Proposal:** Editable draft of a potential GitHub review effect.
- **Proposal Revision:** Immutable version of edited proposal content.
- **Decision:** Human or policy actor acceptance/rejection of one proposal revision.
- **Publication Effect:** One authorized intention to create, update, or delete an external GitHub artifact. Its typed owner may be a proposal revision or an operational lifecycle object such as a review-start marker.
- **Publication Attempt:** One network attempt to realize a publication effect.
- **Attention Reason:** Derived reason an item appears in inbox.
- **Domain Event:** Immutable fact emitted after committed state change.

Canonical diff hash is SHA-256 over a versioned, deterministic manifest sorted by path. Each entry contains path, change type, base blob SHA, head blob SHA, file modes, binary flag, and normalized patch hash when text is available. Hash algorithm version is stored with revision so canonicalization may evolve safely.

```text
Pull Request
  Revision
    Review Intent
      Review Run attempt(s)
        Assessment
          Findings
          Policy Evaluation
            Proposal
              Proposal Revision(s)
                Decision
                  Publication Effect
                    Publication Attempt(s)
    Operational Publication Effect(s), such as marker create/delete
      Publication Attempt(s)
```

### 7.2 Assessment contract

Review engines return judgment, not GitHub commands:

```json
{
  "schema_version": 1,
  "verdict": "pass | concerns | changes_required | inconclusive",
  "summary": "Short revision-level assessment",
  "confidence": "high | medium | low",
  "limitations": ["What could not be verified"],
  "coverage": {
    "status": "complete | partial | unknown",
    "changed_files_total": 12,
    "reviewed_files": 12,
    "omitted": [{"path": "generated.lock", "reason": "profile exclusion"}]
  },
  "findings": [
    {
      "client_id": "stable-within-output",
      "severity": "blocker | high | medium | low | note",
      "category": "correctness | security | performance | testing | maintainability | other",
      "path": "src/example.go",
      "line": 42,
      "side": "RIGHT",
      "message": "Problem and impact",
      "evidence": "Why this conclusion follows from the diff",
      "suggestion": "Optional remediation"
    }
  ]
}
```

Validator requirements:

- reject unknown schema versions;
- validate enum values, sizes, and required fields;
- ensure every path belongs to the revision;
- verify line and side against the stored diff;
- recompute a finding fingerprint from category, normalized path, anchor, and message;
- downgrade invalid inline anchors to unanchored findings rather than losing feedback;
- record validation warnings separately from engine limitations;
- compare declared coverage with the canonical changed-file manifest;
- force coverage to `partial` when any changed file, patch segment, binary change, generated-file exclusion, or context-limit omission was not assessed;
- cap body, finding count, and message sizes before storage/publication.

Policy must never select automatic approval when coverage is `partial` or `unknown`. A profile may intentionally exclude files, but exclusions remain visible and require confirmation or `no_external_action`.

### 7.3 Policy disposition

Policy produces one of:

- `no_external_action`
- `auto_publish_approval`
- `propose_approval`
- `propose_comment`
- `propose_changes`
- `require_human_review`

Assessment and disposition are deliberately not one enum. Example: a `pass` assessment can become `no_external_action` on the user's own PR, `propose_approval` in a cautious repository, or `auto_publish_approval` in an explicitly trusted repository. `no_external_action` remains auditable; ignoring a PR happens earlier in watch-rule evaluation and creates no review intent.

### 7.4 Main state machines

#### Review intent

```text
observed -> queued -> running -> completed
                   -> failed -> queued

observed|queued|running -> canceled
                        -> superseded
                        -> closed
```

An intent owns one or more run attempts. It records why work exists: active assigned-review request, authored-PR policy, watch rule, user command, or retry. A new current revision supersedes active intents for the old revision, including base-only changes. An assigned-review intent is eligible only while GitHub still reports an active review request unless a user explicitly starts an on-demand run.

#### Review run attempt

```text
queued
  -> preparing
  -> running
  -> validating
  -> succeeded

queued|preparing|running|validating
  -> canceled
  -> failed_retryable
  -> failed_terminal
  -> superseded

failed_retryable (terminal for this attempt; application may create a new queued attempt)
```

A run becomes `superseded` when a newer revision exists. Running work may be canceled to save cost, but completed output remains historical.

#### Proposal

```text
draft -> awaiting_decision -> rejected
                         -> approved

draft|awaiting_decision|approved -> superseded
draft|awaiting_decision          -> withdrawn

draft -> approved  (policy actor, only for an auto-publication disposition)
```

Only `awaiting_decision` may be edited. Editing creates a new immutable proposal revision and invalidates any earlier unsigned decision. A proposal can be approved only when its revision and observation targets are current. For `auto_publish_approval`, the policy transaction creates the proposal revision, policy-actor decision, publication effect, domain event, and outbox row atomically; no human-decision state is simulated.

#### Publication effect

```text
planned -> dispatching -> succeeded
                      -> definite_failure -> dispatching
                      -> uncertain -> succeeded
                                   -> dispatching  (explicit human authorization only)
                                   -> abandoned

planned|definite_failure -> superseded
```

An effect is immutable in purpose, typed owner, target revision, target observation, rendered payload hash, and correlation key. A retry creates another attempt on the same effect; changed content or target creates a new effect. `uncertain` means GitHub may have accepted the effect but local confirmation was lost. Verification must run before human-authorized retry. Proposal-backed effects require an effective approval decision. Operational effects, such as marker create/delete, are authorized by their lifecycle policy and do not require a proposal.

#### Publication attempt

```text
created -> request_started -> succeeded
                           -> definite_failure
                           -> uncertain
```

Publication attempt states are `created`, `request_started`, `succeeded`, `definite_failure`, and `uncertain`. Attempts are immutable after terminal state. Only effect-level transition rules decide whether a new attempt may be created.

#### Pull request

GitHub owns `open`, `closed`, `merged`, and `draft`. Local state projects those facts. Closing or merging a PR supersedes pending proposals and cancels queued runs. Reopening creates no new revision unless SHA changed, but policy may schedule a new run.

## 8. Unified control-plane experience

### 8.1 Navigation

Version 2 has five primary destinations:

1. **Inbox** — everything requiring attention.
2. **Pull requests** — searchable inventory across all observed PRs.
3. **Runs** — execution queue, live progress, failures, and diagnostics.
4. **History and analytics** — outcomes, decisions, publications, and trends.
5. **Settings** — GitHub connection, engines, profiles, policies, notifications, and retention.

“My PRs” is a saved filter, not a separate subsystem. “Pending decisions,” “human reviews,” and “review requests” are inbox facets, not top-level product concepts.

### 8.2 Inbox

Each card represents one PR and current revision. Multiple attention reasons appear on the same card:

- review requested but no current run;
- review queued or running;
- proposal awaiting decision;
- engine inconclusive;
- run failed;
- publication failed;
- authored PR needs attention;
- GitHub access or state is stale.

Default ordering:

1. failed publication;
2. requested changes awaiting decision;
3. other proposal awaiting decision;
4. terminal/inconclusive review failure;
5. unreviewed explicit review request;
6. authored PR concern;
7. queued/running informational work.

Cards show repository, PR number/title, author, relationship, head SHA prefix, latest sync time, run state, verdict, highest severity, policy disposition, and available primary action.

### 8.3 Attention lifecycle

`attention_items` is a rebuildable projection, never source of truth. Each open condition produces a stable occurrence fingerprint from reason type and owning entity/version. Projection rules open, update, resolve, or recur items as domain state changes.

- User acknowledgement hides an occurrence from default inbox without changing its domain owner.
- A changed occurrence fingerprint, such as another failed attempt or newer revision, reopens attention.
- Decision-required attention resolves only through reject, approve/publish, proposal withdrawal, supersession, or terminal PR state; acknowledgement alone cannot dismiss it.
- Operational failures may be acknowledged, retried, or explicitly abandoned with reason.
- Reconciliation resolves assigned-review attention when request is removed, and terminal attention when PR closes/merges.
- If user completes review directly on GitHub, reconciliation records an `external_resolution`, resolves matching attention, and withdraws any incompatible local proposal without claiming local publication.
- Acknowledgements are immutable actor events stored separately from projection. “Unacknowledge” creates a new event.

### 8.4 PR detail page

One page explains the entire lifecycle:

- GitHub header and freshness indicator;
- current revision and prior revisions;
- latest assessment, findings, limitations, and validation warnings;
- current proposal with before/after editor;
- explicit statement of GitHub effect before confirmation;
- run controls with profile, engine, model, effort, and additional context;
- chronological timeline of triggers, runs, decisions, publications, and supersession;
- links to raw diagnostics subject to retention and redaction policy.

### 8.5 Decision workflow

Before confirmation, UI must show:

- exact action: approve, comment, or request changes;
- exact head SHA, base SHA, and revision identifier;
- body content;
- every inline comment and anchor;
- comments that could not be anchored;
- actor attribution expected on GitHub;
- whether this action is automatic or human-triggered.

Confirmation closes immediately after durable decision creation. Progress appears as a persistent publication status, not a blocking modal. Duplicate clicks return the existing decision/publication resource.

### 8.6 Accessibility and responsiveness

- WCAG 2.2 AA target.
- Full keyboard navigation and visible focus.
- Semantic landmarks and heading order.
- Focus-managed dialogs only where confirmation is necessary.
- ARIA live regions for asynchronous state.
- Color never carries status alone.
- Mobile view preserves actions and evidence rather than collapsing them into hidden hover controls.
- Reduced-motion preference respected.

### 8.7 Canonical user journeys

#### Assigned review

Active GitHub review request creates relationship and observation. Matching automatic rule creates intent/run. Successful assessment becomes policy evaluation. Silent high-confidence pass may auto-publish only under explicit repository rule; every other actionable conclusion opens one proposal card. Decision publishes or rejects, and timeline retains full chain.

#### Authored PR

Authored relationship uses same revision and run pipeline. Default rule tracks without running. User starts review from inbox/PR page. Assessment produces advisory ready/attention status and findings, never self-approval/change-request publication. New revision supersedes prior ready state and opens new pending work according to rule.

#### New revision

Webhook/reconciliation creates new revision when head/base/diff identity changes. Queued old work cancels; running old work may finish historically; awaiting proposals supersede immediately. New intent may include configured prior context. UI groups revisions in one timeline instead of replacing rows.

#### Failure

Timeout, invalid output, adapter crash, lost permission, incomplete diff, and publication ambiguity produce typed operational conditions. User sees shortest actionable next step: retry new attempt, change engine/profile, inspect coverage, fix credentials, verify GitHub, or abandon. No operational failure is mislabeled as code-review verdict.

## 9. System architecture

```text
GitHub webhooks ---------+
                        |
GitHub reconciler -------+--> Event Inbox --> PR Projector --> Policy Scheduler
                                                               |
UI/API commands -----------------------------------------------+
                                                               v
                                                   Review Intent + Durable Job
                                                               |
               +----------------------+------------------------+------------------+
               |                      |                                           |
               v                      v                                           v
        Context Builder       Review Orchestrator                         Publication Worker
               |                      |                                           |
               |              Engine Adapter                              GitHub Mutation API
               |               CLI or API                                        |
               |                      |                                           |
               +------------> Validator/Normalizer --> Policy --> Proposal/Decision
                                                                                  |
Domain Events --> Outbox --> Notifications / SSE / Analytics Projections <--------+

All modules share one transactional database and typed domain contracts.
```

### 9.1 Module boundaries

#### GitHub gateway

- Authenticates requests.
- Receives and verifies webhooks.
- Fetches PR metadata, review requests, diffs, file contents, and status.
- Posts reviews and issue comments.
- Maps provider errors into stable internal classes.
- Never contains workflow policy.
- Detects missing or truncated per-file patches. It reconstructs a canonical diff through the full diff/compare API or a verified local Git worktree when possible; otherwise it marks revision coverage incomplete.

#### Event inbox and projector

- Stores each webhook delivery before processing.
- Deduplicates by GitHub delivery ID.
- Converts webhook/poll data into canonical PR and revision facts.
- Records source event and observed timestamp.
- Writes state changes, domain events, and outbox rows in the same database transaction. Dispatch begins only after commit.

#### Reconciler

- Periodically lists relevant open PRs and refreshes known active PRs.
- Uses conditional requests and batched queries where useful.
- Repairs missed review requests, pushes, closes, merges, and access changes.
- Marks freshness without deleting history after transient failures.

#### Policy scheduler

- Evaluates ordered watch rules against PR facts.
- In one transaction, creates a review intent, its first queued run attempt, the job referencing that run, domain events, and outbox rows; alternatively creates only an attention reason when no run is authorized.
- Deduplicates identical triggers.
- Cancels or supersedes obsolete queued work.

#### Context builder

- Creates a deterministic revision manifest owned by the control plane.
- Includes metadata, base/head SHAs, unified diff, selected files, profile version, prior findings, user instructions, and artifact hashes.
- Enforces size and path limits.
- Produces a content hash stored on the run.
- Supports optional context providers such as Atlas through a read-only interface.
- May materialize an immutable read-only worktree for capable CLI adapters. The manifest remains canonical, and the run records whether the engine received `diff_only`, `selected_files`, or `read_only_worktree` access.
- Emits an explicit coverage budget and omission list. Context-window truncation is data, never an unlogged implementation detail.

#### Review orchestrator

- Claims a run job using a lease and fencing token; the job references the already-created immutable run attempt.
- Selects and invokes configured engine adapter.
- Streams structured progress to run events.
- Applies timeout and cancellation.
- Stores engine identity, parameters, prompt/profile version, duration, and token/cost metadata when available.
- Does not publish GitHub effects.

#### Engine adapters

Common interface:

```text
Capabilities() -> supported parameters and output modes
ValidateConfig(config) -> errors/warnings
Review(context, revision_bundle, run_config) -> output stream + final assessment payload
Cancel(run) -> best-effort termination
Health() -> availability and version
```

Initial adapters:

- Claude CLI
- Codex CLI
- Cursor Agent CLI/custom argv
- Anthropic API
- OpenAI API

CLI adapter rules:

- execute argv directly without a shell;
- pass large prompts through stdin or an owned input file;
- use a minimal allowlisted environment;
- use an isolated temporary working directory;
- set process-group cancellation and bounded output capture;
- never expose GitHub mutation credentials;
- support adapter-specific framing and fallback JSON extraction;
- record CLI version and resolved executable path.

A temporary directory, reduced environment, or read-only worktree is not a security sandbox. Every CLI engine configuration declares one enforced isolation level:

- `container_isolated`: rootless OCI container, read-only source mount, writable scratch only, dropped capabilities, resource limits, and explicit network policy;
- `native_restricted`: reviewed OS-specific sandbox implementation with documented filesystem, process, and network guarantees;
- `trusted_host`: normal host process with reduced environment but potential access to user files, credentials, and network.

`trusted_host` requires explicit opt-in and persistent UI warning. Policy cannot auto-publish results from `trusted_host` unless the repository rule separately permits that isolation level. Direct API adapters offer the strongest default boundary because the service sends a bounded bundle without granting an autonomous local process host tool access.

Isolated configurations declare tool allowlist, read/write mounts, CPU/memory/process/time limits, network destinations, and credential delivery. Prefer a short-lived provider credential through inherited descriptor, narrow secret file, or local credential broker. Never mount the user's home directory or full credential store into an isolated engine. A CLI that cannot authenticate without broad host access is `trusted_host`, not falsely labeled isolated. Sandbox setup fails closed when an advertised control cannot be applied.

API adapter rules:

- use provider-supported structured output when available;
- retry only documented transient failures;
- record provider request identifiers;
- avoid logging prompts or source by default;
- expose cost and rate-limit signals when available.

#### Validator and normalizer

- Owns assessment schema.
- Validates and fingerprints findings.
- Maps diff anchors.
- Separates engine limitations from product validation warnings.
- Produces a canonical assessment consumed by policy and UI.

#### Policy engine

- Evaluates deterministic ordered rules.
- Stores input facts, matched rule version, and disposition.
- Creates a proposal or attention reason.
- Has no network access.

#### Decision service

- Creates proposal revisions and decisions.
- Uses optimistic concurrency and revision freshness checks.
- Records actor, timestamp, reason, and source IP/session where applicable.
- Enqueues publication in the same transaction as approval.

#### Publication service

- Sole owner of GitHub write operations.
- Revalidates PR state, head SHA, base SHA, and canonical diff identity.
- Validates inline anchors again against current diff.
- Posts one review with body and valid inline comments.
- Stores GitHub review/comment IDs and response metadata on the attempt and confirmed effect.
- Retries safe failures with backoff.
- Converts ambiguous results into `uncertain` rather than blindly reposting.

#### Notification hub

- Consumes domain events from outbox.
- Applies user preferences and deduplication windows.
- Supports browser, sound file, TTS, and log channels initially.
- Never blocks decisions or publications.

#### API and event stream

- Exposes versioned JSON API.
- Publishes live changes through Server-Sent Events.
- Generates OpenAPI schema used to generate the TypeScript client.
- Contains transport validation, not domain rules.

### 9.2 Why a modular monolith

One process and one database provide transactional decisions, jobs, and outbox events without distributed transactions. Module interfaces retain a later extraction path, but current scale does not justify network boundaries. Review engines already provide the expensive and failure-prone external boundary.

## 10. Technology decisions

### 10.1 Backend: Go

Use the latest supported Go 1.26 patch release as baseline.

Reasons:

- strong fit for a long-running I/O service with bounded concurrency;
- explicit context cancellation for HTTP and subprocess work;
- direct argv subprocess execution without shell expansion;
- simple cross-platform binaries;
- embedded static web assets;
- static types around lifecycle transitions;
- lower operational overhead than a Python virtual environment or Node runtime.

The language does not fix domain design by itself. The same architecture could be implemented well in Python. Go is chosen for distribution, process supervision, and concurrency clarity.

Alternatives considered:

- **Python:** fastest migration and existing expertise, but packaging and subprocess/concurrency discipline remain more operationally expensive.
- **TypeScript/Node:** one language across UI/backend, but less attractive for a local daemon supervising long-lived subprocesses and shipping as one native artifact.
- **Rust:** excellent safety and binary distribution, but higher implementation cost for little product benefit at this scale.

### 10.2 Backend libraries and structure

- Standard `net/http` routing unless a concrete missing need justifies a router dependency.
- `database/sql` with `modernc.org/sqlite` for CGO-free cross-platform SQLite.
- SQL migrations committed as numbered files.
- `sqlc`-generated query code where supported; hand-written repositories only for dynamic reporting queries.
- Structured logging through Go standard structured logging facilities.
- OpenTelemetry-compatible tracing boundary, disabled by default locally.
- No ORM and no dependency-injection framework.

Suggested layout:

```text
cmd/reviewd/
cmd/reviewctl/
internal/domain/
internal/application/
internal/adapters/github/
internal/adapters/engine/
internal/adapters/notification/
internal/persistence/sqlite/
internal/persistence/postgres/
internal/api/
internal/worker/
migrations/
web/
```

### 10.3 Frontend: lightweight server-rendered control desk

- Semantic HTML, CSS, and small browser JavaScript embedded in the Go binary.
- Versioned JSON API remains the boundary between dashboard and domain code.
- Browser JavaScript owns only local view state, request lifecycle, and explicit
  local mutation controls; SQLite remains source of truth.
- No frontend framework, build pipeline, global client store, or server-side
  rendering layer is required for this single-user loopback control desk.
- Component behavior must meet accessibility requirements without depending on
  framework primitives.
- Charting stays optional and isolated behind an analytics view adapter.

This choice keeps startup, deployment, and diagnosis simple. Reconsider a
framework only if multi-page navigation, independently shipped UI teams, or
substantially richer client-side interaction becomes real product pressure.

### 10.4 Storage

SQLite is default for local/single-instance deployment:

- foreign keys enabled;
- WAL mode enabled;
- bounded busy timeout;
- short write transactions;
- explicit checkpoint/backup behavior;
- database kept on local disk, never a network filesystem.

PostgreSQL becomes required when more than one service instance or concurrent team actors are supported. Domain services depend on repositories and transaction interfaces, not SQLite-specific SQL behavior.

### 10.5 Job execution

Use a database-backed queue rather than an in-memory task or external broker.

Each job has type, payload reference, state, attempt, available time, lease owner, lease expiry, lease generation, priority, and last error class. Workers claim jobs transactionally, receive a fencing token, and heartbeat before half the lease duration. Expired leases return to available state. Every state commit uses compare-and-swap on job ID, lease owner, and generation; a stale worker cannot commit after another worker reclaims the job. Handlers must still be idempotent because external APIs cannot honor the local fencing token.

Default concurrency:

- one active run per PR;
- configurable global and per-engine limits;
- CLI engine default limit of one per adapter installation;
- API adapters may run concurrently within provider limits;
- publication serialized per PR;
- GitHub reads globally rate-limited with adaptive backoff.

Reconciliation, web serving, and notification delivery use separate worker pools and never wait for review-engine capacity.

## 11. Persistence model

All primary keys use sortable opaque IDs. GitHub numeric IDs remain external identifiers, not internal keys.

| Table | Purpose and important constraints |
|---|---|
| `connections` | GitHub connection mode and non-secret metadata |
| `repositories` | GitHub repository identity, installation, active state |
| `pull_requests` | One row per repository/number; current GitHub facts and current revision ID |
| `pull_request_observations` | Immutable policy-relevant fact snapshots: title, body hash, labels, draft/base state, reviewer set, source generation |
| `pr_relationships` | Temporal relationships with active-from/active-until, source, subject, and observation generation |
| `revisions` | Immutable base/head/diff snapshot; unique repository/PR/head SHA/base SHA/diff hash |
| `review_profiles` | Stable profile identity |
| `review_profile_versions` | Immutable instructions and context settings |
| `watch_rules` | Stable rule identity and current-version pointer |
| `watch_rule_versions` | Immutable match, trigger, publication matrix, priority, and policy-set generation |
| `policy_sets` | Ordered rule-generation identity used by evaluations and reordering |
| `engine_configs` | Stable engine configuration identity and current-version pointer |
| `engine_config_versions` | Immutable adapter, model parameters, isolation policy, and secret references |
| `review_intents` | Durable request, trigger source, user context, selected profile, lifecycle; idempotency key unique |
| `review_runs` | Immutable execution attempt under an intent; attempt number unique within intent |
| `run_events` | Progress and diagnostics timeline |
| `assessments` | One normalized final assessment per successful run |
| `findings` | Normalized findings with anchors, fingerprints, and validation status |
| `policy_evaluations` | Immutable revision, observation, rule/profile/engine inputs, matched rule, and disposition |
| `proposals` | Lifecycle container tied to revision and policy evaluation |
| `proposal_revisions` | Immutable editable content versions |
| `decisions` | Immutable human/policy decisions; one effective decision per proposal revision |
| `publication_effects` | Authorized external intention with typed owner (`proposal_revision` or operational lifecycle object), revision/observation targets, payload hash, and idempotency key |
| `publication_attempts` | Immutable network attempts, response/error metadata, and GitHub identifiers; attempt number unique within effect |
| `external_artifacts` | Optional eye comment and other managed GitHub artifacts |
| `attention_items` | Materialized derived reasons for efficient inbox reads |
| `attention_acknowledgements` | Immutable acknowledgement/reopen events keyed to attention occurrence fingerprint |
| `jobs` | Durable background work and leases |
| `inbox_events` | Raw webhook/reconciliation inputs; delivery ID unique when present |
| `domain_events` | Append-only audit-friendly internal events |
| `outbox` | Transactional delivery queue for notifications and live updates |
| `notification_deliveries` | Per-channel attempts and deduplication |
| `preferences` | Local user UI and notification preferences |
| `artifacts` | Content-addressed artifact metadata: class, hash, size, storage path, encryption, expiry, deletion state |
| `migration_ledger` | Legacy source IDs, checksums, import status |

Database-enforced invariants include:

- one current revision pointer per PR;
- one active review intent per revision/profile/trigger fingerprint;
- one active run attempt per PR through a partial unique index on queued/preparing/running/validating states;
- one effective decision per proposal revision;
- one publication effect per typed owner, target revision/observation, effect type, and payload hash;
- one active publication attempt per effect;
- monotonic attempt number and lease generation;
- foreign keys preventing decisions, effects, or findings from outliving their owning revision chain.

Large raw prompts, diffs, and engine output are compressed content-addressed artifacts. Write to restrictive temporary file, hash and fsync, atomically rename inside artifact root, then commit metadata reference. Startup/GC removes unreferenced temporary/orphan files after grace period. Backup and restore always treat database plus artifact root as one consistency set.

Audit boundary is explicit:

- permanent: entity transitions, domain events, profile/rule versions, run configuration and timings, context/output manifests and hashes, normalized assessments/findings, exact proposal revisions, decisions, publication effects/attempts, external IDs, and migration ledger;
- retention-controlled: raw source, unified diff bytes, complete prompts, raw engine output, webhook bodies, and detailed operational logs.

After a raw artifact expires, audit can prove which hashed input/output produced a decision but cannot reproduce or display deleted content. UI labels that limitation.

Default retention:

- normalized assessments, decisions, and publications: indefinite;
- raw engine output and prompt bundle: 30 days;
- source snapshots/diffs: 30 days after PR closes;
- webhook bodies: 7 days after successful processing;
- operational run logs: 30 days.

Retention is configurable and cleanup emits auditable events.

## 12. Policy and configuration

### 12.1 Review profiles

A profile version contains:

- name and description;
- review instructions;
- output schema version;
- default engine and parameters;
- timeout;
- maximum diff/context size;
- file include/exclude patterns;
- prior-review context strategy;
- optional context providers;
- validation thresholds;
- publication body template.

Updating a profile creates a new version. Every run records exact version.

Prior-review context strategy is one of:

- `none`;
- `latest_successful_assessment` from immediately preceding revision;
- `latest_human_edited_proposal` from immediately preceding revision;
- `assessment_and_human_edits` (default);
- `explicit_run`, requiring a run ID in manual command.

Only data from the same PR may be selected. Context builder labels prior content as historical and untrusted, records source IDs/hashes, and never treats an old finding as verified against current revision. If preceding revision is unavailable or artifacts expired, run continues with a recorded limitation unless profile requires continuity.

### 12.2 Watch rules

Rules are ordered and first-match-wins in version 2. Additive rules are deliberately unsupported because merge precedence makes safety review harder. Match fields include repository, owner, author, relationship, draft state, labels, target branch, and file patterns.

Actions include:

- ignore;
- track only;
- review automatically;
- require manual start;
- select profile;
- override engine parameters;
- set rule-level external-action policy;
- set re-review behavior on new commit.

Rule-level external-action policy is distinct from the installation runtime gate:

- `advisory_only`: assessments and proposals remain local; policy cannot authorize an effect;
- `require_confirmation`: policy creates a proposal awaiting human decision;
- `auto_publish`: policy may authorize an effect only after built-in safety gates; repository opt-in is required for approvals;
- `human_attention`: create attention without a publishable proposal.

The global runtime publication mode is independently one of `disabled`, `simulated`, or `enabled`. It is a final deployment safety gate, not a rule outcome. `disabled` creates no publication attempts; `simulated` records the effect and a simulated result without network mutation; `enabled` permits dispatch. A rule cannot override a stricter global mode.

Example:

```yaml
- name: own-prs-manual
  when:
    relationship: authored_by_me
  review:
    trigger: manual
    profile: world-class
  publication:
    policy: advisory_only

- name: assigned-default
  when:
    relationship: review_requested
  review:
    trigger: automatic
    profile: adaptive
  publication:
    pass: require_confirmation
    concerns: require_confirmation
    changes_required: require_confirmation
    inconclusive: human_attention
```

Every change creates an immutable `watch_rule_version`; stable `watch_rules` only hold identity, enabled state, and current version pointer. Reordering creates a new policy-set generation so a historical evaluation can reconstruct exact order.

### 12.3 Policy evaluation contract

Evaluation order is normative:

1. Reject terminal, stale, unauthorized, or malformed input.
2. Match enabled watch-rule versions by ascending priority then stable ID; select first match.
3. Resolve profile and engine defaults, then validate allowed one-shot overrides.
4. Schedule or suppress review intent according to rule trigger.
5. After successful assessment, apply built-in safety gates.
6. Map verdict through selected rule's publication matrix.
7. Render proposal body deterministically from profile template, assessment summary, and ordered findings.
8. Store evaluation input snapshot, policy-set generation, matched rule version, safety overrides, rendered output hash, and final disposition.

Built-in safety gates override repository rules:

- own PR cannot produce GitHub approval or change-request publication;
- partial/unknown coverage cannot auto-approve;
- automatic approval requires `pass`, high confidence, complete coverage, no findings above `note`, empty review body, and no inline comments;
- stale revision cannot create or approve proposal;
- terminal/draft PR policy is enforced before publication;
- failed run has no assessment and therefore no assessment-based disposition;
- unsupported engine parameters fail before run creation.

Default matrix when selected rule omits a verdict:

| Verdict | Default disposition |
|---|---|
| `pass` | `propose_approval` |
| `concerns` | `propose_comment` |
| `changes_required` | `propose_changes` |
| `inconclusive` | `require_human_review` |

Policy validation rejects unknown fields, duplicate priorities within one policy generation, impossible auto-publication for authored PRs, missing profiles/engines, and automation that violates a built-in safety gate.

### 12.4 Configuration precedence

Use fewer configuration channels:

1. explicit CLI bootstrap flags;
2. environment variables for deployment and secret references;
3. one YAML bootstrap file;
4. database-managed product settings.

Bootstrap configuration covers bind address, database, logging, and secret references. Product settings such as profiles, rules, engine parameters, and notifications live in the database and are edited through UI/API. This removes the need to restart for routine behavior changes.

Secrets never appear in product settings responses. Local mode stores secrets in OS credential storage where available, with environment/file references as fallback. Server mode uses mounted secrets or a secret manager.

### 12.5 Publication and dry-run modes

- `disabled`: discovery, reviews, proposals, decisions, and authorized effect records persist normally, but no publication attempt or dispatch job is created.
- `simulated`: effects and simulated attempts persist with exact rendered payload and safety checks, but GitHub mutation adapter is never called.
- `enabled`: authorized effects may call GitHub.

Changing mode is audited and does not retroactively release old effects. This deliberately differs from the legacy promise that dry-run leaves no database state: durable simulation is necessary to inspect and compare behavior. For a no-state diagnostic, `reviewctl review --ephemeral <PR URL>` runs one review in an isolated temporary context, prints normalized output, and performs no product database, notification, or GitHub mutation.

## 13. GitHub integration

### 13.1 Authentication modes

#### Local user mode

Use a fine-grained personal token, GitHub CLI credential helper, or GitHub App user token. Actions are attributable to user where GitHub supports it. Bind UI to loopback by default.

#### Server mode

Use a GitHub App installation. Installation tokens are short-lived and cached until near expiry. Request minimum repository permissions. GitHub App webhooks are primary ingestion.

Version 2 still exposes its control UI/API only on loopback. Server mode may expose a separate webhook-only listener behind HTTPS/reverse proxy; that listener serves no control endpoints and authenticates only signed GitHub deliveries. Shared team control-plane access is deferred until an actor identity and authorization design is approved.

### 13.2 Event ingestion

Relevant events include pull-request open/reopen/update/synchronize/close, review-request changes, installation/repository access changes, and review events needed to reconcile publications.

Webhook handler:

1. enforce request size limit;
2. verify signature using constant-time comparison;
3. store delivery ID, event type, action, headers subset, and body hash/body;
4. return success after durable acceptance;
5. process asynchronously.

### 13.3 Reconciliation

Webhook mode still runs reconciliation at a slower interval. Poll-only local mode runs it at configured interval with jitter. A successful snapshot updates observed facts atomically. A failed scan records staleness and preserves last known facts.

Discovery is defined per connection mode:

- **Local user mode:** paginate account-wide searches for open PRs with `review-requested:<configured-login>` and `author:<configured-login>`. Explicitly watched repositories additionally enumerate open PRs. If search reaches provider result caps, partition by updated-time windows and report any unverifiable gap.
- **GitHub App mode:** enumerate active installations and repositories by immutable GitHub repository ID, backfill open PRs per repository, then use webhooks for low-latency change. Periodic per-installation reconciliation remains mandatory.
- Follow every list generation with conditional detail refresh for changed/unknown items. Prefer batched GraphQL reads where their permission/error behavior is covered by contract tests; use REST for provider operations requiring it.
- Track repository rename or transfer by immutable GitHub ID and update display coordinates. Lost installation/repository access ends active relationships but preserves history.
- Store scan generation, query partition, pages expected/received, provider total, and completion state. Only a complete generation may end relationships absent from results. Partial, capped, rate-limited, or failed generations never remove prior attention state.
- Search indexing delay is expected. Webhook observations and direct PR refresh take precedence over an older search snapshot.

Each material change to title/body hash, labels, draft state, base branch, reviewer set, relationship set, or PR state creates a new observation. A run, policy evaluation, proposal, decision, and publication effect store the exact observation ID used. Draft-to-ready, reviewer-set changes, relationship changes, and policy-relevant label/base changes re-evaluate scheduling and policy even when revision identity is unchanged. If re-evaluation changes eligibility, disposition, profile, engine configuration, or rendered content, pending proposals/effects are superseded. Review-request removal cancels queued automatic intents; a running intent may finish for history but cannot auto-publish. Head/base/diff changes create a new revision and supersede unpublished work.

For assigned reviews, `review_requested` is a current relationship, not a permanent label. A new commit schedules another automatic intent only when the configured reviewer is still requested. A user may still start an explicit on-demand intent from retained PR history.

Refresh the selected PR immediately before:

- starting an on-demand run;
- approving a proposal;
- publishing a review;
- expiring an active proposal due to suspected close/merge.

### 13.4 Publication safety

Before GitHub mutation:

- PR remains open and not merged;
- current revision ID, head SHA, base SHA, and diff hash equal effect targets;
- current policy-relevant observation ID equals the effect target, or a fresh deterministic policy evaluation has produced an equivalent newly authorized effect;
- actor still has permission;
- inline anchors remain valid;
- no existing publication with same idempotency key is confirmed;
- publication mode is enabled, not simulated or disabled.

GitHub may accept a request whose response is lost. For ambiguous network failures, query recent reviews/comments for the stored correlation marker or expected actor/content to reconcile the effect before any human-authorized retry. Do not blindly repost.

### 13.5 Delivery semantics

The database guarantees one local publication effect, not exactly-once GitHub delivery. GitHub review creation does not accept the product's idempotency key.

- A definite success records external IDs and completes the effect.
- A definite failure before any request bytes are sent may retry automatically.
- A complete GitHub response that unambiguously rejects the mutation may follow its operation-specific error policy. Explicit rate-limit and precondition responses are retryable only when their semantics prove no mutation occurred.
- Once any mutation request bytes may have been transmitted, timeout, connection reset, truncated response, proxy failure, and provider 5xx are `uncertain`, even if the generic HTTP layer labels them transient. They never enter automatic retry.
- A lost/ambiguous response creates an `uncertain` attempt and effect.
- Verification searches the exact PR revision for actor, review state, creation window, body fingerprint, and inline-comment fingerprint. Commented reviews may include a hidden correlation marker when policy allows; silent approvals cannot.
- One unique match confirms publication. Multiple matches or no reliable match leave effect `uncertain`.
- An uncertain effect never retries automatically. User may mark the effect externally completed, abandon it, or explicitly authorize another attempt after inspecting GitHub.

This favors a visible missed automation over a duplicate GitHub review.

### 13.6 Review-start marker lifecycle

Optional eye marker is a managed `external_artifact`, not incidental monitor logic. Its create/delete operations are publication effects owned by the marker lifecycle object rather than a proposal. States are `planned`, `creating`, `active`, `deleting`, `deleted`, `uncertain`, and `orphaned`.

- Marker belongs to PR, revision, and intent; stored external comment ID is mandatory after confirmed create.
- Before a replacement, publisher deletes confirmed prior marker. If deletion fails or is ambiguous, it does not create another marker and opens operational attention.
- Default compatibility lifetime is `until_next_run`; profile may choose `until_run_terminal`.
- PR close/merge schedules best-effort cleanup without delaying lifecycle projection.
- Create/delete attempts use same uncertain-delivery rules as reviews.
- Reconciliation may adopt a uniquely identifiable existing marker or label unknown duplicates `orphaned`; it never deletes arbitrary user comments.

## 14. API design

All endpoints live under `/api/v1`. JSON errors use stable code, human message, retryability, and correlation ID.

### 14.1 Read endpoints

```text
GET /inbox
GET /pull-requests
GET /pull-requests/{id}
GET /pull-requests/{id}/timeline
GET /pull-requests/{id}/revisions
GET /review-intents
GET /review-intents/{id}
GET /runs
GET /runs/{id}
GET /runs/{id}/events
GET /proposals/{id}
GET /publication-effects/{id}
GET /publication-effects/{id}/attempts
GET /history
GET /analytics/overview
GET /analytics/trends
GET /analytics/repositories
GET /analytics/authors
GET /profiles
GET /rules
GET /engines
GET /settings
GET /health/live
GET /health/ready
GET /events/stream
```

### 14.2 Command endpoints

```text
POST /pull-requests/{id}/runs
POST /review-intents/{id}/runs
POST /runs/{id}/cancel
POST /proposals/{id}/revisions
POST /proposals/{id}/decisions
POST /publication-effects/{id}/attempts
POST /publication-effects/{id}/resolutions
POST /pull-requests/{id}/reconcile
POST /attention/{id}/acknowledgements
DELETE /attention/{id}/acknowledgements/current
POST /connections
POST /connections/{id}/check
POST /repositories
DELETE /repositories/{id}
POST /profiles
POST /profiles/{id}/versions
POST /rules
POST /rules/{id}/versions
POST /policy-sets/{id}/reorder
DELETE /rules/{id}
POST /engines
POST /engines/{id}/versions
POST /engines/{id}/check
POST /notifications/preview
PUT  /settings/notifications
PUT  /settings/publication-mode
```

`POST /pull-requests/{id}/runs` creates an intent, first queued attempt, and job in one transaction. `POST /review-intents/{id}/runs` retries an existing eligible intent by creating another queued attempt and job in one transaction. The orchestrator only claims that existing run. Commands creating background work return `202 Accepted` with intent, run, and job IDs. Decision commands require `proposal_revision_id`, `expected_revision_id`, `expected_observation_id`, `expected_head_sha`, and `expected_base_sha`. Stale requests return `409 revision_or_observation_changed`, never implicit approval of newer work.

Use cursor pagination for histories and timelines. Use ETags or version fields for mutable settings and proposal drafting.

### 14.3 Normative transport conventions

- Mutating requests require `Idempotency-Key`; server stores key, actor, route, request hash, status, and response resource for at least 24 hours. Reusing a key with different body returns `409 idempotency_conflict`.
- Mutable settings require `If-Match` with resource version. Mismatch returns `412 version_conflict`.
- List response shape is `{"items": [...], "next_cursor": "...", "snapshot_at": "..."}`. Cursor is opaque and stable only for same filter/sort.
- Inbox filters: `reason`, `repository_id`, `relationship`, `state`, `acknowledged`, `updated_before`; sort defaults to priority then occurrence time.
- Timestamps are UTC RFC 3339 with fractional seconds. IDs are opaque strings. SHA fields are lowercase full hex values.
- Error shape is `{"error":{"code":"revision_changed","message":"...","retryable":false,"correlation_id":"...","details":{}}}`. Internal exception text is never returned.
- SSE event ID is persisted domain-event sequence. Client sends `Last-Event-ID`; server replays retained events then follows live outbox delivery. If requested event expired, server sends `resync_required` and closes. UI then refetches active queries.
- Normative OpenAPI plus generated Go/TypeScript contract tests must be committed before endpoint implementation.

Run command example:

```json
{
  "expected_revision_id": "rev_...",
  "profile_version_id": "rpv_...",
  "engine_config_version_id": "ecv_...",
  "user_context": "Focus on migration rollback safety",
  "overrides": {"model": "provider-model-alias", "effort": "high"}
}
```

Decision command example:

```json
{
  "proposal_revision_id": "prv_...",
  "expected_revision_id": "rev_...",
  "expected_observation_id": "obs_...",
  "expected_head_sha": "full-head-sha",
  "expected_base_sha": "full-base-sha",
  "decision": "approve | reject",
  "reason": "Optional audit reason"
}
```

Accepted asynchronous command response:

```json
{
  "status": "accepted",
  "intent_id": "rvi_...",
  "run_id": "run_...",
  "job_id": "job_...",
  "resource_url": "/api/v1/runs/run_..."
}
```

Connection/engine secret fields accept secret references only, never literal secret values returned by API. Backup, restore, legacy import, and full support-bundle export remain authenticated `reviewctl` commands because they operate on service lifecycle and local files.

## 15. Reliability and failure handling

### 15.1 Idempotency keys

- revision: repository ID + PR number + head SHA + base SHA + canonical diff hash;
- review intent: revision ID + trigger fingerprint;
- run attempt: intent ID + attempt number;
- decision: proposal revision + actor + client command ID;
- publication effect: owner kind + owner ID + target revision + target observation + effect type + payload hash;
- publication attempt: effect ID + attempt number;
- webhook: GitHub delivery ID;
- notification: domain event + channel + template version.

The trigger fingerprint is a versioned hash of trigger kind, logical occurrence identity, target observation ID, profile version, engine-config version, normalized one-shot overrides, and normalized user-context hash. Logical occurrence identity is stable across reconciliation of the same condition: for example relationship activation ID for an automatic assigned review, webhook delivery/domain-event ID for a material transition, or client command ID for a manual request. Repeating a manual review intentionally requires a new client command ID; repeated polls do not create new intent identity.

### 15.2 Retry classes

- **Read-only/idempotent transient:** timeout, connection reset, provider 5xx, and explicit rate limit may retry with exponential backoff, jitter, and cap.
- **GitHub mutation before-send failure:** retry only when the transport proves no request bytes were sent.
- **GitHub mutation post-send ambiguity:** timeout, reset, truncated response, proxy error, or provider 5xx after possible transmission becomes `uncertain`; never automatically retry.
- **GitHub mutation definite rejection:** follow operation-specific handling only when a complete response proves the mutation was not applied.
- **Authentication/authorization:** stop retrying; create attention item.
- **Invalid input/output:** terminal for attempt; optional user-triggered rerun.
- **Stale revision:** supersede, never retry.
- **Ambiguous publication:** verify externally before retry.
- **Resource exhaustion:** delay and surface capacity state.

### 15.3 Crash recovery

- Expired job leases become claimable.
- Running subprocesses die with service process; run attempt becomes retryable or failed during recovery.
- `dispatching` effects without confirmed response become `uncertain` and enter verification, not automatic repost.
- Outbox rows remain until acknowledged by each consumer.
- SQLite backup includes database, WAL, and migration version through supported backup procedure.

### 15.4 Service targets

- No automatic duplicate GitHub publication across locally observed retries or crashes; ambiguous external outcomes stop in `uncertain` until uniquely reconciled or resolved by a human.
- Webhook durable acknowledgement under one second under normal local load.
- Local read API p95 under 250 ms with 10,000 historical reviews.
- Webhook-triggered queue visibility under five seconds.
- Poll-only discovery within configured interval plus jitter.
- UI remains usable while engines run.
- One failed PR, engine, or notification channel cannot stop reconciliation.

## 16. Security model

Pull-request content is untrusted. It may include prompt injection, malicious filenames, huge diffs, terminal control sequences, or instructions designed to exfiltrate credentials.

Required controls:

- Engines receive no GitHub write token.
- Do not execute PR code by default.
- Materialized source is read-only to review process where platform permits, but read-only access alone is not isolation.
- CLI environment uses an allowlist, not inherited full environment.
- CLI isolation level is enforced and surfaced in every run. Network policy is deny-by-default for isolated engines, with explicit provider endpoints or proxy access when the CLI requires network authentication.
- Host-executed CLIs are treated as trusted software with potential host access; setup explains this risk and automation policy can forbid them.
- Temporary paths use secure random directories, validated joins, restrictive permissions, and cleanup.
- Diff/file size, count, and decompression limits prevent resource abuse.
- Logs strip terminal control characters and redact known secret patterns.
- Raw prompts/output are excluded from normal logs.
- Webhook signatures and request limits are mandatory.
- GitHub permissions follow least privilege.
- Browser-rendered model/GitHub content is escaped; rendered Markdown is sanitized.
- First launch creates a per-install control secret. Browser setup exchanges a one-time launch token for a short-lived, `HttpOnly`, `SameSite=Strict` session cookie. Later loopback sessions still require authentication; the CLI can open or print a new one-time URL.
- All browser mutations require origin checks and anti-CSRF tokens.
- Control API refuses non-loopback binding in version 2. Public ingress, when configured, is a separate webhook-only listener with explicit proxy trust and request limits.
- Destructive settings and publication-mode changes are audited.
- Export and backup exclude secrets.

Threat-model tests must cover prompt injection, shell argument injection, path traversal, stored XSS, CSRF, webhook spoofing/replay, token leakage, stale-decision races, and duplicate publication.

## 17. Notifications

Notification types derive from domain events:

- review observed (`review_observed`, preserving the discovery notification/sound);
- review started;
- decision required;
- human review required;
- own PR ready;
- own PR needs attention;
- review timeout/failure;
- publication failure;
- PR merged/closed with pending work;
- proposal superseded.

Preferences support enabled channels, quiet hours, per-event templates, speech rate, custom files, and runtime mute. Placeholder rendering receives typed safe fields such as repository, PR number, author, and title. Unknown placeholders remain visible in preview but fail validation when saving.

`review_observed` has the same channel/preference contract as other events and defaults to the legacy new-review sound when migrated. Contract tests verify one delivery per observation occurrence and no replay on unchanged reconciliation scans.

Sound/TTS playback is best-effort background work. UI/API command completion never waits for audio.

## 18. Analytics and audit

Analytics derive from normalized runs, assessments, findings, proposals, decisions, and publications. Do not infer behavior from queue-table status.

Initial metrics:

- observed and reviewed PR count;
- queue and review latency;
- run success, timeout, cancellation, and invalid-output rate;
- verdict and severity distribution;
- proposal acceptance, rejection, and edit rate;
- automatic publication rate;
- stale/superseded work rate;
- publication failure/retry rate;
- volume by repository and author;
- engine duration, token use, and cost when available;
- finding recurrence across revisions by fingerprint.

Every state-changing command records actor, source, correlation ID, prior version, new version, and reason. Imported legacy records are labeled `actor=legacy_import` and never presented as newly produced assessments.

## 19. Observability and operations

- Structured logs with correlation, PR, revision, run, proposal, and job IDs.
- Redaction at logging boundary.
- Health report includes database, migration, worker, GitHub auth, engine availability, webhook freshness, and reconciliation freshness.
- Metrics endpoint optional and loopback-only by default.
- Run diagnostics page shows stage durations and shortest actionable error, with expandable redacted details.
- Startup validates database access, migrations, selected engines, bind security, and notification configuration.
- Graceful shutdown stops claims, cancels or drains work within deadline, checkpoints state, and closes listeners.
- `reviewctl doctor` checks GitHub credentials, engine binaries/API credentials, database integrity, writable temp space, webhook configuration, and audio tools.

## 20. Testing strategy

### 20.1 Domain tests

- Table-driven state transitions for runs, proposals, decisions, and publications.
- Property tests for idempotency and stale-revision rejection.
- Policy rule fixtures with exact matched-rule explanations.
- Finding fingerprint, canonical diff, coverage downgrade, and diff-anchor validation fixtures.

### 20.2 Adapter contracts

Every GitHub adapter passes the same fake-server suite: pagination, conditional requests, rate limits, permission failures, diff truncation, posting, ambiguous response, and status refresh.

Every engine adapter passes the same suite: capabilities, config validation, success, malformed output, oversized output, timeout, cancellation, progress, and version capture.

### 20.3 Persistence tests

- Migration from every released schema version.
- Transaction rollback and outbox atomicity.
- Concurrent decision and publication claims.
- Lease heartbeat, expiry, fencing-token races, and recovery.
- SQLite busy handling and backup/restore.
- PostgreSQL suite activated before team mode.

### 20.4 Integration and end-to-end tests

- Recorded GitHub webhook payload fixtures.
- Fake GitHub and fake engine full workflow.
- Browser flows for inbox, review start, proposal edit, approval, rejection, stale revision, failure retry, mute, and settings.
- Keyboard, focus, accessible-name, contrast, and mobile viewport checks.
- Service-kill tests during run, decision, publication, and outbox delivery.
- Shadow-mode comparison against current application for selected PR fixtures.

### 20.5 Security tests

- Malicious PR title/body/diff payload corpus.
- Shell metacharacter argv tests.
- Environment and token non-disclosure checks.
- Stored XSS and Markdown sanitation.
- CSRF and origin enforcement.
- Webhook signature/replay.
- Stale SHA and double-click races.

## 21. Migration from current application

Migration should preserve history while avoiding false equivalence between old action records and new normalized assessments.

### 21.1 Legacy mapping

| Legacy data | Version 2 mapping |
|---|---|
| `pr_reviews` | PR, revision, imported run/assessment summary, and timeline event |
| `pending_approvals` pending | Proposal plus latest proposal revision; decision absent |
| `pending_approvals` approved/rejected | Proposal, proposal revision, imported decision, publication metadata when knowable |
| `pending_approvals` expired | Superseded proposal |
| `pending_approvals` merged/closed | Proposal superseded by PR terminal state |
| `own_prs` | PR relationship `authored_by_me`, revision, imported advisory run state |
| `review_requests` | PR relationship `review_requested` and last-observed metadata |
| `review_started_comments` | Managed external artifact |
| YAML/env prompt configuration | Initial profile and bootstrap settings |
| Repository/author filters | Ordered imported watch rule |
| Sound configuration | Notification preferences |

Legacy action mapping:

| Legacy action | Imported assessment | Imported disposition |
|---|---|---|
| `approve_without_comment` | `pass` | confirmed publication when evidence exists; otherwise historical `no_external_action` marker |
| `approve_with_comment` | `pass` | proposal/comment or confirmed publication according to legacy status |
| `request_changes` | `changes_required` | proposal/change request or confirmed publication according to legacy status |
| `requires_human_review` with genuine uncertainty | `inconclusive` | `require_human_review` |
| `requires_human_review` caused by operational failure | no assessment; failed run | operational attention |

Importer rules:

- Process one repository/PR group per transaction and record source-row checksum before commit.
- Rerun is idempotent through unique source table/source ID ledger entries; interrupted imports resume at next uncommitted group.
- Merge rows sharing repository, PR, head SHA, and base SHA into one revision, but preserve every source row as a distinct imported event/run/proposal/decision as appropriate.
- Empty/null SHA creates a non-publishable synthetic legacy revision `legacy:<table>:<id>` and warning.
- Parse legacy naive timestamps as UTC, preserve original text, and flag invalid values instead of guessing.
- Malformed inline-comment JSON is stored as retained raw import artifact with warning; importer never drops the source row.
- Conflict priority is external evidence, then approved/rejected decision, then pending proposal, then historical assessment. Priority selects current projection only; it never deletes lower-priority history.
- Dry-run emits counts by entity, warnings, source and planned-target checksums, conflicts, and sample mappings without writes.
- Completion verifies source-row ledger coverage, entity counts, per-PR checksums, malformed-data report, and sampled rendered content before cutover.

Preserve original row ID and table in `migration_ledger`. Preserve edited and original content as separate proposal revisions where available. Unknown GitHub publication IDs remain unknown; migration must not attempt to rediscover or repost historical actions.

Legacy `requires_human_review` rows whose reason identifies timeout, output parsing, CLI failure, or unexpected runtime error import as failed run attempts plus operational attention items. They must not become `inconclusive` code assessments. Only genuine reviewer uncertainty imports as an assessment verdict.

### 21.2 Delivery phases

#### Phase 0: behavior contract

- Freeze a feature inventory and representative database fixture.
- Export current configuration with secrets removed.
- Capture golden workflows and GitHub/engine fake fixtures.
- Add backup and migration validation commands.
- Ship a final legacy release with a shared writer-ownership guard. It must refuse GitHub mutations unless it holds the current ownership generation.

The shared guard is a small ownership SQLite database plus an OS advisory lock file in a configured local state directory accessible to both binaries. Acquisition requires holding the exclusive lock, then `BEGIN IMMEDIATE` compare-and-swap of the singleton row from the expected generation to `generation + 1`, recording owner, process instance, acquisition time, and checkpoint. The active writer keeps the lock file descriptor open for its full mutation lifetime, renews a heartbeat by matching owner and generation, and rechecks that tuple immediately before obtaining mutation credentials and before every GitHub write. Crash releases the OS lock; the successor must acquire it and increment generation before writing. A stale process whose tuple no longer matches fails closed and discards cached credentials. Cutover refuses network filesystems or any platform where lock semantics cannot be verified by an integration probe.

#### Phase 1: foundation

- Create Go service, schema migrations, repositories, jobs, outbox, health, and CLI.
- Build GitHub connection and canonical PR/revision projection.
- Run reconciler in read-only shadow mode.

#### Phase 2: review execution

- Build context bundle and CLI adapters.
- Implement assessment schema, validation, runs, cancellation, timeout, and diagnostics.
- Import prompt packs as profile versions.
- Compare new assessments with existing runs without GitHub mutation.

#### Phase 3: policy and publication

- Implement watch rules, policy evaluation, proposals, proposal editing, decisions, and publisher.
- Run publisher in simulated mode and compare intended effects.
- Prove duplicate and stale-revision protections with crash tests.

#### Phase 4: user experience

- Deliver inbox, PR detail/timeline, runs, history, analytics, settings, and notification controls.
- Complete accessibility and responsive tests.
- Make new UI read current and imported data.

#### Phase 5: controlled cutover

- Back up legacy database.
- Stop old GitHub writer.
- Perform final import and reconciliation.
- Enable new publisher for a small repository allowlist.
- Expand after successful observation window.
- Keep kill switch that sets publication mode to `disabled` without stopping reviews.

#### Phase 6: retirement

- Retain legacy database read-only for defined period.
- Remove compatibility aliases after export verification.
- Document rollback and final archive.

### 21.3 Rollback

Cutover uses the shared Phase 0 writer-ownership database and continuously held advisory lock. Both legacy and version 2 execute the same guard library/protocol; startup enters read-only mode if the guard cannot be acquired or validated. Only the lock holder whose owner and generation match the row may load GitHub mutation credentials.

Rollback procedure:

1. Set version 2 publication mode to `disabled` and drain/resolve all `dispatching` or `uncertain` effects.
2. Stop version 2 writer and freeze a rollback checkpoint.
3. Reverse-export every version 2 reviewed revision, decision, and confirmed/uncertain effect after original cutover checkpoint.
4. Import suppression records into legacy database for every affected repository/PR/head SHA. Confirmed publications import as completed reviews; uncertain effects import as human-attention blocks and must never auto-retry.
5. Reconcile GitHub and compare export/import counts and checksums.
6. Atomically transfer writer ownership to legacy generation, then start legacy writer.

If schema/features exist that reverse importer cannot represent, rollback is **restore-forward only**: keep version 2 data, deploy previous version 2 binary, and do not restart legacy writer. Restarting legacy directly from pre-cutover snapshot is forbidden because it can duplicate GitHub actions.

## 22. Implementation acceptance criteria

Version 2 is ready for primary use when:

- all preserved capabilities in section 6 have an implemented mapping or explicit product removal decision;
- one PR/revision lifecycle powers every inbox facet and history view;
- two concurrent identical commands cannot create duplicate run or publication effects;
- stale proposal approval always fails safely;
- restart during each workflow stage recovers deterministically;
- assigned and authored PRs use the same run/assessment model;
- Claude CLI, Codex CLI, Cursor Agent CLI, and at least one direct API adapter pass engine contract tests, including Cursor stream-envelope behavior;
- webhook and poll-only modes both converge to the same PR state;
- imported history counts and sampled content match legacy database;
- non-loopback startup fails without configured security;
- accessibility and security test suites pass;
- operational documentation covers install, backup, restore, upgrade, diagnosis, and rollback.
- incomplete review coverage blocks automatic approval and is visible in UI/API;
- publication ambiguity never causes automatic retry and has a tested manual-resolution flow;
- writer-ownership transfer and rollback suppression prevent legacy/new dual publication.

## 23. Open product decisions

These decisions do not block the architecture but must be resolved before implementation reaches publication:

1. Should local GitHub actions be attributed to the user or a dedicated GitHub App bot by default?
2. Which direct API provider ships first?
3. Should automatic silent approval be available globally or require per-repository opt-in?
4. Should raw source/prompt artifacts default to 7, 30, or 0 days retention?
5. Should profile/rule editing ship in first UI release or remain YAML/CLI until lifecycle is stable?
6. Is browser notification support sufficient, or is a native tray companion required later?

Recommended defaults: user attribution in local mode, per-repository auto-approval opt-in, 30-day diagnostic retention, UI read-only plus YAML import for first alpha, and no native tray application.

## 24. Rejected designs

### Keep current tables and redesign only UI

Rejected because overlapping storage lifecycles would remain. UI would continue reconstructing one PR story from unrelated rows.

### Let the model directly choose and execute GitHub action

Rejected because model judgment and authorization are different responsibilities. It also exposes mutation credentials to prompt-injectable execution.

### Webhook-only ingestion

Rejected because missed, delayed, disabled, or misconfigured deliveries need repair.

### Polling-only ingestion

Supported as local fallback, but rejected as preferred server mode due latency, rate use, and N+1 detail calls.

### Separate services for monitor, reviewer, web UI, and notifications

Rejected for current scale. It adds distributed coordination without independent scaling value.

### External workflow engine and message broker

Rejected for local-first deployment. Database jobs, leases, and outbox provide required durability.

### Electron desktop application

Rejected initially. Embedded browser UI provides cross-platform control without a second runtime and packaging surface.

## 25. Source basis and external references

Current-product analysis used repository implementation, tests, README files, and Atlas knowledge atoms including architecture [K-000001], CLI parsing [K-000002] [K-000008], SHA deduplication [K-000003], inline validation [K-000004], human approval workflow [K-000005], single-flight execution [K-000018], re-review context [K-000024], cached review requests [K-000025], pagination [K-000026], terminal PR cleanup [K-000029], and tool configuration [K-000030].

External decisions align with:

- [GitHub App authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/about-authentication-with-a-github-app)
- [GitHub App best practices](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/about-creating-github-apps/best-practices-for-creating-a-github-app)
- [GitHub webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)
- [GitHub webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)
- [Go release policy and history](https://go.dev/doc/devel/release)
- [Go embedded files](https://pkg.go.dev/embed)
- [Go subprocess execution and cancellation](https://pkg.go.dev/os/exec)
- [SQLite write-ahead logging](https://sqlite.org/wal.html)
- [CGO-free Go SQLite driver](https://pkg.go.dev/modernc.org/sqlite)
- [Vite documentation](https://vite.dev/guide/)
- [TanStack Query documentation](https://tanstack.com/query/latest/docs/framework/react/quick-start)
