# Code Reviewer — v2 control plane

`reviewd` is a local Go control plane for evidence-bound GitHub pull-request review. It keeps an append-only SQLite record of observations, canonical diffs, review runs, assessments, policy evaluations, proposals, decisions, and publication attempts.

This is a ground-up replacement. Legacy Python runtime, legacy dashboard, and `data/reviews.db` are not part of v2 operation.

## Safety model

- Use a separate v2 database: default `data/control-plane.db`.
- `data/reviews.db` is legacy input only. Never point `reviewd` or v2 migrations at it.
- GitHub discovery, PR detail, diff, file-list, and tree traffic are GET-only. A separate, bounded publisher can make one explicitly requested review write only in `enabled` mode.
- Review engines receive a bounded JSON bundle through stdin, run in a fresh directory, and receive no GitHub credentials or inherited environment.
- All review, policy, proposal, and publication records are immutable and tied to currently verified canonical evidence.
- `disabled` is default. `simulated` records a local simulated attempt only. `enabled` requires configured shadow reconciliation, a GitHub token, and explicit dispatch; it revalidates live diff anchors before a single non-retrying GitHub write.

See [operations runbook](docs/OPERATIONS_RUNBOOK.md) for backup, upgrade,
restore, and cutover rehearsal.

## Prerequisites

- Go version declared in [`go.mod`](go.mod)
- A GitHub token only for optional GET-only reconciliation/hydration/review evidence reads
- A trusted local review-engine executable only when enabling review execution

Run tests:

```bash
go test ./...
```

## Set up the control-plane database

For normal local startup, copy the v2 template and run the launcher. `run.sh`
loads `.env.v2` first, then falls back to legacy `.env` only for inherited
credentials such as `GITHUB_TOKEN`; legacy Python settings do not configure
the Go daemon.

```bash
cp .env.v2.example .env.v2
./run.sh
```

Open <http://127.0.0.1:8080/> after startup. The launcher defaults to the
separate `data/control-plane.db`, applies known migrations, and keeps
publication disabled.

## Using `reviewctl`

`reviewctl` is the operator CLI. It is not required for normal dashboard use:
start `reviewd`, let it reconcile, then select PRs in the Control Desk. Use
`reviewctl` when bootstrapping, diagnosing, importing history, or deliberately
changing review policy.

Start with discovery, never guessed commands:

```bash
go run ./cmd/reviewctl --help
go run ./cmd/reviewctl db migrate --help
```

Normal safe operator path:

1. `./run.sh`; open the dashboard and confirm **Read model online**.
2. Check database state: `go run ./cmd/reviewctl db status --database data/control-plane.db`.
3. Let startup reconciliation find PRs. Use dashboard **Build canonical evidence** for a selected PR when needed.
4. Use `github reconcile` or `github hydrate` only for a manual GET-only retry.
5. Create a profile and apply a policy only when ready to execute trusted local reviews.
6. Keep publication `disabled`; dashboard simulation never writes GitHub. Enabled dispatch is a separate deliberate cutover step.

Command map:

| Need | Command group | Safety |
| --- | --- | --- |
| Check or migrate v2 database | `db status`, `db migrate --apply` | Migration changes only v2 DB. |
| Preserve/import old Python history | `db backup`, `legacy inspect`, `legacy import --apply` | Legacy DB is source-only. |
| Discover or hydrate PR facts | `github reconcile --shadow`, `github hydrate --shadow` | GitHub GET only. |
| Define review behavior | `profile create`, `policy apply` | Immutable local records. |
| Run a review | `review queue`, `review schedule` | Requires canonical evidence and trusted engine. |
| Make a human decision | `proposal edit`, `proposal decide` | Append-only; no GitHub write. |
| Record/safely dispatch publication | `proposal publish --simulate` | Simulation is local; `--dispatch` needs enabled mode. |

Never pass a GitHub token as a command argument. Use `--token-env GITHUB_TOKEN`.
Never use `data/reviews.db` as `--database`; it is legacy input only.
Enabled publication stores its separate local writer lock under
`data/writer-ownership` by default; set `REVIEWD_WRITER_OWNERSHIP_STATE_DIR`
only when both writer binaries can access the same local state directory.
Before cutover, verify host lock behavior with:

```bash
go run ./cmd/reviewctl db ownership-probe --state-dir data/writer-ownership
```

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

