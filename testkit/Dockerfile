# syntax=docker/dockerfile:1.7
# TestKit images. Base images are pinned in infra/compose/versions.env and
# passed as build args by `testkit up` (never "latest").
ARG GO_BUILD_IMAGE=golang:1.24.7-alpine3.21
ARG BASE_IMAGE=alpine:3.20.3
ARG DOCKER_CLI_IMAGE=docker:27.3.1-cli
ARG OTEL_COLLECTOR_IMAGE=otel/opentelemetry-collector-contrib:0.112.0

FROM ${GO_BUILD_IMAGE} AS build
# Optional extra CA (TESTKIT_BUILD_CA) for builds behind a TLS-intercepting proxy.
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; fi
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/mockhub ./cmd/mockhub && \
    go build -ldflags="-s -w" -o /out/testkit ./cmd/testkit && \
    go build -ldflags="-s -w" -o /out/tkprobe ./cmd/tkprobe && \
    go build -ldflags="-s -w" -o /out/tkstats ./cmd/tkstats

FROM ${BASE_IMAGE} AS mockhub
RUN adduser -D -u 10001 mock
COPY --from=build /out/mockhub /usr/local/bin/mockhub
USER mock
EXPOSE 8081 2525 8082
ENTRYPOINT ["/usr/local/bin/mockhub"]

# The runner image: testkit CLI + docker CLI/compose, used by ./tk on hosts that only have Docker.
FROM ${DOCKER_CLI_IMAGE} AS runner
COPY --from=build /out/testkit /usr/local/bin/testkit
ENV TESTKIT_IN_NETWORK=1
ENTRYPOINT ["/usr/local/bin/testkit"]

# Pinned OpenTelemetry collector + a static health probe (the upstream image has no shell).
FROM ${OTEL_COLLECTOR_IMAGE} AS otel-collector
COPY --from=build /out/tkprobe /tkprobe

# Container resource exporter (Docker stats API -> Prometheus, cAdvisor metric names).
FROM ${BASE_IMAGE} AS tkstats
COPY --from=build /out/tkstats /usr/local/bin/tkstats
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tkstats"]
