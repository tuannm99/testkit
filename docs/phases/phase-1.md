# Phase 1 — observability, run_id everywhere, annotations, panel evidence

## Plan
1. Profile `observability`: Prometheus, Loki, Tempo, OTel collector, Alloy (logs), container-stats, kafka-exporter, Grafana + image renderer — all provisioned from files, all with health checks.
2. Run labels on containers (`testkit.run_id`, `testkit.ns`, `testkit.service`, `testkit.case`, `testkit.trigger`) turned into metric labels (Prometheus docker_sd) and log labels (Alloy).
3. Dashboards `tk-order-worker` (RED) and `tk-run` (USE, consumer lag, log levels) with `run_id`/`ns` variables and a per-run annotation query.
4. Adapters `collect/prometheus` (query_range → raw JSON + CSV + stats), `collect/loki`, `collect/grafana` (annotations, render, locked links, capture = image + raw data of the same queries).
5. CLI `testkit annotate`, `testkit collect`; `core/evidence` (run dir, manifest with sha256, verify).
6. Demo run + automated checks: `scripts/acceptance/phase1.sh`.

## Files
`infra/{prometheus,loki,tempo,otel-collector,alloy,grafana}/`, observability services in `infra/compose/docker-compose.yml`,
`testkit/adapters/collect/{prometheus,loki,grafana,dockerstats}`, `testkit/cmd/{tkstats,tkprobe}`,
`testkit/cmd/testkit/obs_cmds.go`, `testkit/core/evidence`, `panels:` in `testkit/services/order-worker.yaml`.

## Verification

`testkit up --services order-worker --profile core,stores,mocks,observability`:
```
up: 14 services healthy in 11s: alloy, container-stats, elasticsearch, grafana, grafana-renderer, kafka,
    kafka-exporter, loki, mailpit, mockhub, otel-collector, postgres, prometheus, tempo
```

Acceptance demo (`scripts/acceptance/phase1.sh`, log in `out/acceptance/phase1.log`): a worker labelled
with a fresh run id processes 300 orders (Kafka + DB poll), then 100 more while the payment mock answers
500 → 429 → 201; four annotations mark the steps; `testkit collect` exports the window; the script checks
that every panel has a PNG, a link locked to the window, raw samples only inside the window, only the
run's `run_id`, Loki logs with the run id, annotations and labelled metrics.
```
  consumer_lag      Kafka consumer lag by group                      image=ok points=48
  container_cpu     Container CPU (cores)                            image=ok points=12
  container_memory  Container memory (working set)                   image=ok points=13
  e2e_latency       End-to-end latency p95 (enqueue -> done)         image=ok points=9
  error_rate        Error rate — share of jobs not ok                image=ok points=9
  in_flight         In-flight jobs / dead letters                    image=ok points=23
  log_levels        Log lines by level                               image=ok points=18
  p95_latency       Process latency p50/p95/p99                      image=ok points=27
  payment_calls     Payment API calls by HTTP code                   image=ok points=17
  payment_latency   Payment API latency p95                          image=ok points=10
  poller            Poller: empty polls/s and claims/s               image=ok points=33
  throughput        Throughput — jobs/s by source and result         image=ok points=17
  loki logs with run_id=r20261004-181408-p1: 802
  grafana annotations tagged r20261004-181408-p1: 4
  prometheus series with run_id=r20261004-181408-p1 (worker_jobs_total): 2
PHASE1 ACCEPTANCE: PASS (evidence in out/r20261004-181408-p1/demo)
```
Per panel the evidence directory holds `<name>.png`, `<name>.<ref>.prom.json` (raw API answer),
`<name>.<ref>.csv`, and `panels.json` (title, locked link, interpolated queries, min/max/last per series).
Images were inspected: real series with the step and fault annotations as shaded regions.

Unit tests: `go test ./...` (adds `core/evidence`: seal + verify detects modified/added files).

## Risks / open points
- cAdvisor replaced by `tkstats` (A16). Per-container network/disk counters depend on the engine's stats API.
- Rendering uses the Chromium-based renderer (≈ 1–2 s per panel); a run with many panels × steps should
  render only the panels named in `evidence.grafana`.
- Tempo/OTel are provisioned and healthy, but the reference worker does not emit traces yet (not required by the acceptance criterion).
