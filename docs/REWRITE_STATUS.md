# Go Rewrite Status

Last implementation checkpoint: `6a92c06` (`fix(engine): expose safe provider output shape`).
Last full Go suite checkpoint: `1005372` (`go test ./...`, `go vet
./...`, and `go test -race ./...`).
Last browser fixture checkpoint: current stage (`pnpm test:e2e`).

This file is the handoff checklist for work after the Go rewrite begins. Update
it in the same commit as every meaningful implementation stage.

## Current product state

`reviewd` is a working local Go control plane, not a blank scaffold.

- Separate v2 SQLite database: `data/control-plane.db`.
- Legacy database: `data/reviews.db`, read-only and checksum-protected.
- `run.sh` builds and starts Go `reviewd` on loopback port 8080 and appends
  process output to `data/reviewd.log`.
- Startup schedules GET-only GitHub reconciliation and canonical hydration.
- Dashboard shows observed pull requests when no attention record exists,
  immutable timeline, durable activity, history, analytics, settings, and
  notification preferences. Inbox, Runtime Activity, history, analytics, and
  settings refresh every 10 seconds without page reload.
- Selected PR can queue canonical evidence hydration from dashboard. It creates
  or reuses one durable `github.hydrate.v1` job and cannot write to GitHub.
- Review profiles, review runs, assessment validation, policy, proposals,
  decisions, simulated publication, guarded enabled publication, outbox, and
  local notification delivery have Go implementations and tests.
- Writer-ownership foundation holds an exclusive local advisory lock and a
  monotonic ownership generation in separate SQLite state. Runtime credential
  and publisher wiring remains outstanding.

## Verified facts

- Last full verification before this status update:
  `go test ./...`, `go vet ./...`, `go test -race ./...` all passed.
- Legacy database SHA-256:
  `682e7096fd28b1c8035fab77ae5c32c296c4bd45ec42ba7ef69464804a2a7fe3`.
- Current intended default is safe observation:
  `REVIEWD_PUBLICATION_MODE=disabled`, review execution disabled, and GitHub
  reconciliation GET-only.

## Operator path today

1. Start with `./run.sh`.
2. Open `http://127.0.0.1:8080`.
3. Confirm `Read model online`, observed PR cards, and Runtime Activity.
4. Select a PR; use **Build canonical evidence** when current canonical diff is
   absent. Check Runtime Activity and `data/reviewd.log` for job outcome.
5. To execute reviews, create an immutable profile and policy rule, then set
   `REVIEWD_REVIEW_EXECUTION_ENABLED=true` and trusted
   `REVIEWD_REVIEW_ENGINE_ARGV`. This is intentionally off by default.

## Remaining work

### Operational alpha — next, in order

- [x] Dashboard selected-PR facts: current evidence state, run count, proposal
      count; hydration is disabled when canonical evidence exists.
- [x] Dashboard control to re-check automatic policy and queue an eligible
      review. Runtime Activity shows durable job outcome.
- [x] Dashboard readiness guide for profiles, active policy rules, and trusted
      local review-engine configuration; `reviewctl` remains complete fallback.
- [x] Isolated Playwright dashboard smoke test starts a temporary v2 database
      and verifies local control-desk bootstrap.
- [x] Isolated Playwright fixture verifies GET-only reconciliation, canonical
      hydration, and live inbox refresh against a fake loopback GitHub API.
- [x] Isolated Playwright fixture verifies automatic-rule selection and trusted
      engine execution after canonical hydration.
- [x] End-to-end browser workflow: reconcile, hydrate, queue, run, policy
      proposal, human decision, and disabled-mode simulated publication.
- [x] Runtime Activity exposes bounded durable job failure class and reason for
      reconciliation, hydration, and review diagnosis.

### Local review MVP — required before legacy removal

- [x] Product decision: retain embedded lightweight HTML/CSS/JavaScript control
      desk. React/TypeScript rewrite is explicitly removed from scope unless
      future product pressure justifies it.
- [x] One native provider completes a real local Agent review from canonical
      evidence through persisted assessment and policy proposal. Agent uses its
      normal authenticated environment and an isolated trusted bridge workspace.
- [ ] Claude native adapter real-review parity. Its authenticated structured
      output invocation has been smoke-checked; run one dashboard review before
      declaring parity.
- [x] Clear terminal provider/auth/output diagnostics in Runtime Activity and
      `data/reviewd.log`; no retry storm for local configuration failures.
- [x] Review lifecycle notifications: `review.started`, `review.completed`,
      and `review.failed` use existing local Sound/TTS/Browser/Log preferences.
- [ ] Organization wildcard policy selection (`owner/*`) covered by tests and
      documented.
- [ ] Backup/restore commandbook and one fixture rehearsal.
- [ ] Final Go-only cutover: remove legacy Python runtime and its dependencies
      only after the above local review workflow is proven.

### Explicitly deferred

- Reverse export, rollback suppression, and shared legacy/v2 writer transfer.
- Multi-user/server control plane, retention controls, alerts, and a direct API
  review-engine adapter.
- Full accessibility/responsive completion beyond current local dashboard.

## Safety rules

- Never write to `data/reviews.db`; use only `data/control-plane.db` for v2.
- Never put token values in logs, UI, database job payloads, commits, or docs.
- Keep GitHub traffic GET-only until publication mode is deliberately enabled.
- Do not stage generated `reviewd` binary or operator-local `.env*` files.
- Preserve unrelated local edits, especially `run.sh`, unless explicitly
  included in a requested stage.

## Handoff notes

- Normative architecture: `docs/GREENFIELD_PRODUCT_DESIGN.md`.
- Runtime and dashboard commands: `README.md`, `WEB_UI_README.md`.
- `data/reviewd.log` is process output. Durable job/event/outbox state is
  available from dashboard Runtime Activity and `/api/v1/activity`.
- Operator command discovery and safe first-use workflow live in README's
  **Using `reviewctl`** section; use `reviewctl --help` before command flags.
- Tracked `examples/review-profiles/` files show profile description,
  instructions, settings, and a human-confirmed automatic policy.
- `docs/POLICIES.md` documents policy fields and explicitly marks `review` as
  retained-but-not-yet-interpreted runtime metadata.
- `REVIEWD_WRITER_OWNERSHIP_STATE_DIR` makes shared local writer lock location
  explicit for future controlled cutover.
- `reviewctl db ownership-probe` proves one exclusive local lock and a durable
  heartbeat before writer cutover.
- Recent important fix: credentials stored as `env:VARIABLE_NAME` must be
  normalized to `VARIABLE_NAME` only when resolving process environment. Do not
  pass a token value where a token environment-variable name is expected.
- Native provider output is never persisted as raw agent thinking or text. The
  runtime records safe output shape diagnostics; agent framing is reduced to a
  complete JSON object and then validated against immutable diff evidence.
