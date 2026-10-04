# Phase 5 — Chaos, performance with a baseline, mutation gate

## Plan
1. Chaos per execution: Toxiproxy (built from a pinned source version) gives every execution its own
   proxies in front of the dependencies declared in `chaos.proxies`; faults = latency, timeout, reset,
   bandwidth, down. Infrastructure faults (pause/stop a store, pause the service, CPU hog) and packet
   faults (`tc netem`, needs NET_ADMIN + `sch_netem`) as steps declaring the capability they need.
2. Experiment steps: `load.start` (open model, constant arrival rate) → steady state → fault →
   `chaos.hold` with `abort_if` (blast radius) → `chaos.clear` → `chaos.recover` (recovery time).
3. Performance: trigger executor (end-to-end latency = DB completion − send time) and k6
   (`constant-arrival-rate`), warm-up excluded, N repetitions, SLO thresholds, medians + samples.
4. Baselines per environment fingerprint; regression = beyond the allowed ratio and significant
   (Mann-Whitney U, α = 0.05).
5. Mutation gate `testkit admit` for new cases (green repeatedly, red under every declared defect)
   with the approval recorded in the case file.

## Scenarios
| Case | What it proves |
|------|----------------|
| TC-CHAOS-001 | payment gateway +1.5 s for 8 s under 10 job/s: no lost job, no double charge, recovery ≤ 30 s |
| TC-CHAOS-002 | Postgres unreachable 5 s under load: nothing dead-lettered, nothing lost; mutation `outage_is_failure` must go red |
| TC-CHAOS-003 | worker frozen 3 s (docker pause, db-poll): no loss, no duplicate after resume |
| TC-CHAOS-004 | 300 ms + 5 % packet loss + CPU hog: everything processed exactly once (needs `netem`) |
| TC-PERF-001 | 20 job/s through Kafka, 3 repetitions: p95/p99 latency, throughput, compared with the baseline |
| TC-PERF-002 | refund webhook 50 req/s with k6: p95, error rate |
| TC-PERF-901 | draft demo: same load, payment 45 ms instead of 20 ms → must be reported as a regression |

## Product defect found
TC-CHAOS-002 first failed for a real reason: a 5 s Postgres outage consumed the job's delivery
attempts and dead-lettered it. The reference worker now classifies connection/availability errors
as `domain.ErrUnavailable` and retries them with capped backoff without consuming attempts
(commit "reference-worker: retry jobs on store outages instead of dead-lettering"). The failpoint
`outage_is_failure` restores the old behaviour; the case goes red with it (counter-evidence).

