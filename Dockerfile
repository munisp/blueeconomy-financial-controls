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

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS intent-api
COPY --from=build /out/intent-api /intent-api
USER nonroot:nonroot
ENTRYPOINT ["/intent-api"]

FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS mojaloop-adapter
COPY --from=build /out/mojaloop-adapter /mojaloop-adapter
USER nonroot:nonroot
ENTRYPOINT ["/mojaloop-adapter"]
