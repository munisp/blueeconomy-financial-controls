CREATE TABLE cvff_applications (
    application_id TEXT PRIMARY KEY,
    external_ref TEXT NOT NULL UNIQUE,
    beneficiary_id TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL CHECK (currency IN ('NGN', 'USD')),
    state TEXT NOT NULL CHECK (state IN (
        'SUBMITTED',
        'UNDERWRITING_PRIMARY', 'UNDERWRITING_SECONDARY', 'UNDERWRITING_TERTIARY',
        'NIMASA_APPROVAL', 'BANK_CONFIRMATION',
        'DISBURSEMENT_PENDING', 'DISBURSED', 'AUDITED',
        'REJECTED', 'RECONCILIATION_REQUIRED'
    )),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0)
);

CREATE TABLE cvff_role_assignments (
    application_id TEXT NOT NULL REFERENCES cvff_applications(application_id),
    role TEXT NOT NULL CHECK (role IN (
        'UNDERWRITER_PRIMARY', 'UNDERWRITER_SECONDARY', 'UNDERWRITER_TERTIARY',
        'NIMASA_APPROVER', 'RECEIVING_BANK', 'BENEFICIARY'
    )),
    principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (application_id, role),
    -- Strict separation of duties: one principal holds at most one role per application.
    UNIQUE (application_id, principal_id)
);

CREATE TABLE cvff_approvals (
    approval_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES cvff_applications(application_id),
    role TEXT NOT NULL CHECK (role IN (
        'UNDERWRITER_PRIMARY', 'UNDERWRITER_SECONDARY', 'UNDERWRITER_TERTIARY',
        'NIMASA_APPROVER', 'RECEIVING_BANK', 'BENEFICIARY'
    )),
    principal_id TEXT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('APPROVE', 'REJECT')),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

-- Approval entries are immutable audit evidence.
CREATE FUNCTION cvff_approvals_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'cvff_approvals entries are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER cvff_approvals_no_update
    BEFORE UPDATE OR DELETE ON cvff_approvals
    FOR EACH ROW EXECUTE FUNCTION cvff_approvals_immutable();

CREATE TABLE fx_rates (
    rate_id UUID PRIMARY KEY,
    base_currency TEXT NOT NULL CHECK (base_currency = 'USD'),
    quote_currency TEXT NOT NULL CHECK (quote_currency = 'NGN'),
    ngn_per_usd_micro BIGINT NOT NULL CHECK (ngn_per_usd_micro > 0),
    effective_date DATE NOT NULL,
    maker TEXT NOT NULL,
    checker TEXT,
    state TEXT NOT NULL CHECK (state IN ('PENDING_CONFIRMATION', 'CONFIRMED', 'REJECTED')),
    created_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    CHECK (checker IS NULL OR checker <> maker)
);

-- At most one confirmed CBN reference rate per effective date.
CREATE UNIQUE INDEX fx_rates_confirmed_date_idx ON fx_rates (effective_date) WHERE state = 'CONFIRMED';

ALTER TABLE financial_intents
    ADD CONSTRAINT financial_intents_currency_check CHECK (currency IN ('NGN', 'USD'));

CREATE TABLE cvff_disbursement_legs (
    application_id TEXT PRIMARY KEY REFERENCES cvff_applications(application_id),
    rate_id UUID NOT NULL REFERENCES fx_rates(rate_id),
    ngn_per_usd_micro BIGINT NOT NULL CHECK (ngn_per_usd_micro > 0),
    fee_transfer_id TEXT NOT NULL,
    cost_transfer_id TEXT NOT NULL,
    fee_ngn_minor BIGINT NOT NULL CHECK (fee_ngn_minor > 0),
    cost_usd_minor BIGINT NOT NULL CHECK (cost_usd_minor > 0),
    cost_ngn_equivalent BIGINT NOT NULL CHECK (cost_ngn_equivalent > 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE cvff_outbox (
    event_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES cvff_applications(application_id),
    event_type TEXT NOT NULL CHECK (event_type IN (
        'cvff.application.submitted', 'cvff.underwriting_started', 'cvff.decision.recorded', 'cvff.sla_escalated',
        'cvff.disbursed', 'cvff.audited', 'cvff.reconciliation_required'
    )),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);

CREATE INDEX cvff_outbox_unpublished_idx ON cvff_outbox (created_at) WHERE published_at IS NULL;
