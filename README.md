# Code Reviewer — v2 control plane

`reviewd` is a local Go control plane for evidence-bound GitHub pull-request review. It keeps an append-only SQLite record of observations, canonical diffs, review runs, assessments, policy evaluations, proposals, decisions, and simulated publication attempts.

This is a ground-up replacement. Legacy Python runtime, legacy dashboard, and `data/reviews.db` are not part of v2 operation.

## Safety model

- Use a separate v2 database: default `data/control-plane.db`.
- `data/reviews.db` is legacy input only. Never point `reviewd` or v2 migrations at it.
- GitHub traffic in this release is GET-only: discovery, PR detail, diff, file list, and tree reads.
- Review engines receive a bounded JSON bundle through stdin, run in a fresh directory, and receive no GitHub credentials or inherited environment.
- All review, policy, proposal, and publication records are immutable and tied to currently verified canonical evidence.
- Real GitHub publication is unavailable. `disabled` is default; `simulated` records a local simulated attempt only. Neither mode can approve, comment on, or request changes on GitHub.

## Prerequisites

- Go version declared in [`go.mod`](go.mod)
- A GitHub token only for optional GET-only reconciliation/hydration/review evidence reads
- A trusted local review-engine executable only when enabling review execution

Run tests:

```bash
go test ./...
```

## Set up the control-plane database

Create or advance only the v2 database. `--apply` is required for schema writes.

```bash
go run ./cmd/reviewctl db migrate \
  --database data/control-plane.db \
  --apply

go run ./cmd/reviewctl db status \
  --database data/control-plane.db
```

`reviewd` refuses to start with pending migrations. Let it apply known migrations during local bootstrap:

```bash
REVIEWD_DATABASE_PATH=data/control-plane.db \
REVIEWD_MIGRATION_MODE=apply \
go run ./cmd/reviewd
```

It listens only on loopback, default `127.0.0.1:8080`. Stop with `Ctrl-C`.

## Import legacy history (optional)

The v2 importer retains legacy rows as historical snapshots only. Imported revisions are permanently non-publishable and import creates no review jobs, events, outbox rows, or GitHub activity.

First make a manifest-verified backup while the legacy app is stopped:

```bash
go run ./cmd/reviewctl db backup \
  --source data/reviews.db \
  --destination data/backups/reviews-v2-import.db

go run ./cmd/reviewctl db verify-backup \
  --backup data/backups/reviews-v2-import.db
```

Plan is read-only. Apply only after checking its JSON result:

```bash
go run ./cmd/reviewctl legacy import \
  --source data/backups/reviews-v2-import.db \
  --source-id legacy-python-reviews-v1 \
  --database data/control-plane.db

go run ./cmd/reviewctl legacy import \
  --source data/backups/reviews-v2-import.db \
  --source-id legacy-python-reviews-v1 \
  --database data/control-plane.db \
  --apply
```

The same source ID is idempotent only for unchanged source-row checksums. Any changed source row fails closed.

## Observe GitHub safely

Shadow reconciliation records factual PR metadata. It requires a current v2 database and explicit `--shadow`; it does not queue reviews or publish anything.

```bash
GITHUB_TOKEN=... go run ./cmd/reviewctl github reconcile \
  --database data/control-plane.db \
  --connection-id github-local \
  --shadow \
  --token-env GITHUB_TOKEN
```

Tokens are read at execution time. Prefer `--token-env NAME`; `--token-file PATH` is also supported. Literal token flags are intentionally absent. Do not put token values in policy, profile, proposal, or command arguments.

Search results are candidates only. Reconciliation fetches authoritative PR detail and stores exact head/base facts. Incomplete, stale, capped, or rate-limited scans may add positive facts but cannot remove a relationship or advance a complete checkpoint.

### Attach canonical diff evidence

Hydration creates a canonical revision only after reading an exact text diff, verified file coverage, and base/head trees. Run it after an observation exists:

```bash
GITHUB_TOKEN=... go run ./cmd/reviewctl github hydrate \
  --database data/control-plane.db \
  --connection-id github-local \
  --owner OWNER \
  --repository REPOSITORY \
  --number 42 \
  --shadow \
  --token-env GITHUB_TOKEN
```

The daemon can continuously enqueue the same GET-only reconciliation and hydration work:

