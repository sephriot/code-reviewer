#!/usr/bin/env bash
set -euo pipefail

reviewd_e2e_dir="$(mktemp -d)"
reviewd_e2e_db="$reviewd_e2e_dir/control-plane.db"
node e2e/fake-github.mjs &
reviewd_e2e_github_pid="$!"
trap 'kill "$reviewd_e2e_github_pid" 2>/dev/null || true' EXIT

until curl -fsS http://127.0.0.1:18081/user >/dev/null; do sleep 0.1; done

go run ./cmd/reviewctl db migrate --database "$reviewd_e2e_db" --apply
REVIEWD_DATABASE_PATH="$reviewd_e2e_db" \
REVIEWD_LISTEN_ADDRESS="127.0.0.1:18080" \
REVIEWD_MIGRATION_MODE="check" \
REVIEWD_PUBLICATION_MODE="disabled" \
REVIEWD_SHADOW_RECONCILE_ENABLED="true" \
REVIEWD_GITHUB_CONNECTION_ID="e2e-github" \
REVIEWD_GITHUB_API_BASE_URL="http://127.0.0.1:18081" \
REVIEWD_GITHUB_TOKEN_ENVIRONMENT="REVIEWD_E2E_GITHUB_TOKEN" \
REVIEWD_E2E_GITHUB_TOKEN="fixture-token" \
go run ./cmd/reviewd
