# Go Rewrite Status

Last verified checkpoint: `a3fa373` (`feat(dashboard): queue selected evidence`)

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
  notification preferences.
- Selected PR can queue canonical evidence hydration from dashboard. It creates
  or reuses one durable `github.hydrate.v1` job and cannot write to GitHub.
- Review profiles, review runs, assessment validation, policy, proposals,
  decisions, simulated publication, guarded enabled publication, outbox, and
  local notification delivery have Go implementations and tests.

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
- [ ] Dashboard control to queue an eligible review and show durable run state.
- [ ] Dashboard setup/help for profiles, active policy rules, and trusted local
      review-engine configuration; retain CLI as complete fallback.
- [ ] End-to-end browser test: reconcile, hydrate, queue, run, policy proposal,
      human decision, and simulated publication.
- [ ] Clear operator diagnostics for failed reconciliation/hydration/review jobs
      in dashboard, not only log file/activity list.

### Complete design acceptance — larger follow-on

- [ ] Full React/TypeScript client specified in greenfield design. Current UI is
      production-oriented static HTML/JavaScript served by Go; it is functional
      but does not meet that technology decision.
- [ ] Writer-ownership guard shared with legacy, cutover checkpointing,
      reverse export, rollback suppression, and lock integration probes.
- [ ] Backup/restore/upgrade commandbook and cutover rehearsal against fixtures.
- [ ] Engine contract suite for Claude CLI, Codex CLI, Cursor Agent CLI, and a
      direct API adapter.
- [ ] Accessibility and responsive browser-suite completion.
- [ ] Retention/export controls and operational metrics/alerts.

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
- Recent important fix: credentials stored as `env:VARIABLE_NAME` must be
  normalized to `VARIABLE_NAME` only when resolving process environment. Do not
  pass a token value where a token environment-variable name is expected.
