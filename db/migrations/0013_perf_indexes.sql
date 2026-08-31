-- Phase 11 performance audit: missing indexes justified by actual query code.
-- All statements are idempotent (IF NOT EXISTS) and none duplicate an index
-- already declared in migrations 0001-0012.

-- cvff.Store.ListApprovals / beneficiary pipeline reads:
--   WHERE application_id = $1 ORDER BY created_at, approval_id
CREATE INDEX IF NOT EXISTS cvff_approvals_application_idx
    ON cvff_approvals (application_id, created_at, approval_id);

-- fx.Store.ExpirePendingConfirmation TTL sweep (cvff-worker):
--   WHERE state = 'PENDING_CONFIRMATION' AND created_at < $1
CREATE INDEX IF NOT EXISTS fx_rates_pending_created_idx
    ON fx_rates (created_at) WHERE state = 'PENDING_CONFIRMATION';

-- mojaloop.CallbackStore.MarkReservedTimeouts sweep:
--   WHERE transfer_state = 'RESERVED' AND timed_out_at IS NULL AND updated_at < $2
-- The existing idx_mojaloop_transfer_callbacks_state covers transfer_state
-- only; the timeout marker + updated_at window make this composite selective.
CREATE INDEX IF NOT EXISTS mojaloop_callbacks_timeout_sweep_idx
    ON mojaloop_transfer_callbacks (transfer_state, updated_at)
    WHERE timed_out_at IS NULL;

-- revenue.Store.ListExceptions (GET /v1/revenue/recon/exceptions):
--   WHERE state = 'OPEN' ORDER BY created_at, exception_id
-- The existing recon_exceptions_class_idx leads with class and does not serve
-- a state-only predicate or the queue ordering.
CREATE INDEX IF NOT EXISTS recon_exceptions_state_created_idx
    ON recon_exceptions (state, created_at, exception_id);

-- revenue.Store.ListTransitions (GET .../debit-notes/{id}/transitions):
--   WHERE debit_note_id = $1 ORDER BY created_at, transition_id
CREATE INDEX IF NOT EXISTS revenue_note_transitions_note_idx
    ON revenue_debit_note_transitions (debit_note_id, created_at, transition_id);

-- tradefinance.Store.ListApplications (GET /v1/tradefinance/applications):
--   WHERE trader_id = $1 ORDER BY created_at DESC, application_id
CREATE INDEX IF NOT EXISTS tf_applications_trader_idx
    ON tf_applications (trader_id, created_at DESC, application_id);

-- tariff.Store.ListExemptionAudits (GET .../assessments/{id}/exemption-audits):
--   WHERE assessment_id = $1 ORDER BY created_at
CREATE INDEX IF NOT EXISTS tariff_exemption_audit_assessment_idx
    ON tariff_exemption_audit (assessment_id, created_at);

-- intent.Store.ListReconciliationIntents (settlement-sync):
--   WHERE state IN ('POSTED','VOIDED') ORDER BY external_ref
CREATE INDEX IF NOT EXISTS financial_intents_state_ref_idx
    ON financial_intents (state, external_ref);

-- cvff disbursement report window scan (internal/cvff/disbursement.go):
--   WHERE l.created_at >= $1 AND l.created_at < $2 ORDER BY l.created_at
CREATE INDEX IF NOT EXISTS cvff_disbursement_legs_created_idx
    ON cvff_disbursement_legs (created_at);
