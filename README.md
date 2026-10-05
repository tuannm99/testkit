# TestKit

Shared toolkit to test many services the same way: functional / integration /
cross-service E2E, performance (load, stress, spike, soak) against a baseline,
and chaos — with evidence collected automatically and a report a QC engineer
can sign off without asking a developer.

Everything is declared in files (services, scenarios, infrastructure,
thresholds), everything runs on Docker, and pass/fail is decided by hard
rules (assertions, SLO thresholds) — never by an AI.

## Documentation

| Document | For |
|---|---|
| [docs/huong-dan-su-dung.md](docs/huong-dan-su-dung.md) | User guide (Vietnamese): install, run, read results, write cases, perf/chaos/UI, release, CI, troubleshooting |
| [docs/tham-chieu-testcase.md](docs/tham-chieu-testcase.md) | Test case reference: fields, `given`, steps, checks, operators, templates, mutations, suites and the gate |
| [docs/tham-chieu-service.md](docs/tham-chieu-service.md) | Service descriptor reference: image, stores, mocks, triggers, entities, failpoints, env, chaos, reconcile, panels |
| [docs/tham-chieu-cau-hinh.md](docs/tham-chieu-cau-hinh.md) | Configuration reference: `testkit.yaml` (qc, ai), env files, environment variables, every CLI command |
| [docs/huong-dan-qc.md](docs/huong-dan-qc.md) | QC: run the release, read the report, hand over results |
| [AGENTS.md](AGENTS.md) | Rules and workflows for AI agents |
| [docs/assumptions.md](docs/assumptions.md), [docs/phases/](docs/phases/) | Design decisions and the build log of each phase |

## Quick start (host with only Docker)

```sh
./tk doctor                         # docker, compose, privileges, ports, RAM, disk
./tk up --services order-worker     # only what that service declares, waits for health checks
./tk status
./tk down                           # removes containers, volumes, network (idempotent)
```

Running tests and reading the evidence:

```sh
./tk lint                                   # every scenario: steps, checks, templates, evidence
./tk plan testkit/scenarios/order/TC-ORDER-017.yaml   # what a case will do, without running it
./tk run testkit/scenarios                  # → out/<run_id>/report.html, junit.xml, traceability.csv, manifest.json
./tk run --mutations testkit/scenarios/order          # + counter-evidence: each case must go red with the system broken
./tk run testkit/scenarios/chaos            # chaos experiments (Toxiproxy; netem needs NET_ADMIN, else skipped)
./tk baseline record testkit/scenarios/perf/TC-PERF-001.yaml   # perf baseline of this environment
./tk run testkit/scenarios/perf             # SLOs + statistical comparison with the baseline
./tk admit --approve --by <name> <new-case.yaml>      # mutation gate before a new case is approved
./tk verify out/<run_id>                    # evidence bundle unchanged since the run (sha256 manifest)
```

Release (what QC signs off — see `docs/huong-dan-qc.md`):

```sh
./tk run testkit/suites/release.yaml        # approved cases + mutations + retries → GO / NO-GO
                                            # → out/<run_id>.zip (report, evidence, Zephyr Scale import files)
./tk qc cases                               # out/qc/testcases.csv + .md (tool-neutral)
                                            # each bundle has qc/results.csv + .md and qc/testcases.csv + .md
ZEPHYR_TOKEN=... ./tk qc push out/<run_id>  # optional, with qc.tool: files, zephyr-scale
```

Optional AI assistant (advisory only, redacted data only — `ai:` in `testkit.yaml`; agents read `AGENTS.md`):

```sh
./tk ai context out/<run_id> --task triage  # exactly what would be sent (redacted); sends nothing
./tk ai triage out/<run_id>                 # suggested causes for red / flaky / weak cases → ai/triage.md
./tk ai summary out/<run_id>                # sign-off summary (facts by TestKit) → ai/summary.md
./tk ai draft --service <svc> --req-id <REQ> --requirement "..."   # draft case → lint → admit by a person
./tk pack out/<run_id>                      # re-pack: checks the manifest and that no secret is inside
```

With Go 1.24 installed you can use the CLI directly: `make build && ./bin/testkit doctor`.

Behind a TLS-intercepting proxy, set `TESTKIT_BUILD_CA=/path/to/ca.pem` so image builds trust it.

## Layout

```
testkit.yaml                 project config (paths, network, images TestKit builds)
infra/compose/               docker-compose.yml (profiles), versions.env (pinned images), testkit.env (ports, test creds)
testkit/                     Go module: CLI, Mock Hub, core, adapters, steps
  cmd/testkit  cmd/mockhub
  core/        config, infra, kit, scenario, orchestrator, assert, evidence, result, report, perf
  adapters/    mock (http, smtp, socket, mail), store, trigger, sut, chaos, exec (k6), collect
  steps/       step and check vocabulary used by scenarios (`./tk steps`)
  scenarios/   test cases (YAML), one directory per area
  baselines/   perf baselines per environment fingerprint
  suites/      release suites (kind: Suite) and their gate
  services/    one YAML per service under test
  mocks/       OpenAPI documents served to the Mock Hub
reference-worker/            sample service (Kafka + DB-poll worker) proving the kit end to end
ui-tests/                    Playwright (TypeScript) UI tests, run by the ui.run step in the pinned Playwright image
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
