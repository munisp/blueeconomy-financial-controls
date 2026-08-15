# Blue Economy Financial Controls

This repository contains the real TigerBeetle ledger integration boundary, durable financial-intent control and protocol-grounded Mojaloop FSPIOP adapter boundary for the platform. It uses the official TigerBeetle Go client and supports explicit account creation plus two-phase pending, post and void transfer operations against an approved configured cluster. It persists financial intent, maker/checker approval, reservation-request state, ambiguous outcomes and reconciliation-required evidence in PostgreSQL. The `financial-reconcile` command reads posted/voided intents from that PostgreSQL store and compares them with an explicitly supplied external statement, producing hashed findings and failing closed when discrepancies exist. The Mojaloop package builds signed FSPIOP requests for quotes/transfers/recovery and durably validates signed transfer callbacks with exact replay and state-transition controls. It does **not** contain a Ministry-approved participant endpoint, switch connection, settlement authority or live-fund authorization.

## Commands

`tigerbeetle-verifier` performs a redacted account lookup and writes non-secret evidence. It requires `TIGERBEETLE_CLUSTER_ID_HEX`, `TIGERBEETLE_REPLICA_ADDRESSES`, `--account-id-hex` and `--evidence`.

`tigerbeetle-ledger` performs a real write operation only when an operator supplies an approved TigerBeetle environment and explicit identifiers. It requires `TIGERBEETLE_CLUSTER_ID_HEX`, `TIGERBEETLE_REPLICA_ADDRESSES`, `TIGERBEETLE_LEDGER` and `TIGERBEETLE_CODE`.

The `internal/mojaloop` package provides the protocol boundary. It supports standard FSPIOP-Signature creation and verification with RS256, RS384 and RS512; binds signatures to the full payload, HTTP method, request URI, source and destination; builds HTTPS-only signed requests for `POST /quotes/{ID}`, `POST /transfers` and `GET /transfers/{ID}`; and exposes a callback handler for `PUT /transfers/{ID}` with durable PostgreSQL state. Callback handling supports RESERVED → COMMITTED/ABORTED, exact idempotent replay, immutable identity checks and fail-closed regression/conflict responses. `scripts/verify-mojaloop-local.sh` runs the callback state test against real PostgreSQL 16.

| Operation | Required flags | TigerBeetle operation |
|---|---|---|
| `account` | `--id-hex`, optional `--history` | Creates an account idempotently when the existing account has the same immutable attributes. |
| `reserve` | `--id-hex`, `--debit-account-id-hex`, `--credit-account-id-hex`, `--amount`, `--timeout` | Creates a pending transfer that reserves funds until posted, voided or timed out according to the approved ledger policy. |
| `post` | `--id-hex`, `--pending-transfer-id-hex` | Posts an existing pending transfer using a distinct transfer identifier. |
| `void` | `--id-hex`, `--pending-transfer-id-hex` | Voids an existing pending transfer using a distinct transfer identifier. |

The command has no default cluster, replica, ledger, code, account, amount or partner endpoint. It fails when any required environment value or identifier is absent, malformed or outside the TigerBeetle protocol width. Every client result is checked for the official `Created` status; a non-success result exits non-zero.

The tagged financial-intent integration can be run with `scripts/verify-intent-local.sh`; it uses real PostgreSQL 16.4 and verifies exact external-reference replay, conflicting immutable-field rejection, distinct maker/checker approval, reservation-request and posted states, reconciliation-intent listing and five outbox records. `scripts/verify-mojaloop-local.sh` applies the committed callback migration and verifies signed-boundary logic, reserve/commit transitions, exact replays, terminal replays and regression rejection against real PostgreSQL. `financial-reconcile` requires `DATABASE_URL`, `STATEMENT_PATH` and `REPORT_PATH`; it reads only posted/voided intents, hashes the supplied statement bytes and returns non-zero when findings exist. The store does not call TigerBeetle or a Mojaloop partner automatically, so no live money movement occurs as a side effect of these local tests.

## Financial and Mojaloop gate

A write-capable TigerBeetle client is **not by itself a payment-platform implementation**. Before any financial flow can be enabled, the Ministry must approve the genuine non-production TigerBeetle cluster, account/ledger/code model, participant and settlement design, separation-of-duties controls, reconciliation policy, key/secret delivery, Mojaloop switch and participant endpoints, certificates, test participants, payment rulebook and financial-control sign-off. The operation must then pass two-phase transfer, duplicate, timeout, post, void, recovery and independent reconciliation tests.

The repository contains no placeholder participant, currency, amount, account, transfer, payment or settlement data. Production execution remains disabled until the real dependencies, regulated operating approvals and target-environment evidence exist.
