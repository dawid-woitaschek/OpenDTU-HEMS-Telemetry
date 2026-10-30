# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
# Optional CA bundle for builds behind a TLS-inspecting development proxy.
RUN --mount=type=secret,id=proxy_ca \
    --mount=type=cache,id=hems-telemetry-gomod,target=/go/pkg/mod \
    if [ -f /run/secrets/proxy_ca ]; then \
      SSL_CERT_FILE=/run/secrets/proxy_ca go mod download; \
    else go mod download; fi
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,id=hems-telemetry-gomod,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /collector ./cmd/server
RUN mkdir /data && chown 65532:65532 /data && chmod 0700 /data

FROM scratch
COPY --from=builder /collector /collector
COPY --from=builder --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/collector", "healthcheck"]
ENTRYPOINT ["/collector"]
