-- W-FEAT-4: statutory tariff engine for Nigerian maritime revenue.
-- Rate tables are DATA: versioned, effective-dated, maker/checker-approved
-- rows (never hardcoded constants). Exemptions are first-class,
-- machine-checkable rules — the Jan-2026 Supreme Court NLNG ruling
-- (>US$150m refund) proved discretionary exemptions are where the money
-- leaks. All money is integer minor units of the row currency.
--
-- Rates below are seeded from the engagement's verified research
-- (research/cruise_tanker_revenue_dim02.md). Rows whose research basis is
-- ambiguous carry provisional = TRUE with the ambiguity documented in the
-- statutory reference; nothing is guessed silently.

-- ---------------------------------------------------------------------------
-- Rate tables (versioned, effective-dated, maker/checker).
-- band_logic:
--   PER_GRT_BAND            amount = vessel GRT x rate_minor_per_unit, band
--                           selected by [band_floor, band_ceiling) on GRT
--   PERCENT_GROSS_FREIGHT   amount = gross freight x rate_bps / 10000 (half-up)
-- Applies-to filtering (which assessments an instrument touches) is engine
-- logic; the rate row carries only the statutory rate and its window.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tariff_rates (
    rate_id              TEXT PRIMARY KEY,
    instrument           TEXT NOT NULL CHECK (instrument IN (
                             'NPA_SHIP_DUES',
                             'NPA_LEVY_ACT',
                             'SEA_PROTECTION_LEVY_2012',
                             'CABOTAGE_SURCHARGE',
                             'NIMASA_LNG_CARRIER_DUE',
                             'NIWA_INLAND_CHARGE')),
    agency               TEXT NOT NULL CHECK (agency IN ('NPA', 'NIMASA', 'NIWA', 'FMMBE')),
    band_logic           TEXT NOT NULL CHECK (band_logic IN ('PER_GRT_BAND', 'PERCENT_GROSS_FREIGHT')),
    currency             TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    rate_minor_per_unit  BIGINT CHECK (rate_minor_per_unit IS NULL OR rate_minor_per_unit > 0),
    rate_bps             BIGINT CHECK (rate_bps IS NULL OR rate_bps > 0),
    band_floor           BIGINT NOT NULL DEFAULT 0 CHECK (band_floor >= 0),
    band_ceiling         BIGINT,
    statutory_reference  TEXT NOT NULL,
    provisional          BOOLEAN NOT NULL DEFAULT FALSE,
    effective_from       DATE NOT NULL,
    effective_to         DATE,                 -- NULL = open-ended
    state                TEXT NOT NULL CHECK (state IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    maker                TEXT NOT NULL,
    checker              TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (checker IS NULL OR checker <> maker),
    CHECK (band_ceiling IS NULL OR band_ceiling > band_floor),
    CHECK ((band_logic = 'PER_GRT_BAND' AND rate_minor_per_unit IS NOT NULL AND rate_bps IS NULL)
        OR (band_logic = 'PERCENT_GROSS_FREIGHT' AND rate_bps IS NOT NULL AND rate_minor_per_unit IS NULL))
);

-- One ACTIVE row per (instrument, band_floor) overlapping window is a
-- service-level invariant (enforced by the store in one transaction).

-- ---------------------------------------------------------------------------
-- Exemptions as first-class, machine-checkable rules. match_kind:
--   ENTITY          entity_ref of the vessel's owner/charterer (NLNG pattern)
--   CARGO_CATEGORY  cargo category (e.g. LNG_EXPORT)
--   VOYAGE_FLAG     a flag on the voyage declaration
--   CABOTAGE_TRADE  any cabotage voyage
-- Every applied exemption emits a tariff_exemption_audit row AND an outbox
-- event: who/what/why/statutory basis.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tariff_exemptions (
    exemption_id         TEXT PRIMARY KEY,
    instrument           TEXT NOT NULL,
    match_kind           TEXT NOT NULL CHECK (match_kind IN ('ENTITY', 'CARGO_CATEGORY', 'VOYAGE_FLAG', 'CABOTAGE_TRADE')),
    match_value          TEXT NOT NULL,
    statutory_basis      TEXT NOT NULL,
    evidence_requirement TEXT NOT NULL,
    effective_from       DATE NOT NULL,
    effective_to         DATE,
    state                TEXT NOT NULL CHECK (state IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    maker                TEXT NOT NULL,
    checker              TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (checker IS NULL OR checker <> maker)
);

-- ---------------------------------------------------------------------------
-- Assessments: immutable, deterministic, replay-safe.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tariff_assessments (
    assessment_id        TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL UNIQUE,
    request_hash         TEXT NOT NULL,        -- sha256 of the canonical request
    request              JSONB NOT NULL,
    as_of                DATE NOT NULL,
    total_usd_minor      BIGINT NOT NULL DEFAULT 0 CHECK (total_usd_minor >= 0),
    total_ngn_minor      BIGINT NOT NULL DEFAULT 0 CHECK (total_ngn_minor >= 0),
    requester            TEXT NOT NULL,        -- verified token subject
    correlation_id       TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tariff_assessment_lines (
    assessment_id        TEXT NOT NULL REFERENCES tariff_assessments (assessment_id),
    line_no              INTEGER NOT NULL,
    instrument           TEXT NOT NULL,
    agency               TEXT NOT NULL,
    applicability        TEXT NOT NULL CHECK (applicability IN ('CHARGED', 'EXEMPT', 'NOT_APPLICABLE', 'UNRATED')),
    basis                TEXT NOT NULL,        -- machine-readable basis summary
    statutory_reference  TEXT NOT NULL DEFAULT '',
    rate_description     TEXT NOT NULL DEFAULT '',
    amount_minor         BIGINT NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    currency             TEXT NOT NULL DEFAULT 'USD',
    exemption_id         TEXT,
    provisional          BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (assessment_id, line_no)
);

-- Exemption audit: one row per applied exemption, forever.
CREATE TABLE IF NOT EXISTS tariff_exemption_audit (
    audit_id             TEXT PRIMARY KEY,
    assessment_id        TEXT NOT NULL REFERENCES tariff_assessments (assessment_id),
    exemption_id         TEXT NOT NULL REFERENCES tariff_exemptions (exemption_id),
    instrument           TEXT NOT NULL,
    match_kind           TEXT NOT NULL,
    match_value          TEXT NOT NULL,
    statutory_basis      TEXT NOT NULL,
    evidence_requirement TEXT NOT NULL,
    requester            TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Tariff outbox (repo convention: transactional outbox drained by the
-- publisher; Kafka envelope registration lives in internal/outbox, which is
-- outside this change's isolation boundary — the table shape matches the
-- existing drains exactly so registration is a one-line mapping later).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tariff_outbox (
    event_id      UUID PRIMARY KEY,
    subject_id    TEXT NOT NULL,
    event_type    TEXT NOT NULL CHECK (event_type IN (
                      'tariff.assessment.created',
                      'tariff.exemption.applied',
                      'tariff.rate.created',
                      'tariff.rate.activated',
                      'tariff.exemption.created',
                      'tariff.exemption.activated')),
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    published_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS tariff_outbox_unpublished_idx ON tariff_outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS tariff_rates_instrument_window_idx ON tariff_rates (instrument, state, effective_from);
CREATE INDEX IF NOT EXISTS tariff_exemptions_lookup_idx ON tariff_exemptions (instrument, state, match_kind, match_value);

-- ---------------------------------------------------------------------------
-- Seed rates from the verified research (maker/checker: seed pair).
-- ---------------------------------------------------------------------------

-- NPA ship dues: US$1.47 per GRT, unbanded (single band from GRT 0).
INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency,
    rate_minor_per_unit, band_floor, band_ceiling, statutory_reference, provisional,
    effective_from, state, maker, checker)
VALUES ('rate-npa-ship-dues-2007', 'NPA_SHIP_DUES', 'NPA', 'PER_GRT_BAND', 'USD',
    147, 0, NULL, 'NPA Act ship dues — US$1.47/GRT on international port calls (verified research)', FALSE,
    DATE '2007-01-01', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;

-- NIMASA Act s.15 sea protection levy: 3% of gross freight. Superseded by
-- the 2012 Sea Protection Levy instrument — closed effective window so
-- current assessments do not double-charge.
INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency,
    rate_bps, statutory_reference, provisional, effective_from, effective_to, state, maker, checker)
VALUES ('rate-npalevy-s15-2007', 'NPA_LEVY_ACT', 'NIMASA', 'PERCENT_GROSS_FREIGHT', 'USD',
    300, 'NIMASA Act 2007 s.15 — sea protection levy 3% of gross freight (verified research)', FALSE,
    DATE '2007-01-01', DATE '2012-12-31', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;

-- Sea Protection Levy 2012: ~0.2% of gross freight. The research states the
-- 2012 levy at "about 0.2%" — encoded PROVISIONAL pending the gazetted band
-- schedule (documented, not guessed).
INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency,
    rate_bps, statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('rate-spl-2012', 'SEA_PROTECTION_LEVY_2012', 'NIMASA', 'PERCENT_GROSS_FREIGHT', 'USD',
    20, 'Sea Protection Levy 2012 — ~0.2% of gross freight per research; PROVISIONAL pending gazetted band schedule', TRUE,
    DATE '2013-01-01', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;

-- Cabotage Act 2003: 2% surcharge on gross freight of cabotage voyages,
-- accruing to the CVFF (FMMBE custody).
INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency,
    rate_bps, statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('rate-cabotage-2004', 'CABOTAGE_SURCHARGE', 'FMMBE', 'PERCENT_GROSS_FREIGHT', 'USD',
    200, 'Cabotage Act 2003 — 2% surcharge on gross freight of coastal trade, accruing to CVFF (verified research)', FALSE,
    DATE '2004-01-01', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;

-- NIMASA LNG carrier due: US$0.30/GRT on LNG carriers calling Nigerian ports
-- (the due at the centre of the NLNG litigation).
INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency,
    rate_minor_per_unit, band_floor, band_ceiling, statutory_reference, provisional,
    effective_from, state, maker, checker)
VALUES ('rate-nimasa-lng-due-2008', 'NIMASA_LNG_CARRIER_DUE', 'NIMASA', 'PER_GRT_BAND', 'USD',
    30, 0, NULL, 'NIMASA Act 2007 — US$0.30/GRT due on LNG carriers (verified research)', FALSE,
    DATE '2008-01-01', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;

-- NIWA s.28 inland-waterways charge: the research identifies the instrument
-- and agency but no rate. No seed row: assessments on inland voyages emit an
-- UNRATED line (visible gap) rather than an invented number.

-- NLNG exemption: NIMASA Act 2007 s.19 exempts NLNG vessels from the
-- US$0.30/GRT LNG carrier due — upheld by the Supreme Court, Jan 2026
-- (>US$150m refund). Encoded as an ENTITY rule requiring evidence.
INSERT INTO tariff_exemptions (exemption_id, instrument, match_kind, match_value,
    statutory_basis, evidence_requirement, effective_from, state, maker, checker)
VALUES ('exemption-nlng-lng-due', 'NIMASA_LNG_CARRIER_DUE', 'ENTITY', 'NLNG',
    'NIMASA Act 2007 s.19 — NLNG vessel exemption from the LNG carrier due, affirmed by Supreme Court ruling (Jan 2026)',
    'NLNG vessel registration certificate evidencing NLNG ownership for the assessed voyage',
    DATE '2008-01-01', 'ACTIVE', 'seed-tariff', 'seed-tariff-review') ON CONFLICT DO NOTHING;
