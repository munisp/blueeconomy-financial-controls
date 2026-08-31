# Performance Notes (Phase 11 audit)

Scope: index coverage vs. actual query code, unbounded queries, N+1 patterns,
connection-pool sizing, Kafka producer batching. No behavior changes; all
fail-closed invariants preserved.

## Indexes added (db/migrations/0013_perf_indexes.sql)

| Index | Justifying query |
|---|---|
| `cvff_approvals_application_idx` | `cvff.Store.ListApprovals`: `WHERE application_id ORDER BY created_at, approval_id` |
| `fx_rates_pending_created_idx` (partial) | `fx.Store.ExpirePendingConfirmation` TTL sweep |
| `mojaloop_callbacks_timeout_sweep_idx` (partial) | `mojaloop.CallbackStore.MarkReservedTimeouts`: `WHERE transfer_state='RESERVED' AND timed_out_at IS NULL AND updated_at < $2` |
| `recon_exceptions_state_created_idx` | `revenue.Store.ListExceptionsPage`: `WHERE state='OPEN' ORDER BY created_at, exception_id` (existing `recon_exceptions_class_idx` leads with `class`) |
| `revenue_note_transitions_note_idx` | `revenue.Store.ListTransitions`: `WHERE debit_note_id ORDER BY created_at, transition_id` |
| `tf_applications_trader_idx` | `tradefinance.Store.ListApplications`: `WHERE trader_id ORDER BY created_at DESC, application_id` |
| `tariff_exemption_audit_assessment_idx` | `tariff.Store.ListExemptionAudits`: `WHERE assessment_id ORDER BY created_at` |
| `financial_intents_state_ref_idx` | `intent.Store.ListReconciliationIntents`: `WHERE state IN (...) ORDER BY external_ref` |
| `cvff_disbursement_legs_created_idx` | disbursement report window scan on `created_at` |

All use `IF NOT EXISTS`; none duplicate an index from migrations 0001-0012.

## Query caps / pagination

- `GET /v1/revenue/recon/exceptions` now accepts `limit` (default 500, hard max
  5000) via `revenue.Store.ListExceptionsPage`; ordering is deterministic so
  the cap is stable. `ListExceptions` keeps the old unlimited signature for
  internal/test callers.
- Mojaloop reserved-timeout sweep batches 500 rows per tick (`LIMIT 500 FOR
  UPDATE`); the worker ticks periodically so a backlog drains across ticks and
  one tick can no longer hold an unbounded lock list.
- `listExceptionsByID` now filters `WHERE exception_id = $1` in SQL instead of
  loading the entire exception table and filtering in memory.

## Connection pool sizing (env, opt-in)

`telemetry.ApplyPoolEnv` is applied in `Telemetry.NewPGXPool` and
`tradefinance.Open`. Unset variables keep the pgx defaults (unchanged
behavior):

- `DB_POOL_MAX_CONNS` (default: pgx default = max(4, NumCPU))
- `DB_POOL_MIN_CONNS` (default: 0)
- `DB_POOL_MAX_CONN_IDLE_SEC` (default: 1800)
- `DB_POOL_MAX_CONN_LIFE_SEC` (default: 3600)

Invalid values fail closed at startup.

## Kafka producer batching (env, opt-in)

`outbox.NewKafkaProducer` honors `KAFKA_BATCH_SIZE` (default 1, unchanged) and
`KAFKA_BATCH_TIMEOUT_MS` (default 0, unchanged). `RequiredAcks: RequireAll` is
untouched, so durability semantics are unchanged when batching is enabled.

## Remaining recommendations (not implemented)

- `revenue.RunRecon` unmatched-settlement/statement scans (`NOT EXISTS`
  anti-joins) are full-set passes by design; consider batching by
  `created_at` windows if tables grow past ~1M rows.
- `tariff.Store.ListRates` (admin view) is unbounded; rate cardinality is
  small and slowly changing, so no cap was added — revisit if agencies start
  bulk-loading historical rates.
- TigerBeetle ledger lookups are out of scope of the Postgres pool tuning.
