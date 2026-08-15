#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tb_root="$root/integration/tigerbeetle"
data="$tb_root/data"
pg_compose=(sudo docker compose -f "$root/docker-compose.integration.yml")
tb_compose=(sudo docker compose -f "$tb_root/compose.yaml")
cleanup() {
  "${pg_compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  "${tb_compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$data"
}
trap cleanup EXIT
rm -rf "$data"
mkdir -p "$data"
for replica in 0; do
  sudo docker run --rm --network host --security-opt seccomp=unconfined -v "$data:/data" ghcr.io/tigerbeetle/tigerbeetle:0.17.9 \
    format --cluster=0 --replica="$replica" --replica-count=1 "/data/0_${replica}.tigerbeetle"
done
"${pg_compose[@]}" up -d --wait postgres
"${tb_compose[@]}" up -d
for _ in $(seq 1 120); do
  container=$("${tb_compose[@]}" ps -q tigerbeetle-0)
  if [[ -n "$container" ]] && sudo docker logs "$container" 2>&1 | grep -q 'listening on'; then break; fi
  sleep 1
done
for replica in tigerbeetle-0; do
  container=$("${tb_compose[@]}" ps -q "$replica")
  sudo docker logs "$container" 2>&1 | grep -q 'listening on'
done
cd "$root"
DATABASE_URL='postgres://blueeconomy:local-only-integration-password@127.0.0.1:55435/blueeconomy_finance?sslmode=disable' \
MIGRATION_PATH="$root/db/migrations/0001_financial_intents.sql" \
TIGERBEETLE_CLUSTER_ID_HEX='00000000000000000000000000000000' \
TIGERBEETLE_REPLICA_ADDRESSES='127.0.0.1:3001' \
go test -tags liveintegration -race ./internal/orchestration -run TestReserveApprovedAgainstLiveTigerBeetle -count=1
printf '%s\n' 'S3 live TigerBeetle orchestration passed: real accounts, approved PostgreSQL intent, pending transfer creation and ledger read-back.'
