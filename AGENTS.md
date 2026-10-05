# Guide for AI agents working on TestKit

This file is for any coding or QA agent (Claude Code, other assistants, scripted agents) that writes,
runs or investigates tests in this repository. Read it fully before changing anything. People decide;
the deterministic rules of TestKit decide pass/fail; agents draft, run, explain and propose.

## Non-negotiable rules

1. **Never decide or change a result.** Pass/fail comes from assertions (operator + expected value), SLO
   thresholds, mutation runs and the release gate. Do not edit `run.json`, `report.html`, `manifest.json`
   or anything under `out/<run_id>/` by hand. A bundle that changed after sealing is refused by `verify`,
   `pack`, `report`, `qc export` and `ai *`.
2. **Never make a red case green by weakening it.** Do not loosen an operator or threshold, delete an
   assertion, raise `within` to hide slowness, add retries inside the test, or mark a case flaky. Find
   the cause: product defect, environment, test defect, or flakiness (and then fix the race).
3. **Never approve a case.** New cases are `status: draft`. Only a person runs
   `testkit admit --approve --by <name>` after the mutation gate admitted the case. Do not add or edit
   `admission:` blocks; do not change `status:` to `approved`.
4. **No secrets or personal data in files**: not in cases, fixtures, mocks, evidence or commits. Use the
   test-only values of `infra/compose/testkit.env` and per-run identifiers containing `{{ .ns }}`.
   `pack` refuses a bundle with a declared secret in it.
5. **Only redacted data leaves the machine.** Use `testkit ai ...` for model calls: it redacts, checks
   (refuses if anything sensitive remains) and records every request. To see what would be sent:
   `testkit ai context out/<run_id> --task triage`. Never paste raw logs, snapshots, mails or `.env`
   values into a chat or an external tool.
6. **No real third parties** in tests: mocks only (OpenAPI-validated). **No SQLite/in-memory** stores in
   place of the real ones. **No `sleep`**: wait on a condition (`wait.until`, `within`, `assert.during`).
7. **Everything is declared in files.** A new service is a YAML file in `testkit/services/`, not core code.
   No hard-coded addresses or credentials.
8. Missing privilege (NET_ADMIN, netem, docker.sock) → the case is skipped and reported, never "fixed" by
   removing the requirement.

## Repository map

```
testkit.yaml              project config (paths, images, qc, ai)
infra/compose/            docker-compose.yml, versions.env (pinned images), testkit.env (ports, test creds)
testkit/core/             config, kit (interfaces), scenario (DSL, lint), orchestrator, assert, evidence,
                          result, report, perf (stats/baselines), ai (redaction, prompts, tasks)
testkit/adapters/         stores, triggers, mocks, chaos, exec (k6, playwright), collectors, qc, ai providers
testkit/steps/            vocabulary of steps and checks (`testkit steps`)
testkit/services/         one YAML per service under test
testkit/scenarios/        cases (YAML); drafts/ holds drafts; suites in testkit/suites/
ui-tests/                 Playwright TypeScript UI tests (run by the ui.run step)
reference-worker/         sample service used to prove the kit
docs/                     assumptions.md (decisions), phases/, huong-dan-qc.md (QC guide, Vietnamese)
```
Layering is enforced by `testkit/internal/archtest`: core never imports adapters.

## Commands

Use `./tk <cmd>` (Docker only) or `./bin/testkit <cmd>` (built with `make build`).

| Goal | Command |
|---|---|
| Stack up / health | `./tk doctor`, `./tk up --services <svc>`, `./tk status` |
| What a case may use | `./tk steps` (steps, checks, operators), `testkit/services/<svc>.yaml` (entities, mocks, triggers, failpoints) |
| Check / preview a case | `./tk lint <file>`, `./tk plan <file>` |
| Run | `./tk run <file|dir>` (`--mutations`, `--retries 1`), suites: `./tk run testkit/suites/release.yaml` |
| Mutation gate for a new case | `./tk admit <file>` (people add `--approve --by <name>`) |
| Evidence | `out/<run_id>/report.html`, `case.json`, `assertions/*.json`, `timeline.json`, `logs/`, `output/` |
| Integrity / bundle | `./tk verify out/<run_id>`, `./tk pack out/<run_id>` |
| Assistant (optional) | `./tk ai context|triage|summary out/<run_id>`, `./tk ai draft --service <svc> --requirement "..."` |

## Workflow: write a new test case

1. Read the requirement, the service descriptor and 1–2 approved cases of the same service (style).
2. Write `testkit/scenarios/drafts/<ID>.yaml` with `status: draft` (or run `./tk ai draft`):
   - purpose and preconditions in Vietnamese; each expectation has `id`, check, operator + expected value
     and a `why`; expectations must be able to fail (no tautologies);
   - per-run data (`{{ .ns }}` in identifiers and addresses); the declared triggers (Kafka and DB poll when
     the service has both: the same scenario must give the same result through both);
   - at least one `mutations:` entry: a failpoint of the service that breaks the behaviour, with the
     `expect_red` assertions that must turn red. If no failpoint fits, say so — the service needs one.
3. `./tk lint`, `./tk plan`, `./tk run --mutations <file>`; read the evidence of every execution.
4. `./tk admit <file>`. If a mutation **survives**, the case does not detect its defect: strengthen the
   case. If it is **not evaluated** (errored), fix the cause. If the expected red assertions are wrong
   (e.g. a protection elsewhere legitimately keeps one green), correct `expect_red` and say why.
5. Hand over to a person for `--approve`. Report what you verified, with run ids.

## Workflow: investigate a red run

1. Open `out/<run_id>/report.html`: the failed step, the assertions (expected / actual / observed at),
   the rule-based class (product / environment / test / flaky / capability), the timeline, log tail and
   counter-evidence. Use `testkit ai triage out/<run_id>` for a second opinion (advisory only; check its
   citations).
2. Look for facts, not guesses: which executions ran at the same time (timing failures under contention),
   what the mocks journaled, the store snapshots, the generator counters (`load.*`).
3. Fix the cause where it lives — service, scenario, environment or TestKit — with a regression test.
   Re-run and show the new run id. Flaky cases are quarantined only by people (owner, ticket, deadline).

## Workflow: performance

Baselines are per environment fingerprint and must be recorded on an idle stack
(`./tk baseline record <case>`); perf and load-driven cases run alone. A result much better than the
baseline is flagged as a stale baseline — re-record it, do not ignore it.

## Before you finish a change

- `cd testkit && go vet ./... && go test ./...` (and `reference-worker` if touched), `./tk lint`.
- The acceptance script of the area you touched (`scripts/acceptance/phase*.sh`).
- New decisions go to `docs/assumptions.md`; small commits with clear messages.
