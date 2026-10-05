# Phase 0 — skeleton, CLI, infrastructure, reference worker

## Plan
1. Project config (`testkit.yaml`), pinned versions (`infra/compose/versions.env`), ports/test credentials (`testkit.env`).
2. Compose file with profiles `core | stores | mocks | observability | chaos`, health check on every container, 127.0.0.1-only ports.
3. `core/config` (project, endpoints, service descriptor), `core/infra` (docker CLI driver, compose stack, builds, doctor).
4. CLI `testkit doctor | up | down | status`; `./tk` wrapper for Docker-only hosts.
5. Mock Hub (HTTP/webhook part) with namespaces, scripts, faults, journal, OpenAPI validation.
6. Reference worker: Kafka + DB-poll triggers sharing `Process`, payment API with retry/Idempotency-Key, Postgres + outbox, Elasticsearch, SMTP; Uber Fx wiring; failpoints behind a build tag.
7. Verification: unit tests, manual smoke of the worker, then the acceptance script.

## What was built
| Area | Files |
|------|-------|
| Project config | `testkit.yaml`, `infra/compose/{versions,testkit}.env` |
| Infrastructure | `infra/compose/docker-compose.yml` |
| CLI | `testkit/cmd/testkit`, `tk` |
| Core | `testkit/core/config`, `testkit/core/infra` |
| Mock Hub | `testkit/cmd/mockhub`, `testkit/adapters/mock/http`, `testkit/mocks/payment.openapi.yaml` |
| Service descriptor | `testkit/services/order-worker.yaml` |
| Reference worker | `reference-worker/` |
| Self tests | `testkit/internal/archtest` (layering), `core/infra/stack_test.go`, `adapters/mock/http/hub_test.go` |

`testkit up --services order-worker` starts only what the descriptor declares
(postgres, kafka, elasticsearch, mockhub, mailpit) — not ClickHouse/Mongo/Redis.

## Verification

### Unit tests
```
$ cd testkit && go test ./...
ok  github.com/tuannm99/testkit/testkit/adapters/mock/http
ok  github.com/tuannm99/testkit/testkit/core/infra
ok  github.com/tuannm99/testkit/testkit/internal/archtest
```

### Manual smoke of the reference worker (both triggers)
Order `o2` via DB-poll with payment script `500, 429(Retry-After: 1), 201`; order `o1` via Kafka:
```
 id | status | payment_ref | mailed        job_key | status | attempts      event_id      | published
 o2 | paid   | ch_o2       | t             j-o2    | done   |        1      order.paid:o2 | t
 o1 | paid   | ch_o1       | t                                             order.paid:o1 | t
ES count = 2
mock journal: 1 /v1/charges 500 order-o2 | 2 ... 429 order-o2 | 3 ... 201 order-o2 | 4 ... 201 order-o1   (0 schema errors)
mails to c1: 1
```

### Acceptance: Docker-only host, `up` + `down` three times
Run from a clean engine (TestKit images and build cache removed) through `./tk`
(no Go toolchain used): `scripts/acceptance/phase0.sh`. Full log: `out/acceptance/phase0.log`.
```
doctor: host is ready
=== iteration 1: up       up: 5 services healthy in 52s (includes building mockhub + worker images)
=== iteration 1: after down: containers=0 volumes=0 networks=0
=== iteration 2: up       up: 5 services healthy in 20s
=== iteration 2: after down: containers=0 volumes=0 networks=0
=== iteration 3: up       up: 5 services healthy in 20s
=== iteration 3: after down: containers=0 volumes=0 networks=0
PHASE0 ACCEPTANCE: PASS (3/3 up+down cycles clean)
```
Sandbox-specific environment used for the run: `TK_NOFILE_LIMIT=20000` (engine
caps RLIMIT_NOFILE; `doctor` detects it) and `TESTKIT_BUILD_CA` (TLS proxy).

## Risks / open points
- The observability and chaos profiles are declared in the plan but filled in Phases 1 and 5.
- `ghcr.io` blobs are blocked in this sandbox, so the default Toxiproxy image cannot be pulled here (Phase 5 will document the fallback).
- Questions of section 9 (QC tool, Kubernetes, socket protocols, other brokers, nightly sandbox, SLOs) are still open; none blocks Phases 1–3.
