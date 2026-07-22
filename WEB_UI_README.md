# Reviewd control dashboard

The v2 dashboard is a local control desk for durable control-plane state. It replaces none of GitHub’s review UI and does not expose legacy Python pending approvals.

## Run it

Migrate a separate v2 database first, then start `reviewd`:

```bash
go run ./cmd/reviewctl db migrate \
  --database data/control-plane.db \
  --apply

REVIEWD_DATABASE_PATH=data/control-plane.db \
REVIEWD_MIGRATION_MODE=check \
go run ./cmd/reviewd
```

Open <http://127.0.0.1:8080/>. Default listener is loopback-only. `REVIEWD_LISTEN_ADDRESS` rejects non-loopback addresses. Dashboard bootstraps a short-lived HttpOnly, SameSite-Strict local session cookie for mutation routes; it has no remote authentication and must not be exposed through a proxy.

## What dashboard shows

- Current attention items from canonical review/policy/proposal/publication state.
- One immutable timeline for selected pull request and local connection.
- Read-model availability status.
- Durable lifecycle analytics through the control API.
- Local proposal-revision edits plus approval/rejection of exact current revisions.
- No GitHub token display or publication action.

Attention is evidence-bound. When GitHub facts or canonical diff change, stale entries cannot be treated as current work.

## API

All responses have `Cache-Control: no-store`.

| Endpoint | Description |
| --- | --- |
| `GET /api/v1/health/live` | Process liveness |
| `GET /api/v1/health/ready` | DB and migration readiness |
| `GET /api/v1/inbox` | Current attention page |
| `GET /api/v1/pull-requests/{id}/timeline?connection_id=ID` | Immutable pull-request timeline |
| `GET /api/v1/analytics/overview` | Durable review lifecycle totals |
| `GET /api/v1/session` | Loopback-only opaque browser-session bootstrap |
| `POST /api/v1/mutate/proposals/{id}/revisions` | Append a local human proposal revision |
| `POST /api/v1/mutate/proposals/{id}/decisions` | Record one local decision for an owned revision |

`/api/inbox` and `/api/pull-requests/{id}/timeline` are unversioned aliases.

`inbox` accepts optional `limit` (1–100) and opaque `cursor`. Timeline accepts same pagination parameters and requires exactly one `connection_id`. Invalid parameters return a JSON `invalid_request` response. Read failures return a JSON `read_failed` response. Mutation routes require both a loopback remote address and the opaque session cookie; bearer values are rejected and are never exposed to dashboard JavaScript.

Examples:

```bash
curl http://127.0.0.1:8080/api/v1/health/live
curl http://127.0.0.1:8080/api/v1/health/ready
curl 'http://127.0.0.1:8080/api/v1/inbox?limit=50'
curl 'http://127.0.0.1:8080/api/v1/pull-requests/PULL_REQUEST_ID/timeline?connection_id=github-local&limit=50'
```

## Populate dashboard safely

Dashboard only reads local database. To observe GitHub, run explicit GET-only shadow reconciliation against an already migrated v2 database:

```bash
GITHUB_TOKEN=... go run ./cmd/reviewctl github reconcile \
  --database data/control-plane.db \
  --connection-id github-local \
  --shadow \
  --token-env GITHUB_TOKEN
```

Hydrate a selected PR to attach canonical diff evidence:

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

Token values are never sent to dashboard or stored in job payloads. `--token-env` takes only variable name; `--token-file` is optional alternative.

For background GET-only observation, configure `reviewd` with `REVIEWD_SHADOW_RECONCILE_ENABLED=true`, connection ID, token environment name, and a positive interval. See [README.md](README.md#observe-github-safely).

## Publication status

Dashboard is not a GitHub publication interface. Current release supports only:

- `REVIEWD_PUBLICATION_MODE=disabled`: no effect dispatch.
- `REVIEWD_PUBLICATION_MODE=simulated`: bounded worker records local simulated publication attempts for already-authorized effects.

Real GitHub approval, comment, and change-request publication is unavailable. A human proposal decision is local immutable evidence, not an external action.

## Legacy dashboard

Old FastAPI/Jinja UI, pending approvals, sound settings, and `data/reviews.db` belong to legacy Python application. They are not compatible with v2 dashboard. Preserve legacy database as read-only import input; never delete or recreate it to troubleshoot v2.