## TestKit defect found (load generator)
The first acceptance run turned TC-CHAOS-001 red: 199/200 orders paid. The evidence showed the
missing order's job was never in the Kafka topic, so the worker was not at fault. Cause: `load.start`
sent each job in a fire-and-forget goroutine and `load.wait` cancelled the generator as soon as the
last job was *scheduled*, cancelling the in-flight produce of the last job; its error was counted
after the step had already reported "200 sent, 0 errors". Fixed: started sends always complete
(own timeout), the load is done only when every send returned, `sent` counts acknowledged jobs, and
`load.wait` / `load.stop` fail the execution as an *environment* error when the generator lost or
could not enqueue a job — a generator fault can no longer look like a product defect. The perf
executor does the same (it previously folded enqueue failures into the service's error rate).
Regression tests: `core/orchestrator/experiment_test.go` (red on the old code: "delivered 9, report
sent:11 errors:2"), run with `-race`.

## Mutation gate
```
./tk admit <case.yaml>...                       # report only
./tk admit --approve --by qc-lead <case.yaml>   # if every case is admitted: status approved + admission block
```
Rules (deterministic, `core/orchestrator/admission.go`): green on the unbroken system `--stability`
times per trigger (default 2, a red repetition = flaky = rejected); every declared mutation turns it red
with its `expect_red` assertions red; perf cases compared with a baseline; no mutation = rejected;
missing capability = incomplete (never admitted). The approval writes `admission:` (run id, person,
mutations killed, sha256 of the run's manifest) into the case file; lint refuses a drafted case
(`generated:`) that is `approved` without it.

## Verification
`scripts/acceptance/phase5.sh` (log `out/phase5-acceptance.log`), run `r20261004-201120-*`:
```
== 1. chaos            (run --mutations testkit/scenarios/chaos)
  TC-CHAOS-001[kafka]    PASS    recovery=2.6s aborted=False
  TC-CHAOS-002[kafka]    PASS    recovery=6.2s aborted=False
  TC-CHAOS-003[db-poll]  PASS    recovery=3.1s aborted=False
  TC-CHAOS-004[kafka]    SKIPPED step chaos.netem needs capability netem, not available on this host (testkit doctor)
  TC-CHAOS-002 M1 outage_is_failure -> red ['A1', 'A2', 'A3'] KILLED
  run note: TC-CHAOS-004[kafka] skipped (not failed): step chaos.netem needs capability netem ...
== 2. performance and baseline   (baseline order-pipeline-20rps: 5 runs, recorded 19:47Z)
  TC-PERF-001[kafka]   PASS  p95=169ms thr=20.00/s p95_ms -48% p=0.804, p99_ms -33% p=0.714, throughput -1% p=0.500
  TC-PERF-002          PASS  p95=3ms thr=50.07/s (baseline recorded afterwards, see below)
  TC-PERF-901[kafka]   FAIL  p95=13277ms thr=12.79/s p95_ms +4031% p=0.018 REGRESSION, p99_ms +3141% p=0.018 REGRESSION, throughput +36% p=0.018 REGRESSION
== 3. mutation gate
  REJECTED   TC-ORDER-030 (weak draft)
    ok   green-and-stable   TC-ORDER-030[kafka] passed 2/2 times
    FAIL mutations-killed   M1[kafka] SURVIVED (pass; red: [], required: [])
  ADMITTED   TC-ORDER-030 (reviewed draft)
    ok   green-and-stable   TC-ORDER-030[kafka] passed 2/2 times
    ok   mutations-killed   M1[kafka] killed (red: [A3 A4])
  approved TC-ORDER-030 by qc-lead → admission {run_id, by, mutations_killed, manifest_sha256}
  lint bypass.yaml: error: case drafted by draft-tool is approved without passing the mutation gate
  verify: every file matches manifest.json (×4)
PHASE5 ACCEPTANCE: PASS
```
("+36 %" for throughput is the degradation: 19.87 → 12.79 jobs/s; the report writes "tệ đi 36 %".)

TC-PERF-002 baseline `webhook-refund-50rps` recorded (5 runs, p95 median 3.26 ms), then
`run r20261004-202501-7f2e`: p95 +6.6 % p=0.393, p99 −31 %, throughput 0 % → within baseline. The script now
records any missing baseline first and requires both perf cases to be compared.

Unit tests: `go test ./...` in both modules; `go test -race` for `core/orchestrator`
(load generator, admission rules), report perf/chaos/admission sections, `scenario.Approve`.

## Limits / open points
- TC-PERF-901 also shows a capacity limit worth knowing: with the payment gateway at 45 ms the
  reference worker tops out near 12.8 jobs/s (its Kafka consumer handles one record at a time, ≈ 78 ms each), so a
  20 job/s arrival rate builds a backlog and p95 grows to seconds — the open model makes this visible.
- This sandbox has no `sch_netem`: TC-CHAOS-004 is skipped here (class `capability`, run note), never failed.
- Baselines are machine specific (A49); the committed one is for `4cpu-16g-16c8f4efd5`. On another
  machine the acceptance script records one first.
- p95 on a shared 4-CPU host is noisy (baseline samples 239–475 ms): the significance test keeps
  this noise from being reported as a regression (TC-PERF-001 +75 % with p = 0.125 is "not significant"),
  while a real slowdown (TC-PERF-901) gives p = 0.018. Five samples is the minimum for α = 0.05
  with three current repetitions; more repetitions tighten it.
- SLO values in the perf cases are placeholders until the real SLOs are given (section 9).
