# syntax=docker/dockerfile:1.7
# Release image: docker build -t testkit/reference-worker:dev .
# Test image:    docker build --build-arg GO_TAGS=failpoint -t testkit/reference-worker:dev-failpoint .
# Failpoints exist only in the test image.
ARG GO_BUILD_IMAGE=golang:1.24.7-alpine3.21
ARG BASE_IMAGE=alpine:3.20.3

FROM ${GO_BUILD_IMAGE} AS build
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; fi
ARG GO_TAGS=""
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -tags "${GO_TAGS}" -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM ${BASE_IMAGE}
RUN adduser -D -u 10001 worker
COPY --from=build /out/worker /usr/local/bin/worker
USER worker
EXPOSE 8080
HEALTHCHECK --interval=2s --timeout=2s --retries=30 CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/worker"]
