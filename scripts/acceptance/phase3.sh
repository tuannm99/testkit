#!/bin/sh
# Phase 3 acceptance: the same scenarios run through both triggers with
# matching results; crash mid-way (after DB commit, SIGKILL mid-call, SIGTERM)
# and idempotency (duplicate deliveries, late job for a paid order, webhooks)
# pass; Kafka- and DB-poll-specific semantics pass; trigger contract tests pass.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p3}

echo "--- trigger contract tests (same suite for every trigger)"
(cd testkit && TESTKIT_INTEGRATION=1 GOTOOLCHAIN=local go test -count=1 ./adapters/trigger/contract/)
echo "--- scenarios"
$TK lint testkit/scenarios
$TK run --run-id "$RUN_ID" --parallel 4 testkit/scenarios/order testkit/scenarios/jobs testkit/scenarios/webhook
python3 - "out/$RUN_ID" <<'PY'
import json, sys
run = json.load(open(sys.argv[1] + "/run.json"))
execs = [e for e in run["executions"] if not e.get("mutation")]
bad = [e["id"] for e in execs if e["result"] != "pass"]
assert not bad, f"not passing: {bad}"
multi = {e["case_id"] for e in execs if e.get("trigger")} 
par = {p["case_id"]: p for p in run.get("trigger_parity", [])}
both = sorted(c for c in multi if sum(1 for e in execs if e["case_id"] == c) > 1)
for c in both:
    assert c in par and par[c]["match"], f"parity {c}: {par.get(c)}"
print(f"  {len(execs)} executions pass; trigger parity matches for {', '.join(both)}")
PY
echo "PHASE3 ACCEPTANCE: PASS (report: out/$RUN_ID/report.html)"
