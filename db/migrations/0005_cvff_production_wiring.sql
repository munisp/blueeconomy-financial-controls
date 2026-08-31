-- Production wiring for the CVFF four-party rail:
-- 1. The CBN-rate conversion applied at disbursement time is persisted with
--    the rate's effective date, so the exact NGN<->USD basis of both ledger
--    legs is auditable (rate ID and micro value were already recorded; the
--    leg timestamp covers the capture time).
-- 2. The role-assignment and reconciliation-resolution production paths
--    emit their own outbox event types.

ALTER TABLE cvff_disbursement_legs
    ADD COLUMN rate_effective_date DATE NOT NULL;

ALTER TABLE cvff_outbox
    DROP CONSTRAINT cvff_outbox_event_type_check;

ALTER TABLE cvff_outbox
    ADD CONSTRAINT cvff_outbox_event_type_check CHECK (event_type IN (
        'cvff.application.submitted', 'cvff.underwriting_started', 'cvff.decision.recorded', 'cvff.sla_escalated',
        'cvff.disbursed', 'cvff.audited', 'cvff.reconciliation_required',
        'cvff.roles_assigned', 'cvff.reconciliation_resolved'
    ));
