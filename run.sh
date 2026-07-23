#!/usr/bin/env bash

set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$root_dir"

# .env.v2 is the explicit Go control-plane configuration. Legacy .env remains
# useful for inherited credentials such as GITHUB_TOKEN, but its Python-only
# settings are deliberately ignored by reviewd.
if [[ -f .env ]]; then
  set -a
  . ./.env
  set +a
fi
if [[ -f .env.v2 ]]; then
  set -a
  . ./.env.v2
  set +a
fi

: "${REVIEWD_DATABASE_PATH:=data/control-plane.db}"
: "${REVIEWD_LISTEN_ADDRESS:=127.0.0.1:8080}"
: "${REVIEWD_MIGRATION_MODE:=apply}"
: "${REVIEWD_PUBLICATION_MODE:=disabled}"

export REVIEWD_DATABASE_PATH REVIEWD_LISTEN_ADDRESS REVIEWD_MIGRATION_MODE REVIEWD_PUBLICATION_MODE

go build ./cmd/reviewd
exec > >(tee -a data/reviewd.log) 2>&1
exec ./reviewd "$@"
