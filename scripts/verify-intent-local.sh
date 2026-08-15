#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"
docker_prefix=()
if ! docker info >/dev/null 2>&1; then sudo -n docker info >/dev/null 2>&1 && docker_prefix=(sudo docker) || { echo 'Docker unavailable' >&2; exit 1; }; fi
compose=("${docker_prefix[@]}" compose -f docker-compose.integration.yml)
trap '"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true' EXIT
"${compose[@]}" up -d --wait postgres
"${docker_prefix[@]}" exec "$("${docker_prefix[@]}" ps --filter name=financial-controls-postgres -q | head -n1)" psql -p 55435 -U blueeconomy -d blueeconomy_finance -f - < db/migrations/0001_financial_intents.sql >/dev/null
DATABASE_URL='postgres://blueeconomy:local-only-integration-password@127.0.0.1:55435/blueeconomy_finance?sslmode=disable' \
MIGRATION_PATH="$repo_root/db/migrations/0001_financial_intents.sql" \
go test -tags=integration ./internal/intent -run TestRealPostgresIntentFlow -count=1
printf '%s\n' 'Financial intent real PostgreSQL integration passed: exact replay, conflict rejection, maker/checker approval, reservation request and outbox evidence.'
