# Convenience targets for contributors with Go installed. Docker-only hosts use ./tk.
GO ?= go
export GOTOOLCHAIN ?= local

.PHONY: build test vet fmt acceptance-phase0

build:
	cd testkit && $(GO) build -o ../bin/testkit ./cmd/testkit && $(GO) build -o ../bin/mockhub ./cmd/mockhub

test:
	cd testkit && $(GO) test ./...
	cd reference-worker && $(GO) test ./... && $(GO) test -tags failpoint ./...

vet:
	cd testkit && $(GO) vet ./...
	cd reference-worker && $(GO) vet ./... && $(GO) vet -tags failpoint ./...

fmt:
	gofmt -w testkit reference-worker

acceptance-phase0:
	./scripts/acceptance/phase0.sh
