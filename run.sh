#!/bin/bash
set -a
. ./.env
set +a

DIR="$(cd "$(dirname "$0")" && pwd)"

go build -o "$DIR/code-reviewer" "$DIR/cmd/code-reviewer/" || exit 1
export GITHUB_TOKEN=$(gh auth token)
echo "" > ./data/code-reviewer.log || true
exec "$DIR/code-reviewer" "$@"