## Receive GitHub webhooks safely

Optional webhook ingress listens only on the same loopback control listener. Put a trusted local tunnel or proxy in front of it if GitHub must reach this machine; do not expose `reviewd` directly. Enable it with a reference to an environment variable holding the GitHub webhook signing secret:

```bash
REVIEWD_GITHUB_WEBHOOK_ENABLED=true \
REVIEWD_GITHUB_WEBHOOK_SECRET_ENVIRONMENT=GITHUB_WEBHOOK_SECRET \
reviewd
```

`REVIEWD_GITHUB_WEBHOOK_SECRET_ENVIRONMENT` contains only the environment-variable name. The secret is neither saved nor returned. This foundation verifies `X-Hub-Signature-256`, accepts bounded `ping`, `pull_request`, and `pull_request_review` payloads, and retains only delivery metadata plus payload hash for idempotency. When GET-only shadow reconciliation is enabled, verified delivery schedules its existing durable reconciliation job; ingress never calls or publishes to GitHub. Periodic reconciliation remains correctness source.

## Review workflow

1. Reconcile and hydrate a PR until it has current canonical evidence.
2. Create an immutable review profile version.
3. Queue a manual review against that exact profile and evidence.
4. Optionally run `reviewd` with a trusted local engine. The worker re-fetches and rebuilds evidence immediately before execution; drift fails the run rather than reviewing stale code.
5. Validate and persist an assessment. Policy evaluation can render an immutable proposal. A human may append a new proposal revision and decide it.

### Create a profile

Profiles are immutable by `(key, version)`. Input files are bounded regular files; settings must be one JSON object.

- `--description-file`: short human-facing purpose. Dashboard and operators use it to identify profile intent.
- `--instructions-file`: immutable instructions passed into each review bundle; this is where review scope and output expectations live.
- `--settings-file`: strict JSON object for profile-specific engine-neutral settings. It is retained with profile version, not interpreted as shell arguments.

Copy the tracked example rather than creating opaque local files:

```bash
cp -R examples/review-profiles ./local-review-profile
```

```bash
go run ./cmd/reviewctl profile create \
  --database data/control-plane.db \
  --key baseline \
  --version 1 \
  --name "Baseline review" \
  --description-file examples/review-profiles/baseline-description.txt \
  --instructions-file examples/review-profiles/baseline-instructions.txt \
  --settings-file examples/review-profiles/baseline-settings.json
```

Create a new version instead of editing an existing one.

Apply the matching safe policy. It may queue reviews automatically, but every pass produces a human-confirmed proposal; it cannot auto-publish GitHub approval.

See [policy guide](docs/POLICIES.md) for `match`, `review`, priority, trigger,
and publication fields. In short: `match` selects PRs; `review` is retained
future execution metadata and is not interpreted yet, so use `{}`.

### Enable trusted local review execution

After a profile and policy exist, select your already-authenticated native CLI:

```dotenv
REVIEWD_REVIEW_EXECUTION_ENABLED=true
REVIEWD_REVIEW_ENGINE_PROVIDER=codex
REVIEWD_REVIEW_ENGINE_AUTH_ROOT=.reviewd/engine-auth
```

Provider mode requires no argv: `claude`, `codex`, or `agent` selects matching
installed CLI. Set `REVIEWD_REVIEW_ENGINE_ARGV='["/path/to/codex"]'` only to
override executable path. Without a provider, argv is JSON, not a shell command.
The engine reads one evidence bundle from stdin
and must return one v1 assessment JSON object on stdout. It receives no GitHub
token. Stop `./run.sh` with `Ctrl-C`, run `./run.sh` again, then confirm
**Review runtime enabled** in Control Desk settings. Keep it `false` until the
adapter has been tested.

```bash
go run ./cmd/reviewctl policy apply \
  --database data/control-plane.db \
  --generation 1 \
  --rules-file examples/review-profiles/baseline-policy.json
```

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

## Policy, proposal, and guarded publication

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

Rule `match` supports strict, fail-closed predicates: `relationships`, `repository_ids`, `repository_names`, `authors`, `labels`, `is_draft`, `states`, and `base_refs`. Predicates combine with AND; relationship and label arrays require all listed values, while repository, author, state, and base-ref arrays match any listed value. `repository_names` accepts exact `owner/repository` or organization-wide `owner/*`; other globs and regular expressions are rejected. Unknown keys, duplicate keys, malformed values, or invalid current facts block selection.

