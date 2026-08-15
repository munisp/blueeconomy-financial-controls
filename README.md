# Blue Economy Financial Controls

This repository contains the real TigerBeetle ledger integration boundary and durable financial-intent control for the platform. It uses the official TigerBeetle Go client and supports explicit account creation plus two-phase pending, post and void transfer operations against an approved configured cluster. It also persists financial intent, maker/checker approval, reservation-request state, ambiguous outcomes and reconciliation-required evidence in PostgreSQL. It does **not** implement Mojaloop participant integration, payment instructions, settlement, reconciliation with an external payment provider or live-fund authorization.

## Commands

`tigerbeetle-verifier` performs a redacted account lookup and writes non-secret evidence. It requires `TIGERBEETLE_CLUSTER_ID_HEX`, `TIGERBEETLE_REPLICA_ADDRESSES`, `--account-id-hex` and `--evidence`.

`tigerbeetle-ledger` performs a real write operation only when an operator supplies an approved TigerBeetle environment and explicit identifiers. It requires `TIGERBEETLE_CLUSTER_ID_HEX`, `TIGERBEETLE_REPLICA_ADDRESSES`, `TIGERBEETLE_LEDGER` and `TIGERBEETLE_CODE`.

| Operation | Required flags | TigerBeetle operation |
|---|---|---|
| `account` | `--id-hex`, optional `--history` | Creates an account idempotently when the existing account has the same immutable attributes. |
| `reserve` | `--id-hex`, `--debit-account-id-hex`, `--credit-account-id-hex`, `--amount`, `--timeout` | Creates a pending transfer that reserves funds until posted, voided or timed out according to the approved ledger policy. |
| `post` | `--id-hex`, `--pending-transfer-id-hex` | Posts an existing pending transfer using a distinct transfer identifier. |
| `void` | `--id-hex`, `--pending-transfer-id-hex` | Voids an existing pending transfer using a distinct transfer identifier. |

The command has no default cluster, replica, ledger, code, account, amount or partner endpoint. It fails when any required environment value or identifier is absent, malformed or outside the TigerBeetle protocol width. Every client result is checked for the official `Created` status; a non-success result exits non-zero.

The tagged financial-intent integration can be run with `scripts/verify-intent-local.sh`; it uses real PostgreSQL 16.4 and verifies exact external-reference replay, conflicting immutable-field rejection, distinct maker/checker approval, reservation-request state and three outbox records. The store does not call TigerBeetle automatically, so no live money movement occurs as a side effect of this local test.

## Financial and Mojaloop gate

A write-capable TigerBeetle client is **not by itself a payment-platform implementation**. Before any financial flow can be enabled, the Ministry must approve the genuine non-production TigerBeetle cluster, account/ledger/code model, participant and settlement design, separation-of-duties controls, reconciliation policy, key/secret delivery, Mojaloop switch and participant endpoints, certificates, test participants, payment rulebook and financial-control sign-off. The operation must then pass two-phase transfer, duplicate, timeout, post, void, recovery and independent reconciliation tests.

The repository contains no placeholder participant, currency, amount, account, transfer, payment or settlement data. Production execution remains disabled until the real dependencies, regulated operating approvals and target-environment evidence exist.
