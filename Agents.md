# Agents

## Current runtime

`reviewd` is the local Go control plane. Start it with `./run.sh`, open
`http://127.0.0.1:8080`, and use `./reviewctl --help` for operator commands.

- Go code: `cmd/`, `internal/`, `migrations/sqlite/`.
- Current v2 state: `data/control-plane.db`.
- Legacy state: `data/reviews.db`, permanently read-only. Its verified backup
  remains under `data/backups/` and must never become the v2 runtime database.
- Configuration: `.env.v2`; `.env` is imported only to provide referenced
  credentials such as `GITHUB_TOKEN`.

## Review workflow

1. Startup performs GET-only reconciliation and canonical hydration.
2. A matching automatic rule creates durable `review.execute.v1` work.
3. Agent or Claude receives rebuilt immutable diff evidence through the native
   provider adapter and returns a validated v1 assessment.
4. Policy evaluates the assessment and creates a human-confirmed proposal when
   appropriate. Publication stays disabled unless explicitly changed.
5. Local preferences deliver browser, sound, speech, and log notifications for
   review start, completion, failure, and policy evaluation.

`REVIEWD_REVIEW_ENGINE_PROVIDER=agent` or `claude` selects an authenticated
local CLI. Providers use their normal local login state. Engine output is not
persisted as raw thinking; only a complete assessment JSON object survives
strict contract and diff-anchor validation.

## Safety rules

- Keep `REVIEWD_PUBLICATION_MODE_ENABLED=false` for local review operation unless
  publication is deliberately tested.
- Do not log or store GitHub tokens, provider auth values, or raw provider
  output.
- Preserve `data/reviews.db`; use v2 migrations only against
  `data/control-plane.db`.
- Review execution is idempotent in background. Dashboard **Queue eligible
  review** deliberately creates a new immutable run so operators can retry a
  previously failed provider run after configuration repair.

## Development

```bash
GOCACHE=/tmp/code-reviewer-go-build go test ./...
GOCACHE=/tmp/code-reviewer-go-build go vet ./...
GOCACHE=/tmp/code-reviewer-go-build go test -race ./...
pnpm test:e2e
```

Use `docs/REWRITE_STATUS.md` as handoff checklist, `README.md` for first use,
`docs/POLICIES.md` for rule selection, and `docs/OPERATIONS_RUNBOOK.md` for
backup/restore.
