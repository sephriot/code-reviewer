#!/usr/bin/env bash
set -euo pipefail

reviewd_e2e_dir="$(mktemp -d)"
reviewd_e2e_db="$reviewd_e2e_dir/control-plane.db"
reviewd_e2e_root="$(pwd)"
node e2e/fake-github.mjs &
reviewd_e2e_github_pid="$!"
trap 'kill "$reviewd_e2e_github_pid" 2>/dev/null || true' EXIT

until curl -fsS http://127.0.0.1:18081/user >/dev/null; do sleep 0.1; done

go run ./cmd/reviewctl db migrate --database "$reviewd_e2e_db" --apply
printf '%s\n' 'Fixture profile.' > "$reviewd_e2e_dir/profile-description.txt"
printf '%s\n' 'Return one valid v1 assessment JSON document.' > "$reviewd_e2e_dir/profile-instructions.txt"
printf '%s\n' '{}' > "$reviewd_e2e_dir/profile-settings.json"
go run ./cmd/reviewctl profile create \
  --database "$reviewd_e2e_db" \
  --key fixture \
  --version 1 \
  --name 'Fixture profile' \
  --description-file "$reviewd_e2e_dir/profile-description.txt" \
  --instructions-file "$reviewd_e2e_dir/profile-instructions.txt" \
  --settings-file "$reviewd_e2e_dir/profile-settings.json"
printf '%s\n' '{"rules":[{"key":"fixture-auto","enabled":true,"priority":1,"trigger_kind":"automatic","external_action_policy":"require_confirmation","profile_key":"fixture","profile_version":1,"match":{},"review":{},"publication":{"allow_automatic_approval":false}}]}' > "$reviewd_e2e_dir/policy-rules.json"
go run ./cmd/reviewctl policy apply --database "$reviewd_e2e_db" --generation 1 --rules-file "$reviewd_e2e_dir/policy-rules.json"
REVIEWD_DATABASE_PATH="$reviewd_e2e_db" \
REVIEWD_LISTEN_ADDRESS="127.0.0.1:18080" \
REVIEWD_MIGRATION_MODE="check" \
REVIEWD_PUBLICATION_MODE_ENABLED="false" \
REVIEWD_SHADOW_RECONCILE_ENABLED="true" \
REVIEWD_GITHUB_CONNECTION_ID="e2e-github" \
REVIEWD_GITHUB_API_BASE_URL="http://127.0.0.1:18081" \
REVIEWD_GITHUB_TOKEN_ENVIRONMENT="REVIEWD_E2E_GITHUB_TOKEN" \
REVIEWD_E2E_GITHUB_TOKEN="fixture-token" \
REVIEWD_REVIEW_EXECUTION_ENABLED="true" \
REVIEWD_REVIEW_ENGINE_ARGV="[\"/bin/bash\",\"$reviewd_e2e_root/e2e/fake-engine.sh\"]" \
go run ./cmd/reviewd
