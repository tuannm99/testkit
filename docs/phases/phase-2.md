# Phase 2 — Scenario DSL, lint, plan, orchestrator, evidence bundle, report

## Plan
1. `core/kit`: contracts (Connector, Trigger, Checker, Step/Check definitions, Namespace, Registry).
2. `core/scenario`: case model (mandatory id, title, requirement, risk, purpose, preconditions, input, steps/given, expect[] with check/operator/expected/why, evidence), strict YAML, `given` expansion, response shorthand, templates, `lint`.
3. `core/assert`: operators, check-expression parser, Eventually (polling, never sleeping).
4. `core/orchestrator`: case × trigger executions in isolated namespaces; provision → steps → poll → drain → final check → collect → teardown; timeline + Grafana annotations; deterministic "why it passed" sentences; mutation runs attached as counter-evidence; flaky detection via retries.
5. Connectors needed by the first P0 case: Postgres, Kafka (+ trigger), DB-poll trigger, Elasticsearch, Mock Hub, Mailpit, service-under-test containers; `steps/` library.
6. `core/report`: report.html (offline, QC reading order), junit.xml, traceability.csv, run.json; manifest sealed last; `testkit verify`.
7. CLI: `lint`, `plan`, `steps`, `run`, `report`, `verify`.

## Verification

Unit tests (`go test ./...`): assert operators/parser/Eventually, scenario lint (12 kinds of errors detected
in one bad case, unknown keys rejected), response shorthand, templates, JUnit/traceability/HTML rendering
and escaping, evidence seal/verify, layering.

Acceptance (`scripts/acceptance/phase2.sh`, log `out/acceptance/phase2.log`) on `TC-ORDER-017` (P0):
```
lint: 1 case(s) OK
plan: 2 execution(s) planned
run r20261004-184300-p2: 2 pass, 0 fail, 0 error, 0 skipped in 52s
  PASS    TC-ORDER-017[kafka]
  PASS    TC-ORDER-017[db-poll]
verify: every file matches manifest.json
  TC-ORDER-017[kafka]: 7 assertions with evidence, 5 panels, 13 artifacts
  TC-ORDER-017[db-poll]: 7 assertions with evidence, 5 panels, 13 artifacts
  manifest: commit c0a9feb40530, 20 pinned images, 96 hashed files
PHASE2 ACCEPTANCE: PASS
```
The script checks that the report sections appear in the required order, that every assertion carries
id/result/check/operator/expected/actual/why/evidence/observed_at with existing evidence files, that each
conclusion sentence points to evidence, and that every Grafana panel has an image and raw data.

### What the report says (excerpt of section 7, generated, no free text)
> A2 đạt — 2 lần lỗi + 1 lần thành công, không gọi thừa (job trùng không gọi lại). Kỳ vọng
> `mock.payment.calls` = 3; thực tế = 3 lúc 18:39:15.874Z. Bằng chứng: A2.json payment.journal.json provenance.json

### A real defect found on the way
The first DB-poll run failed: `A2 … held at 18:35:00.53 but changed after the triggers drained: expected 3, got 4`
(run `r20261004-183442-f247`). The journal and the worker log showed two duplicate deliveries of job `o1`
processed concurrently and **both** claiming the order: the claim owner was built from
`job_key + attempt`, identical for duplicates, and the claim accepts its own owner. The payment API was
called a fourth time; only the Idempotency-Key prevented a double charge (A4). Fix in the reference worker:
the owner is unique per delivery (Kafka topic/partition/offset, DB job row id). The case was not relaxed.

The first run of all (`r20261004-183157-f4b8`) failed because the Mock Hub image was stale (status-only
responses had no body); the worker correctly treated an unparseable 2xx as an error. Root cause fixed in
TestKit: images are rebuilt when their sources change (A28).

## Risks / open points
- Panels are rendered per case window with step annotations; per-step panel windows are not offered yet.
- `mail.*` isolation depends on recipients containing the namespace (A24).
- Mutation runs are wired (`--mutations`) and shown in section 8; the mutation gate itself is Phase 5.
