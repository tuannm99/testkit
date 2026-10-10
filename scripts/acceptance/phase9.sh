#!/bin/sh
# Phase 9 acceptance: the standard case pack. The pack is generated from the service
# descriptor (gen --check clean), every case is green through every trigger, triggers agree,
# and every applicable mutation turns its case red (duplicate delivery, poison message,
# crash, out of order, dependency faults). Needs the stack with the chaos profile:
#   testkit up --services order-worker --profile core,stores,mocks,chaos
# LOAD=1 also records baselines and runs the steady-load cases (about 40 minutes, idle machine).
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p9}
SVC=order-worker
PACK=testkit/scenarios/generated/$SVC

(cd testkit && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test -count=1 ./core/... \
  && TESTKIT_INTEGRATION=1 GOTOOLCHAIN=local go test -count=1 ./adapters/trigger/contract/)
(cd reference-worker && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go vet -tags failpoint ./...)
$TK lint testkit/scenarios
$TK gen --check

$TK run --mutations --parallel 3 --run-id "$RUN_ID-func" \
  $PACK/duplicate-delivery.yaml $PACK/poison-message-*.yaml $PACK/crash-mid-job.yaml $PACK/out-of-order.yaml || true
$TK run --mutations --run-id "$RUN_ID-fault" $PACK/dependency-fault-*.yaml || true

python3 - "out/$RUN_ID-func" "out/$RUN_ID-fault" <<'PY'
import json, sys
TRIGGERS = {"kafka", "db-poll", "rabbitmq", "redis"}
ids = set(); killed = set(); total = 0
for d in sys.argv[1:]:
    run = json.load(open(d + "/run.json"))
    base = [e for e in run["executions"] if not e.get("mutation")]
    bad = [e["id"] + ":" + e["result"] for e in base if e["result"] != "pass"]
    assert not bad, f"not green without mutation in {d}: {bad}"
    bad = [p["case_id"] for p in run.get("parity", []) if not p["match"]]
    assert not bad, f"triggers disagree in {d}: {bad}"
    for e in base:
        ids.add((e["case_id"], e["trigger"]))
        for m in e.get("mutations", []):
            total += 1
            assert m["killed"], f"mutation survived: {e['id']} {m['id']} {m['failpoint']}"
            killed.add((e["case_id"], m["failpoint"], e["trigger"]))
cases = {c for c, _ in ids}
for c in ("DUP", "CRASH", "ORDER", "POISON-KAFKA", "POISON-DB-POLL", "POISON-RABBITMQ", "POISON-REDIS",
          "FAULT-POSTGRES-DOWN", "FAULT-PAYMENT-RESET-PEER"):
    assert "TC-STD-ORDER-WORKER-" + c in cases, f"case not run: {c}"
for c in ("DUP", "CRASH", "ORDER", "FAULT-POSTGRES-DOWN", "FAULT-PAYMENT-RESET-PEER"):
    trig = {t for cc, t in ids if cc == "TC-STD-ORDER-WORKER-" + c}
    assert trig == TRIGGERS, f"{c} not run through every trigger: {trig}"
print(f"  {len(ids)} executions green; triggers agree; {total} mutation runs, all killed")
PY

if [ "${LOAD:-0}" = 1 ]; then
  for t in kafka db-poll rabbitmq redis; do $TK baseline record "$PACK/steady-load-$t.yaml" --runs 5; done
  $TK run --run-id "$RUN_ID-load" $PACK/steady-load-*.yaml
fi
echo "PHASE9 ACCEPTANCE: PASS (reports: out/$RUN_ID-func/report.html, out/$RUN_ID-fault/report.html)"
