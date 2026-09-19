# Published event topics

All events are signed platform envelopes (version 1.0, producer
`financial-controls`) built by `internal/outbox` and drained from the
transactional outbox by `cmd/outbox-publisher`. The publishable topic set is
logged at startup (`outbox-publisher: publishable topics: ...`) and is
available programmatically via `outbox.Topics()`.

| Topic | Producer (outbox event types) | Consumer | Schema ref |
|---|---|---|---|
| `cvff.disbursement.v1` | cvff disbursement lifecycle (`financial_intent.*`, `cvff.*` — 12 event types) | reserved — no consumer yet (downstream reconciliation/audit consumers are scheduled) | internal/outbox/envelope.go |
| `finance.revenue.v1` | revenue-assurance chain (`revenue.*` — 11 event types) | reserved — no consumer yet | internal/outbox/envelope.go |
| `tradefinance.consent.v1` | WP-6 trade-finance consent rail (`tradefinance.consent.*`) | reserved — no consumer yet | internal/outbox/envelope.go |
| `tradefinance.application.v1` | WP-6 trade-finance application rail (`tradefinance.application.*`) | reserved — no consumer yet | internal/outbox/envelope.go |

## Consumed (inbound) topics — closed loops

| Topic | Consumer | Producer |
|---|---|---|
| `stamps.*` | internal/stampsintake (cmd/stamps-intake) | blueeconomy-tax-stamps |
| `finance.revenue-assessments.v1` | internal/revenueintake (cmd/revenue-intake) | blueeconomy-port-interoperability |

"Reserved" means the signed-envelope production path is intentionally live
while the downstream consumer lands in a later phase. Do not remove
producers; update this table when a consumer is wired.
