#!/bin/sh
# Phase 5 acceptance:
#  1. chaos experiments pass (steady state, fault, abort condition, recovery
#     time) and TC-CHAOS-002 goes red when store outages dead-letter jobs;
#     cases needing a missing capability (netem) are skipped, not failed;
#  2. perf cases pass their SLOs and are compared with this environment's
#     baseline (recorded first if missing); the injected regression
#     (TC-PERF-901, payment +45ms) is reported as a statistically significant
#     regression;
#  3. the mutation gate rejects a weak drafted case, admits the reviewed one
#     and records the approval; lint blocks approving a draft without it.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
STAMP=$(date -u +%Y%m%d-%H%M%S)
FIX=scripts/acceptance/fixtures/phase5

(cd testkit && GOTOOLCHAIN=local go test -count=1 ./core/perf/ ./core/report/ ./core/orchestrator/ ./core/scenario/ ./adapters/chaos/... ./adapters/exec/...)
$TK lint testkit/scenarios

echo "== 1. chaos"
$TK run --run-id "r$STAMP-p5c" --mutations testkit/scenarios/chaos || true
python3 - "out/r$STAMP-p5c" <<'PY'
import json, sys
run = json.load(open(sys.argv[1] + "/run.json"))
base = {e["case_id"]: e for e in run["executions"] if not e.get("mutation")}
for cid, e in sorted(base.items()):
    c = e.get("chaos") or {}
    print(f"  {e['id']:<22} {e['result'].upper():<7} recovery={c.get('recovery_seconds')}s aborted={c.get('aborted')} {e.get('reason','')}")
for cid in ("TC-CHAOS-001", "TC-CHAOS-002", "TC-CHAOS-003"):
    assert base[cid]["result"] == "pass", f"{cid} {base[cid]['result']}: {base[cid].get('reason')}"
n = base["TC-CHAOS-004"]
assert n["result"] in ("pass", "skipped"), f"TC-CHAOS-004 {n['result']}: {n.get('reason')}"
if n["result"] == "skipped":
    assert "netem" in n["reason"], n["reason"]
    assert any("netem" in x for x in run.get("notes", [])), "missing capability not reported"
m = {x["failpoint"]: x for x in base["TC-CHAOS-002"].get("mutations", [])}
assert m["outage_is_failure"]["killed"], m
print(f"  TC-CHAOS-002 M1 outage_is_failure -> red {m['outage_is_failure']['red_assertions']} KILLED")
PY

echo "== 2. performance and baseline"
for b in order-pipeline-20rps:TC-PERF-001 webhook-refund-50rps:TC-PERF-002; do
  if ! $TK baseline show | grep -q "${b%%:*}"; then
    $TK baseline record --runs 5 "testkit/scenarios/perf/${b#*:}.yaml"
  fi
done
$TK run --run-id "r$STAMP-p5p" testkit/scenarios/perf || true
python3 - "out/r$STAMP-p5p" <<'PY'
import json, sys
run = json.load(open(sys.argv[1] + "/run.json"))
perf = {p["id"].split("[")[0]: p for p in run.get("perf", [])}
for cid, p in sorted(perf.items()):
    m = p["metrics"]
    cmp = ", ".join(f"{c['metric']} {c['change_pct']:+.0f}% p={c['p_value']:.3f}{' REGRESSION' if c['regression'] else ''}" for c in p.get("comparisons", []))
    print(f"  {p['id']:<20} {p['result'].upper():<5} p95={m.get('p95_ms', 0):.0f}ms thr={m.get('throughput', 0):.2f}/s {cmp or p.get('note', '')}")
assert perf["TC-PERF-001"]["result"] == "pass" and perf["TC-PERF-001"]["comparisons"], perf["TC-PERF-001"]
assert not any(c["regression"] for c in perf["TC-PERF-001"]["comparisons"])
assert perf["TC-PERF-002"]["result"] == "pass" and perf["TC-PERF-002"]["comparisons"], perf["TC-PERF-002"]
reg = [c for c in perf["TC-PERF-901"]["comparisons"] if c["regression"]]
assert perf["TC-PERF-901"]["result"] == "fail" and any(c["metric"] == "p95_ms" for c in reg), perf["TC-PERF-901"]
PY

echo "== 3. mutation gate"
if $TK admit --run-id "r$STAMP-p5w" "$FIX/TC-ORDER-030.weak.yaml"; then
  echo "weak drafted case was admitted" >&2; exit 1
fi
DEMO="out/r$STAMP-p5-admit"
mkdir -p "$DEMO"
cp "$FIX/TC-ORDER-030.reviewed.yaml" "$DEMO/TC-ORDER-030.yaml"
$TK admit --run-id "r$STAMP-p5a" --approve --by qc-lead "$DEMO/TC-ORDER-030.yaml"
grep -q '^status: approved$' "$DEMO/TC-ORDER-030.yaml"
grep -q "run_id: r$STAMP-p5a" "$DEMO/TC-ORDER-030.yaml"
grep -q "manifest_sha256: $(sha256sum "out/r$STAMP-p5a/manifest.json" | cut -d' ' -f1)" "$DEMO/TC-ORDER-030.yaml"
$TK lint "$DEMO/TC-ORDER-030.yaml"
sed 's/^status: draft/status: approved/' "$FIX/TC-ORDER-030.weak.yaml" > "$DEMO/bypass.yaml"
if $TK lint "$DEMO/bypass.yaml"; then
  echo "lint accepted a drafted case approved without the gate" >&2; exit 1
fi
for r in p5c p5p p5w p5a; do $TK verify "out/r$STAMP-$r"; done
echo "PHASE5 ACCEPTANCE: PASS (reports: out/r$STAMP-p5c, -p5p, -p5w, -p5a)"
