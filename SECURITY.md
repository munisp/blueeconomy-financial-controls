# Security Posture — blueeconomy-financial-controls

Phase 11 security audit (branch `phase11/security`).

## Controls verified
- **Secrets**: working-tree scan (token/key/credential patterns) — clean; no secrets in repo.
- **AuthN/Z**: the only HTTP surface is the Mojaloop adapter callback; every request requires a verified FSPIOP signature plus FSPIOP-Source/Destination allowlist matching (fail-closed). Other binaries are daemons/CLIs with no listener.
- **Injection**: data access is parameterized (pgx); no string-built SQL found.
- **RLS**: the schema (financial_intents, outbox, mojaloop callbacks) is single-tenant national-ledger data with no tenant_id dimension — tenant RLS is not applicable; documented here to record the review.
- **Dependencies**: go.mod pins current versions; govulncheck unavailable offline (documented residual).

## Fixes this phase
- Restored `internal/orchestration/orchestrator.go` (missing from the working tree; build breakage).

## Residuals
- Run `govulncheck` in CI with network access.
- Live TigerBeetle integration tests (`liveintegration` tag) require the local stack.
