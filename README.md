# Blue Economy Financial Controls

This repository contains the initial financial-control integration boundary for the platform. It uses the **official TigerBeetle Go client** to perform a strictly read-only account lookup against an explicitly configured ledger cluster. It cannot create TigerBeetle accounts, transfers, two-phase transfers, Mojaloop quotes, Mojaloop transfers, participant records or payment instructions.

## Implemented component

`tigerbeetle-verifier` requires all of the following at runtime:

| Input | Purpose |
|---|---|
| `TIGERBEETLE_CLUSTER_ID_HEX` | The actual approved TigerBeetle cluster identifier as a hexadecimal Uint128. |
| `TIGERBEETLE_REPLICA_ADDRESSES` | Comma-separated approved replica addresses for the configured cluster. |
| `--account-id-hex` | An approved account identifier to look up. |
| `--evidence` | An approved non-secret evidence-file destination. |

The command writes a redacted result that hashes the account reference and records only whether the account was found plus its ledger, code, timestamp and history/closed flags. It does not write credentials, account balances, raw account identifiers or transfers.

## Financial and Mojaloop gate

A compiled TigerBeetle client is **not** a payment-platform implementation. The Ministry must supply and approve the genuine non-production TigerBeetle cluster, account/ledger/code model, participant/settlement design, separation-of-duties controls, reconciliation policy, key/secret delivery, Mojaloop switch/participant endpoint, certificates, test participants, payment rulebook and financial-control sign-off. The verifier must be run against that environment and its evidence reviewed before implementation of any write-capable ledger or Mojaloop payment capability.

The production implementation will remain disabled until those real dependencies and approvals exist. This repository intentionally contains no placeholder participant, currency, amount, account, transfer, payment or settlement data.
