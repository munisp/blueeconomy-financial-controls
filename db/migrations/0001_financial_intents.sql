CREATE TABLE financial_intents (
    intent_id TEXT PRIMARY KEY,
    external_ref TEXT NOT NULL UNIQUE,
    debit_account_id TEXT NOT NULL,
    credit_account_id TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    ledger INTEGER NOT NULL CHECK (ledger > 0),
    code INTEGER NOT NULL CHECK (code > 0),
    currency TEXT NOT NULL,
    maker TEXT NOT NULL,
    checker TEXT,
    state TEXT NOT NULL CHECK (state IN ('DRAFT', 'APPROVED', 'RESERVATION_REQUESTED', 'RESERVED', 'POSTED', 'VOIDED', 'AMBIGUOUS', 'RECONCILIATION_REQUIRED')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    CHECK (debit_account_id <> credit_account_id),
    CHECK (checker IS NULL OR checker <> maker)
);

CREATE TABLE financial_intent_outbox (
    event_id UUID PRIMARY KEY,
    intent_id TEXT NOT NULL REFERENCES financial_intents(intent_id),
    event_type TEXT NOT NULL CHECK (event_type IN ('financial_intent.created', 'financial_intent.approved', 'financial_intent.state_changed')),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);

CREATE INDEX financial_intent_outbox_unpublished_idx ON financial_intent_outbox (created_at) WHERE published_at IS NULL;
