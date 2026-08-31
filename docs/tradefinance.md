# WP-6 Trade-Finance Rail (CamelONE-style multi-bank trade finance)

Modelled on Singapore's NTP Trade Finance Compliance (TFC): with trader
consent, customs declaration/payment evidence is shared with the trader's
chosen bank so financing compliance is fast; a multi-bank product workflow
offers standardized products.

## Surfaces

- `internal/tradefinance` — consent registry, bank-facing data-sharing API,
  product workflow, disbursement ledger wiring.
- `cmd/tradefinance-api` — service entrypoint (fail-closed env wiring).
- `db/migrations/0011_tradefinance.sql` — DB-enforced invariants.
- `policies/tradefinance.rego` — OPA consent-scope policy (deny-by-default).

## Consent registry

- Trader grants one bank access to scoped datasets:
  `DECLARATION_DIGESTS`, `DUTY_PAYMENT_HISTORY`, `TAX_STAMP_STATUS`.
- Time-boxed (max 366 days), revocable, maker-checker on grant/revoke/reject.
- Activation seals the consent into envelope v1.0 (FHIR R4 Bundle + JWS
  EdDSA over JCS-canonical payload); the bundle and JWS are persisted.
- Every state change appends a hash-chained, DB-immutable audit entry
  (`tf_consent_audit`); `VerifyAuditChain` revalidates the chain.
- One open consent per (trader, bank) — DB partial unique index.

## Bank-facing data-sharing API

`GET /v1/tradefinance/bank/datasets/{traderID}/{scope}`

- Bank authenticates by mTLS client-cert CN or OIDC bearer subject, bound to
  a bank id in `TRADEFINANCE_BANK_REGISTRY_JSON` (env-only; empty registry
  refuses boot).
- Response is an envelope v1.0 signed bundle carrying ONLY the requested
  scope's digest refs. Unconsented scope, foreign bank, unknown trader,
  expired or pending consent all return 403 with no consent oracle; bad
  credentials return 401.

## Product workflow

Starter catalogue (4 of the CamelONE 12): `IMPORT_LC_FACILITATION`,
`EXPORT_PRESHIPMENT_FINANCE`, `INVOICE_RECEIVABLES_FINANCE`,
`DUTY_DEFERRAL_GUARANTEE`.

Lifecycle: `APPLICATION → KYC_CONSENT_CHECK → BANK_REVIEW →
REGULATORY_CLEARANCE → APPROVED → DISBURSEMENT_PENDING → DISBURSED →
SETTLED` (any rejection → `DECLINED`, terminal).

- Reuses the CVFF four-party approval machinery pattern: role-bound
  decisions, strict separation of duties (DB unique constraint), immutable
  approvals (DB trigger), optimistic concurrency.
- `KYC_CONSENT_CHECK` approval is gated on an ACTIVE, unexpired consent for
  (trader, bank) — no consent, no financing (TFC pattern).
- Disbursement ledger: TigerBeetle double-entry with deterministic
  (idempotent) account/transfer ids — pending reservation at approval, post
  at disbursement, settlement post at repayment; legs persisted in
  `tf_disbursement_legs` (forward-only stamping).

## Events

Transactional outbox `tf_outbox` drained by the existing outbox publisher,
envelope-signed, to `tradefinance.consent.v1` /
`tradefinance.application.v1` (mapping in `internal/outbox/envelope.go`).

## Configuration (all required; missing = no boot)

| Env | Purpose |
|---|---|
| `TRADEFINANCE_API_LISTEN_ADDR` | bind address |
| `DATABASE_URL` | PostgreSQL |
| `OUTBOX_SIGNING_PRIVATE_KEY` / `OUTBOX_SIGNING_KEY_EPOCH` | Ed25519 envelope sealing key (env-only) |
| `TRADEFINANCE_BANK_REGISTRY_JSON` | bank registry (OIDC subjects / mTLS CNs) |
| `TRADEFINANCE_API_KEYCLOAK_ISSUER` / `..._JWKS_URL` / `..._JWT_AUDIENCE` | OIDC verification |
