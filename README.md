# TestKit

Shared toolkit to test many services the same way: functional / integration /
cross-service E2E, performance (load, stress, spike, soak) against a baseline,
and chaos — with evidence collected automatically and a report a QC engineer
can sign off without asking a developer.

Everything is declared in files (services, scenarios, infrastructure,
thresholds), everything runs on Docker, and pass/fail is decided by hard
rules (assertions, SLO thresholds) — never by an AI.

## Quick start (host with only Docker)

```sh
./tk doctor                         # docker, compose, privileges, ports, RAM, disk
./tk up --services order-worker     # only what that service declares, waits for health checks
./tk status
./tk down                           # removes containers, volumes, network (idempotent)
```

With Go 1.24 installed you can use the CLI directly: `make build && ./bin/testkit doctor`.

Behind a TLS-intercepting proxy, set `TESTKIT_BUILD_CA=/path/to/ca.pem` so image builds trust it.

## Layout

```
testkit.yaml                 project config (paths, network, images TestKit builds)
infra/compose/               docker-compose.yml (profiles), versions.env (pinned images), testkit.env (ports, test creds)
testkit/                     Go module: CLI, Mock Hub, core, adapters, steps
  cmd/testkit  cmd/mockhub
  core/        config, infra (+ scenario, orchestrator, evidence, assert, report in later phases)
  adapters/    mock/http (+ stores, triggers, chaos, collectors, executors, outputs)
  services/    one YAML per service under test
  mocks/       OpenAPI documents served to the Mock Hub
reference-worker/            sample service (Kafka + DB-poll worker) proving the kit end to end
docs/                        assumptions.md, phases/phase-N.md (what was done, how it was verified)
```

Layering is enforced by a test (`testkit/internal/archtest`): `core` ← `adapters` ← `steps` ← scenarios (YAML).

## Adding a service

Add `testkit/services/<name>.yaml` (see `order-worker.yaml`): image/build,
ports, health, stores it uses, mocks of the third parties it calls, triggers,
entities used by checks, failpoints, and the env it needs (templated per run
namespace). No core code changes.

## Status

See `docs/phases/` for the report of each phase and its acceptance evidence.
