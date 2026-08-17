#!/usr/bin/env bash
# Runs against an authorized six-replica TigerBeetle environment. This script does
# not create a simulated authority, fake payment switch or production claim.
set -euo pipefail

: "${TB_QUORUM_TEST_ENABLED:?set TB_QUORUM_TEST_ENABLED=true after approved change authorization}"
: "${TB_QUORUM_AUTHORIZATION_REF:?supply the approved fault-injection change reference}"
: "${TIGERBEETLE_ADDRESSES:?supply six comma-separated Ministry-controlled replica addresses}"
: "${TB_QUORUM_FAULT_COMMAND:?supply an approved fault-injection command wrapper}"

if [[ "$TB_QUORUM_TEST_ENABLED" != "true" ]]; then
  echo 'six-replica fault injection is disabled' >&2
  exit 64
fi

IFS=',' read -r -a replicas <<< "$TIGERBEETLE_ADDRESSES"
if [[ ${#replicas[@]} -ne 6 ]]; then
  echo 'TIGERBEETLE_ADDRESSES must contain exactly six replica addresses' >&2
  exit 64
fi
for replica in "${replicas[@]}"; do
  [[ "$replica" =~ ^[A-Za-z0-9._:-]+$ ]] || { echo "invalid replica address: $replica" >&2; exit 64; }
done

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"
run_id="quorum-$(date -u +%Y%m%dT%H%M%SZ)"
artifact_dir="${TB_QUORUM_ARTIFACT_DIR:-$repo_root/.quorum-artifacts}/$run_id"
mkdir -p "$artifact_dir"
printf '%s\n' "authorization=$TB_QUORUM_AUTHORIZATION_REF" "replicas=$TIGERBEETLE_ADDRESSES" "run_id=$run_id" > "$artifact_dir/manifest.txt"

# The matching Go tests must be configured to create finite authorized financial
# intents and reconcile every client outcome against the real ledger.
run_case() {
  local case_id=$1
  local fault=$2
  echo "running $case_id" | tee -a "$artifact_dir/execution.log"
  TIGERBEETLE_ADDRESSES="$TIGERBEETLE_ADDRESSES" TB_QUORUM_CASE="$case_id" \
    go test ./internal/orchestration -count=1 -run '^TestSixReplicaQuorum$' -v | tee "$artifact_dir/$case_id.log"
  "$TB_QUORUM_FAULT_COMMAND" "$fault" "$run_id" | tee -a "$artifact_dir/$case_id.log"
  TIGERBEETLE_ADDRESSES="$TIGERBEETLE_ADDRESSES" TB_QUORUM_CASE="$case_id" \
    go test ./internal/orchestration -count=1 -run '^TestSixReplicaQuorumReconciliation$' -v | tee -a "$artifact_dir/$case_id.log"
}

run_case baseline noop
run_case non_leader_restart restart-non-leader
run_case leader_view_change stop-current-leader
run_case two_replica_failure isolate-two-replicas
run_case minority_partition isolate-minority
run_case quorum_loss remove-write-quorum
run_case replica_replacement replace-one-replica

echo "Six-replica resilience tests completed; independent finance reconciliation review is still required: $artifact_dir"
