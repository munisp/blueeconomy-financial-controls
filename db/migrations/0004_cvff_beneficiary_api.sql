-- Beneficiary-facing CVFF API intake: application detail fields, durable
-- state transitions and document metadata. All monetary values remain minor
-- units (kobo); every new write path is tenant-scoped by beneficiary_id.

-- Beneficiary-supplied intake detail. Nullable so applications seeded by
-- other intake paths (reconciliation fixtures, recovery tooling) stay valid;
-- the beneficiary API always writes canonical values.
ALTER TABLE cvff_applications
    ADD COLUMN vessel_name TEXT,
    ADD COLUMN imo_number TEXT,
    ADD COLUMN official_number TEXT,
    ADD COLUMN vessel_class TEXT,
    ADD COLUMN cabotage_route TEXT,
    ADD COLUMN business_name TEXT,
    ADD COLUMN business_rc_number TEXT,
    ADD COLUMN business_address TEXT;

ALTER TABLE cvff_applications
    ADD CONSTRAINT cvff_applications_vessel_class_check
        CHECK (vessel_class IS NULL OR vessel_class IN (
            'FISHING_TRAWLER', 'CARGO_COASTER', 'TUG', 'BARGE',
            'PASSENGER_FERRY', 'SUPPLY_VESSEL', 'CREW_BOAT', 'OTHER'
        )),
    ADD CONSTRAINT cvff_applications_cabotage_route_check
        CHECK (cabotage_route IS NULL OR cabotage_route IN (
            'LAGOS_PORT_HARCOURT', 'LAGOS_ONNE', 'LAGOS_WARRI', 'LAGOS_CALABAR',
            'PORT_HARCOURT_BONNY', 'WARRI_ESCRAVOS', 'INLAND_WATERWAYS', 'OTHER'
        )),
    ADD CONSTRAINT cvff_applications_imo_number_check
        CHECK (imo_number IS NULL OR imo_number ~ '^[0-9]{7}$');

-- Beneficiary ownership index: beneficiaries list only their own applications.
CREATE INDEX cvff_applications_beneficiary_idx ON cvff_applications (beneficiary_id, created_at DESC);

-- Durable record of every state-machine move. Decision moves carry the
-- deciding party; non-decision moves (underwriting start, audit commit,
-- reconciliation branch) carry the acting principal and event type.
CREATE TABLE cvff_transitions (
    transition_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES cvff_applications(application_id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    actor_role TEXT NOT NULL,
    actor_principal_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX cvff_transitions_application_idx ON cvff_transitions (application_id, created_at, transition_id);

-- Transition entries are immutable audit evidence.
CREATE FUNCTION cvff_transitions_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'cvff_transitions entries are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER cvff_transitions_no_update
    BEFORE UPDATE OR DELETE ON cvff_transitions
    FOR EACH ROW EXECUTE FUNCTION cvff_transitions_immutable();

-- Beneficiary-uploaded supporting documents. Bytes live in the configured
-- object-storage backend under a content-addressed key; this table holds the
-- metadata, integrity digest and idempotency binding.
CREATE TABLE cvff_documents (
    document_id UUID PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES cvff_applications(application_id),
    beneficiary_id TEXT NOT NULL,
    document_type TEXT NOT NULL CHECK (document_type IN (
        'VESSEL_REGISTRATION', 'CABOTAGE_LICENSE', 'BANK_DETAILS'
    )),
    file_name TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    sha256_hex TEXT NOT NULL CHECK (sha256_hex ~ '^[0-9a-f]{64}$'),
    storage_backend TEXT NOT NULL CHECK (storage_backend IN ('adls', 's3', 'local-gated')),
    storage_key TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (application_id, idempotency_key)
);

CREATE INDEX cvff_documents_application_idx ON cvff_documents (application_id, created_at, document_id);

-- Document metadata is immutable once recorded; a wrong upload is superseded
-- by a new document, never mutated in place.
CREATE FUNCTION cvff_documents_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'cvff_documents entries are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER cvff_documents_no_update
    BEFORE UPDATE OR DELETE ON cvff_documents
    FOR EACH ROW EXECUTE FUNCTION cvff_documents_immutable();