```bash
GITHUB_TOKEN=... \
REVIEWD_DATABASE_PATH=data/control-plane.db \
REVIEWD_MIGRATION_MODE=check \
REVIEWD_SHADOW_RECONCILE_ENABLED=true \
REVIEWD_GITHUB_CONNECTION_ID=github-local \
REVIEWD_GITHUB_TOKEN_ENVIRONMENT=GITHUB_TOKEN \
REVIEWD_SHADOW_RECONCILE_INTERVAL=5m \
go run ./cmd/reviewd
```

`REVIEWD_GITHUB_TOKEN_ENVIRONMENT` contains a variable name, not a token. `REVIEWD_GITHUB_API_BASE_URL` defaults to `https://api.github.com`.

## Review workflow

1. Reconcile and hydrate a PR until it has current canonical evidence.
2. Create an immutable review profile version.
3. Queue a manual review against that exact profile and evidence.
4. Optionally run `reviewd` with a trusted local engine. The worker re-fetches and rebuilds evidence immediately before execution; drift fails the run rather than reviewing stale code.
5. Validate and persist an assessment. Policy evaluation can render an immutable proposal. A human may append a new proposal revision and decide it.

### Create a profile

Profiles are immutable by `(key, version)`. Input files are bounded regular files; settings must be one JSON object.

```bash
go run ./cmd/reviewctl profile create \
  --database data/control-plane.db \
  --key baseline \
  --version 1 \
  --name "Baseline review" \
  --description-file ./profile-description.txt \
  --instructions-file ./profile-instructions.txt \
  --settings-file ./profile-settings.json
```

Create a new version instead of editing an existing one.

### Queue a review

The PR coordinates must resolve to a current canonical revision. Allowed access modes are `diff_only`, `selected_files`, and `read_only_worktree`.

```bash
go run ./cmd/reviewctl review queue \
  --database data/control-plane.db \
  --connection-id github-local \
  --owner OWNER \
  --repository REPOSITORY \
  --number 42 \
  --profile-key baseline \
  --profile-version 1 \
  --access-mode diff_only
```

The command returns immutable intent/run identifiers and enqueues durable work. Without daemon review execution enabled, queued work remains durable but is not executed.

### Enable local engine execution

The engine command is a trusted JSON argv array, executed directly without a shell. It must read one review bundle JSON document from stdin and write exactly one assessment JSON document to stdout that satisfies the v1 contract. Its process environment is isolated: only `PATH`/locale values configured by adapter and per-run `HOME`/`TMPDIR` are available; GitHub credentials are not passed through.

```bash
GITHUB_TOKEN=... \
REVIEWD_DATABASE_PATH=data/control-plane.db \
REVIEWD_SHADOW_RECONCILE_ENABLED=true \
REVIEWD_GITHUB_CONNECTION_ID=github-local \
REVIEWD_GITHUB_TOKEN_ENVIRONMENT=GITHUB_TOKEN \
REVIEWD_REVIEW_EXECUTION_ENABLED=true \
REVIEWD_REVIEW_ENGINE_ARGV='["/absolute/path/to/review-engine","--json"]' \
go run ./cmd/reviewd
```

Review execution requires enabled shadow reconciliation because it uses that configured GET-only connection to rebuild evidence.

## Policy, proposal, and simulated publication

`reviewctl policy evaluate` evaluates one completed assessment against an already persisted active immutable rule version:

```bash
go run ./cmd/reviewctl policy evaluate \
  --database data/control-plane.db \
  --assessment-id ASSESSMENT_ID \
  --rule-key baseline-rule \
  --rule-version-id RULE_VERSION_ID
```

Create a complete immutable policy generation from a strict, secret-free JSON file. A new generation replaces the active-rule pointers as one atomic policy set; omitted rules are disabled, never deleted. A rule with `automatic` or `manual` trigger must name an existing immutable profile version.

```json
{
  "rules": [{
    "key": "baseline-rule",
    "enabled": true,
    "priority": 10,
    "trigger_kind": "manual",
    "external_action_policy": "require_confirmation",
    "profile_key": "baseline",
    "profile_version": 1,
    "match": {},
    "review": {},
    "publication": {"allow_automatic_approval": false}
  }]
}
```

