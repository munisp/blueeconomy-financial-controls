-- WP-6 trade-finance rail (CamelONE-style multi-bank trade finance).
-- Consent registry (scoped, time-boxed, revocable, maker-checker,
-- hash-chained audit) and the standardized product workflow reusing the
-- four-party approval machinery. All invariants are DB-enforced.

CREATE TABLE tf_consents (
    consent_id TEXT PRIMARY KEY,
    trader_id TEXT NOT NULL,
    bank_id TEXT NOT NULL,
    scopes TEXT[] NOT NULL CHECK (array_length(scopes, 1) >= 1),
    dataset_refs JSONB NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'PENDING_CHECKER', 'ACTIVE', 'REJECTED', 'REVOCATION_PENDING', 'REVOKED'
    )),
    expires_at TIMESTAMPTZ NOT NULL,
    maker_principal TEXT NOT NULL,
    checker_principal TEXT,
    envelope_jws TEXT,
    envelope_bundle JSONB,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    -- Maker/checker separation is durable: the checker is never the maker.
    CHECK (checker_principal IS NULL OR checker_principal <> maker_principal)
);

-- A bank/trader pair has at most one non-terminal consent in flight.
CREATE UNIQUE INDEX tf_consents_open_idx ON tf_consents (trader_id, bank_id)
    WHERE state IN ('PENDING_CHECKER', 'ACTIVE', 'REVOCATION_PENDING');

CREATE TABLE tf_consent_audit (
    audit_id UUID PRIMARY KEY,
    consent_id TEXT NOT NULL REFERENCES tf_consents(consent_id),
    seq BIGINT NOT NULL,
    actor_principal TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN (
        'REQUESTED', 'ACTIVATED', 'REJECTED', 'REVOCATION_REQUESTED', 'REVOKED', 'REVOCATION_REJECTED'
    )),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    entry_hash TEXT NOT NULL,
    prev_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (consent_id, seq)
);

-- Consent audit entries are immutable evidence; the hash chain is only
-- meaningful when history cannot be rewritten.
CREATE FUNCTION tf_consent_audit_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'tf_consent_audit entries are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tf_consent_audit_no_update
    BEFORE UPDATE OR DELETE ON tf_consent_audit
    FOR EACH ROW EXECUTE FUNCTION tf_consent_audit_immutable();

CREATE TABLE tf_applications (
    application_id TEXT PRIMARY KEY,
    external_ref TEXT NOT NULL UNIQUE,
    trader_id TEXT NOT NULL,
    bank_id TEXT NOT NULL,
    consent_id TEXT NOT NULL REFERENCES tf_consents(consent_id),
    product TEXT NOT NULL CHECK (product IN (
        'IMPORT_LC_FACILITATION', 'EXPORT_PRESHIPMENT_FINANCE',
        'INVOICE_RECEIVABLES_FINANCE', 'DUTY_DEFERRAL_GUARANTEE'
    )),
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL CHECK (currency IN ('NGN', 'USD')),
    state TEXT NOT NULL CHECK (state IN (
        'APPLICATION', 'KYC_CONSENT_CHECK', 'BANK_REVIEW', 'REGULATORY_CLEARANCE',
        'APPROVED', 'DISBURSEMENT_PENDING', 'DISBURSED', 'SETTLED', 'DECLINED'
    )),
    facility_account_id TEXT,
    settlement_account_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0)
);

CREATE TABLE tf_role_assignments (
    application_id TEXT NOT NULL REFERENCES tf_applications(application_id),
    role TEXT NOT NULL CHECK (role IN (
        'BANK_KYC_OFFICER', 'BANK_CREDIT_OFFICER', 'CUSTOMS_COMPLIANCE_OFFICER',
        'BANK_TREASURY_OFFICER', 'TRADER'
    )),
    principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (application_id, role),
    -- Strict separation of duties: one principal holds at most one role per application.
    UNIQUE (application_id, principal_id)
);

CREATE TABLE tf_approvals (
    approval_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES tf_applications(application_id),
    role TEXT NOT NULL CHECK (role IN (
        'BANK_KYC_OFFICER', 'BANK_CREDIT_OFFICER', 'CUSTOMS_COMPLIANCE_OFFICER',
        'BANK_TREASURY_OFFICER', 'TRADER'
    )),
    principal_id TEXT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('APPROVE', 'REJECT')),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION tf_approvals_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'tf_approvals entries are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tf_approvals_no_update
    BEFORE UPDATE OR DELETE ON tf_approvals
    FOR EACH ROW EXECUTE FUNCTION tf_approvals_immutable();

CREATE TABLE tf_transitions (
    transition_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES tf_applications(application_id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    actor_role TEXT NOT NULL,
    actor_principal_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE tf_disbursement_legs (
    application_id TEXT PRIMARY KEY REFERENCES tf_applications(application_id),
    reserve_transfer_id TEXT NOT NULL,
    disburse_transfer_id TEXT,
    settle_transfer_id TEXT,
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL CHECK (currency IN ('NGN', 'USD')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE tf_outbox (
    event_id UUID PRIMARY KEY,
    subject_id TEXT NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN (
        'tradefinance.consent.requested', 'tradefinance.consent.activated',
        'tradefinance.consent.rejected', 'tradefinance.consent.revocation_requested',
        'tradefinance.consent.revoked',
        'tradefinance.application.submitted', 'tradefinance.application.decision_recorded',
        'tradefinance.application.disbursed', 'tradefinance.application.settled'
    )),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);

CREATE INDEX tf_outbox_unpublished_idx ON tf_outbox (created_at) WHERE published_at IS NULL;
