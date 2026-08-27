# syntax=docker/dockerfile:1

# Build stage: compile every command statically.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# Per-command runtime targets (distroless, non-root).
FROM gcr.io/distroless/static-debian12:nonroot AS cvff-worker
COPY --from=build /out/cvff-worker /cvff-worker
USER nonroot:nonroot
ENTRYPOINT ["/cvff-worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS outbox-publisher
COPY --from=build /out/outbox-publisher /outbox-publisher
USER nonroot:nonroot
ENTRYPOINT ["/outbox-publisher"]

FROM gcr.io/distroless/static-debian12:nonroot AS financial-orchestrator
COPY --from=build /out/financial-orchestrator /financial-orchestrator
USER nonroot:nonroot
ENTRYPOINT ["/financial-orchestrator"]

FROM gcr.io/distroless/static-debian12:nonroot AS financial-reconcile
COPY --from=build /out/financial-reconcile /financial-reconcile
USER nonroot:nonroot
ENTRYPOINT ["/financial-reconcile"]

FROM gcr.io/distroless/static-debian12:nonroot AS intent-api
COPY --from=build /out/intent-api /intent-api
USER nonroot:nonroot
ENTRYPOINT ["/intent-api"]

FROM gcr.io/distroless/static-debian12:nonroot AS mojaloop-adapter
COPY --from=build /out/mojaloop-adapter /mojaloop-adapter
USER nonroot:nonroot
ENTRYPOINT ["/mojaloop-adapter"]
