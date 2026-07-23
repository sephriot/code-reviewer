#!/usr/bin/env bash
set -euo pipefail

reviewd_e2e_dir="$(mktemp -d)"
reviewd_e2e_db="$reviewd_e2e_dir/control-plane.db"

go run ./cmd/reviewctl db migrate --database "$reviewd_e2e_db" --apply
REVIEWD_DATABASE_PATH="$reviewd_e2e_db" \
REVIEWD_LISTEN_ADDRESS="127.0.0.1:18080" \
REVIEWD_MIGRATION_MODE="check" \
REVIEWD_PUBLICATION_MODE="disabled" \
go run ./cmd/reviewd