```bash
go run ./cmd/reviewctl policy apply \
  --database data/control-plane.db \
  --generation 1 \
  --rules-file ./policy-rules.json
```

`policy evaluate` deliberately evaluates a named active rule/version rather than selecting one implicitly. It produces only evidence-bound local records and, when appropriate, a policy proposal.

Edit a policy-created proposal by appending a human revision, then record one human approval or rejection:

```bash
go run ./cmd/reviewctl proposal edit \
  --database data/control-plane.db \
  --proposal-id PROPOSAL_ID \
  --body-file ./proposal.md \
  --inline-comments-file ./inline-comments.json

go run ./cmd/reviewctl proposal decide \
  --database data/control-plane.db \
  --proposal-revision-id PROPOSAL_REVISION_ID \
  --decision approve \
  --actor-id local-human \
  --reason-file ./decision-reason.txt
```

Both commands reject likely secret-bearing arguments and file content. Decisions are immutable local evidence, not GitHub instructions.

`reviewctl proposal publish --proposal-revision-id ID --simulate` records an effect only for an approved, current proposal revision. In `simulated` mode it queues one local durable `{"simulated":true}` attempt; in `disabled` mode it records the effect without dispatch. There is no code path in this release that performs a GitHub write. Do not rely on this system to publish reviews.

## Control API and dashboard

Start `reviewd`, then open <http://127.0.0.1:8080/>. Dashboard shows current attention, immutable timeline records, local lifecycle analytics, and guarded local proposal edits/decisions. On load it obtains a short-lived, HttpOnly, SameSite-Strict session cookie from the loopback server; browser code never sees the credential. It cannot publish to GitHub; keep the listener on loopback.

```bash
curl http://127.0.0.1:8080/api/v1/health/live
curl http://127.0.0.1:8080/api/v1/health/ready
curl http://127.0.0.1:8080/api/v1/inbox?limit=50
curl 'http://127.0.0.1:8080/api/v1/pull-requests/PULL_REQUEST_ID/timeline?connection_id=github-local'
curl http://127.0.0.1:8080/api/v1/analytics/overview
```

`/api/inbox` and `/api/pull-requests/{id}/timeline` remain aliases. Both inbox and timeline support `limit` (1–100) and opaque `cursor`; timeline also requires `connection_id`.

## Configuration reference

| Variable | Default | Meaning |
| --- | --- | --- |
| `REVIEWD_DATABASE_PATH` | `data/control-plane.db` | Separate v2 SQLite database |
| `REVIEWD_LISTEN_ADDRESS` | `127.0.0.1:8080` | Loopback-only API listener |
| `REVIEWD_MIGRATION_MODE` | `check` | `check` or explicit `apply` |
| `REVIEWD_PUBLICATION_MODE` | `disabled` | `disabled` or local-only `simulated` |
| `REVIEWD_SHADOW_RECONCILE_ENABLED` | `false` | Enables GET-only scheduler |
| `REVIEWD_GITHUB_CONNECTION_ID` | empty | Local connection identity; required when scheduler enabled |
| `REVIEWD_GITHUB_API_BASE_URL` | `https://api.github.com` | GitHub REST API base URL |
| `REVIEWD_GITHUB_TOKEN_ENVIRONMENT` | empty | Name of process variable holding token |
| `REVIEWD_SHADOW_RECONCILE_INTERVAL` | `5m` | Positive scheduling interval |
| `REVIEWD_REVIEW_EXECUTION_ENABLED` | `false` | Enables local CLI review worker |
| `REVIEWD_REVIEW_ENGINE_ARGV` | empty | JSON argv array for trusted engine |

Inspect effective non-secret configuration:

```bash
go run ./cmd/reviewctl config validate
```

## Command index

```text
reviewctl config validate
reviewctl db status|migrate|backup|verify-backup
reviewctl legacy inspect|import
reviewctl github reconcile|hydrate
reviewctl profile create
reviewctl review queue
reviewctl policy evaluate
reviewctl proposal edit|decide
```

See [GREENFIELD_PRODUCT_DESIGN.md](docs/GREENFIELD_PRODUCT_DESIGN.md) for architecture and product rationale. [WEB_UI_README.md](WEB_UI_README.md) documents current control dashboard behavior.
