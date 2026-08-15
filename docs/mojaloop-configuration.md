# Mojaloop Configuration and Onboarding

## Scope

The local callback verification path uses a real PostgreSQL 16 container and does not contact a switch, participant or external endpoint. Run it with:

```bash
cd /home/ubuntu/blueeconomy-financial-controls
bash scripts/verify-mojaloop-local.sh
```

The script applies `db/migrations/0001_financial_intents.sql` and `db/migrations/0002_mojaloop_callbacks.sql`, then runs the tagged PostgreSQL callback test. It does not require, and must not be supplied with, production credentials or fabricated partner URLs.

The adapter configuration is for the future participant runtime. It is intentionally fail-closed and is not consumed by the local callback test.

## Environment variables

| Variable | Required for participant runtime | Meaning and validation |
|---|---:|---|
| `MOJALOOP_FSPIOP_BASE_URL` | Yes | Approved HTTPS switch/FSPIOP base URL; no userinfo, no HTTP. |
| `MOJALOOP_CALLBACK_BASE_URL` | Yes | Approved HTTPS callback ingress base URL registered with the partner/switch. |
| `MOJALOOP_FSPIOP_SOURCE` | Yes | Ministry-approved source participant identifier. |
| `MOJALOOP_FSPIOP_DESTINATION` | Yes | Approved destination participant/switch identifier for the profile. |
| `MOJALOOP_SIGNING_KEY_FILE` | Yes | Runtime-mounted private-key file; never committed or placed in an evidence attachment. |
| `MOJALOOP_SIGNING_KID` | Yes | Registered key identifier used for rotation and callback trust. |
| `MOJALOOP_SIGNATURE_ALGORITHM` | Yes | `RS256`, `RS384` or `RS512`; the selected scheme allowlist must be approved. The local example defaults to `RS256`, but it does not create a key or endpoint. |
| `MOJALOOP_CA_BUNDLE_FILE` | Yes | Runtime-mounted CA bundle for the approved participant/switch PKI. |
| `MOJALOOP_REQUEST_TIMEOUT` | No | Defaults to `10s`; accepted range is 1 second through 2 minutes. |
| `MOJALOOP_LISTEN_ADDR` | Target runtime | Explicit callback listener address; there is no implicit default. |
| `MOJALOOP_TLS_CERT_FILE` | Target runtime | Runtime-mounted HTTPS server certificate. |
| `MOJALOOP_TLS_KEY_FILE` | Target runtime | Runtime-mounted HTTPS private key; never committed. |
| `DATABASE_URL` | Target runtime | PostgreSQL/CNPG connection with TLS and least-privilege role. Never commit credentials. |
| `MOJALOOP_MIGRATION_PATH` | Target runtime | Path to `db/migrations/0002_mojaloop_callbacks.sql` or the controlled migration artifact. |

The committed `config/mojaloop.env.example` contains empty external values by design. It is a configuration shape, not a deployable environment.

## Local preparation

First install the repository-pinned Go toolchain and dependencies, then run `go test -race ./internal/mojaloop`, `go vet ./...`, `govulncheck ./...` and `bash scripts/verify-mojaloop-local.sh`. The real test proves signature and body binding, unsigned-request rejection, callback reserve/commit handling, exact replay, terminal replay, identity conflict and state regression rejection against PostgreSQL. It does not prove switch or participant conformance.

## Ministry target preparation

The Ministry environment owner must first create an environment-registry record containing the switch/FSPIOP base URL, callback ingress URL, participant IDs, certificate fingerprints, JWK/key identifiers, CA bundle reference, OAuth/token policy if required by the selected deployment profile, approved scheme/rulebook version, test window, data classification, rollback contact and expiry. The values are then injected through the target secret manager and workload identity. They must not be written into Git, shell history, test fixtures or evidence files.

The executable `cmd/mojaloop-adapter` bootstrap requires all target variables above, parses an RSA PKCS#1 or PKCS#8 signing key, validates the CA bundle, applies the committed callback migration, binds the callback handler under `/transfers/` and starts an HTTPS server with bounded read/write/header/idle timeouts. It refuses to start if endpoint, participant, key, CA, database, migration, listener or TLS values are missing. The bootstrap has not been pointed at a Ministry or partner environment.

The target deployment must provision PostgreSQL migrations, network egress only to the approved FSPIOP destination, ingress only from the approved callback sources, TLS validation, bounded request size/timeouts, redacted structured audit logging, metrics for accepted/replayed/conflicting callbacks and an operator runbook for ambiguous transfer recovery. The runtime must query authoritative transfer state before deciding whether a TigerBeetle pending transfer is posted or voided.

## Partner onboarding sequence

The partner must exchange a profile document before credentials are issued. It must specify supported FSPIOP version, paths, participant identifiers, callback source/destination values, supported signature algorithms, JWK or certificate distribution, rotation overlap and revocation method, JWE requirements if any, error model, timeout/recovery semantics, rate limits, test accounts, test currencies and statement/reconciliation format.

After the profile is approved, the Ministry installs sandbox credentials into the target secret manager, records fingerprints and expiry, runs the signature and callback conformance suite, then runs quote/transfer submission, asynchronous callback, duplicate, timeout, `GET` recovery, ambiguous response, post/void and reconciliation tests. A partner sandbox pass does not authorize live funds; regulated financial approval and operational acceptance remain separate release gates.

## Configuration failure rules

The runtime must refuse to start or send a request when the endpoint is not HTTPS, a participant ID is missing, the key ID or CA bundle is absent, the algorithm is outside the approved allowlist, the timeout is outside the bounded range, or a callback source/destination does not match the registered profile. A missing or expired key must result in a stop, not fallback to an unsigned request. A failed or ambiguous transfer response must result in durable investigation/recovery state, not blind retry.

## Related source

- `internal/mojaloop/fspiop.go`: signature creation and verification.
- `internal/mojaloop/client.go`: HTTPS-only signed outbound request builder.
- `internal/mojaloop/http.go`: signed callback HTTP handler.
- `internal/mojaloop/store.go`: PostgreSQL callback state machine.
- `scripts/verify-mojaloop-local.sh`: authentic local callback integration.
- `config/mojaloop.env.example`: empty, no-secret configuration shape.
