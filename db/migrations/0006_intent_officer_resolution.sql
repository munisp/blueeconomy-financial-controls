-- FC-2/FC-3b: audit event types for the officer AMBIGUOUS resolution and
-- the maker DRAFT void. The outbox CHECK from 0001 only admitted the three
-- original event types.
ALTER TABLE financial_intent_outbox
    DROP CONSTRAINT financial_intent_outbox_event_type_check;
ALTER TABLE financial_intent_outbox
    ADD CONSTRAINT financial_intent_outbox_event_type_check
    CHECK (event_type IN (
        'financial_intent.created',
        'financial_intent.approved',
        'financial_intent.state_changed',
        'financial_intent.officer_resolved',
        'financial_intent.voided'
    ));
