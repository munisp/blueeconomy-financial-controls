# syntax=docker/dockerfile:1

# Build stage. The TigerBeetle client links its prebuilt static library via
# CGO, so the toolchain needs gcc and CGO must stay enabled; the runtime
# images are distroless with glibc (base-debian12, digest-pinned), running
# as non-root. Matches the proven blueeconomy-ferry-ticketing pattern.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# Per-command runtime targets (distroless glibc base, non-root).
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS cvff-worker
COPY --from=build /out/cvff-worker /cvff-worker
USER nonroot:nonroot
ENTRYPOINT ["/cvff-worker"]

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS outbox-publisher
COPY --from=build /out/outbox-publisher /outbox-publisher
USER nonroot:nonroot
ENTRYPOINT ["/outbox-publisher"]

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS financial-orchestrator
COPY --from=build /out/financial-orchestrator /financial-orchestrator
USER nonroot:nonroot
ENTRYPOINT ["/financial-orchestrator"]

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS financial-reconcile
COPY --from=build /out/financial-reconcile /financial-reconcile
USER nonroot:nonroot
ENTRYPOINT ["/financial-reconcile"]

# intent-api gates money routes with the embedded PBAC pack, baked into the
# image (INTENT_API_POLICY_DIR=/etc/blueeconomy/policies).
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS intent-api
COPY --from=build /out/intent-api /intent-api
COPY policies/ /etc/blueeconomy/policies/
USER nonroot:nonroot
ENTRYPOINT ["/intent-api"]

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS mojaloop-adapter
COPY --from=build /out/mojaloop-adapter /mojaloop-adapter
USER nonroot:nonroot
ENTRYPOINT ["/mojaloop-adapter"]

# cvff-api serves /v1/cvff/applications* for the beneficiary portal. The PBAC
# policy pack is baked into the image so CVFF_API_POLICY_DIR is self-contained
# (the chart sets it to /etc/blueeconomy/policies).
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS cvff-api
COPY --from=build /out/cvff-api /cvff-api
COPY policies/ /etc/blueeconomy/policies/
USER nonroot:nonroot
ENTRYPOINT ["/cvff-api"]

# declaration-scorer serves POST /v1/risk-scores for port-interoperability.
# The versioned rules ship as config data baked into the image; an
# operator-supplied DECLARATION_SCORER_RULES_PATH overrides.
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS declaration-scorer
COPY --from=build /out/declaration-scorer /declaration-scorer
COPY config/declaration-scorer-rules.json /etc/declaration-scorer/rules.json
USER nonroot:nonroot
ENTRYPOINT ["/declaration-scorer"]

# stamps-intake consumes the tax-stamps excise stamp lifecycle events from
# the stamps.* topics (JWS-verified, idempotent landing).
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS stamps-intake
COPY --from=build /out/stamps-intake /stamps-intake
USER nonroot:nonroot
ENTRYPOINT ["/stamps-intake"]