To select the first enabled rule by priority and queue exactly one durable review only when its trigger is `automatic`:

```bash
go run ./cmd/reviewctl review schedule \
  --database data/control-plane.db \
  --connection-id github-local \
  --owner OWNER \
  --repository REPOSITORY \
  --number 42
```

`manual`, `track_only`, and `ignore` rules are reported but do not queue work. Scheduling requires current verified canonical evidence and an immutable profile attached to an automatic rule.

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

`reviewctl proposal publish --proposal-revision-id ID --simulate` records an effect only for an approved, current proposal revision. In `simulated` mode it queues one local durable `{"simulated":true}` attempt; in `disabled` mode it records the effect without dispatch. It refuses enabled mode.

`reviewctl proposal publish --proposal-revision-id ID --dispatch` works only in `enabled` mode and queues one guarded publication job for `reviewd`; it does not write from `reviewctl`. Enabled runtime requires configured shadow reconciliation and GitHub token. Before posting, worker fetches live diff, converts invalid inline comments to review-body findings, claims durable one-shot dispatch, and records success or uncertainty without automatic retry.

Dashboard users may instead select an observed pull request and choose **Build
canonical evidence**. This local authenticated action queues one deduplicated
`github.hydrate.v1` job for current metadata evidence; it performs only bounded
GET requests and cannot publish to GitHub.

An uncertain enabled delivery has no repost command. After inspecting GitHub, an operator can permanently record either the verified external result or abandonment:

```sh
go run ./cmd/reviewctl publication resolve \
  --database data/control-plane.db \
  --effect-id EFFECT_ID \
  --resolution externally_completed \
  --actor-id local-user
```

Use `--resolution abandoned` when no external review should be treated as delivered. Optional `--reason-file` stores a bounded, secret-free audit note. Resolution never queues a job or sends GitHub traffic.

## Control API and dashboard

Start `reviewd`, then open <http://127.0.0.1:8080/>. Dashboard shows current attention, immutable timeline records, local lifecycle analytics, guarded local proposal edits/decisions, and explicit local publication simulation. In `enabled` mode only, an extra browser-confirmed control can queue one guarded GitHub publication. Browser notification preference delivers pending local browser notifications through this open loopback dashboard; grant browser permission when prompted. The Local notifications panel lets you edit speech text for review start, completion, failure, and policy evaluation; leave text empty to silence that event. Policy evaluation is silent by default. On macOS, sound and speech preferences use fixed local `afplay` and `say` commands; unsupported hosts retain a suppressed delivery record. On load it obtains a short-lived, HttpOnly, SameSite-Strict session cookie from the loopback server; browser code never sees the credential. Keep the listener on loopback.

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
| `REVIEWD_PUBLICATION_MODE` | `disabled` | `disabled`, local-only `simulated`, or guarded `enabled` |
| `REVIEWD_SHADOW_RECONCILE_ENABLED` | `false` | Enables GET-only scheduler |
| `REVIEWD_GITHUB_CONNECTION_ID` | empty | Local connection identity; required when scheduler enabled |
| `REVIEWD_GITHUB_API_BASE_URL` | `https://api.github.com` | GitHub REST API base URL |
| `REVIEWD_GITHUB_TOKEN_ENVIRONMENT` | empty | Name of process variable holding token |
| `REVIEWD_SHADOW_RECONCILE_INTERVAL` | `5m` | Positive scheduling interval |
| `REVIEWD_REVIEW_EXECUTION_ENABLED` | `false` | Enables local CLI review worker |
| `REVIEWD_REVIEW_ENGINE_ARGV` | empty | JSON argv array for trusted engine |
| `REVIEWD_GITHUB_WEBHOOK_ENABLED` | `false` | Enables loopback-only signed GitHub webhook ingress |
| `REVIEWD_GITHUB_WEBHOOK_SECRET_ENVIRONMENT` | empty | Name of process variable holding webhook signing secret; required when enabled |

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
reviewctl proposal edit|decide|publish
reviewctl publication resolve
```

See [GREENFIELD_PRODUCT_DESIGN.md](docs/GREENFIELD_PRODUCT_DESIGN.md) for architecture and product rationale. Runtime dashboard behavior and current implementation state live in [REWRITE_STATUS.md](docs/REWRITE_STATUS.md).
