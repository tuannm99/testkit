#!/bin/sh
# Phase 8 acceptance: queue triggers. The same cases give the same result through
# Kafka, the DB job table, RabbitMQ and Redis (trigger parity); a poison message
# is dead-lettered on RabbitMQ and on a Redis stream, and the mutation that drops
# dead letters is killed; every trigger passes the connector contract tests.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p8}
(cd testkit && GOTOOLCHAIN=local go vet ./... && TESTKIT_INTEGRATION=1 GOTOOLCHAIN=local go test -count=1 ./adapters/trigger/contract/)
(cd reference-worker && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go vet -tags failpoint ./...)
$TK lint testkit/scenarios
$TK run --run-id "$RUN_ID" --mutations \
  testkit/scenarios/order/TC-ORDER-017.yaml testkit/scenarios/order/TC-ORDER-018.yaml \
  testkit/scenarios/drafts/TC-QUEUE-001.yaml testkit/scenarios/drafts/TC-QUEUE-002.yaml || true
python3 - "out/$RUN_ID" <<'PY'
import json, sys
run = json.load(open(sys.argv[1] + "/run.json"))
base = [e for e in run["executions"] if not e.get("mutation")]
bad = [e["id"] for e in base if e["result"] != "pass"]
assert not bad, f"not passing without mutation: {bad}"
triggers = {e["case_id"]: set() for e in base}
for e in base:
    triggers[e["case_id"]].add(e["trigger"])
for cid in ("TC-ORDER-017", "TC-ORDER-018"):
    assert triggers[cid] == {"kafka", "db-poll", "rabbitmq", "redis"}, (cid, triggers[cid])
killed = {(e["case_id"], m["failpoint"]) for e in base for m in e.get("mutations", []) if m["killed"]}
for want in (("TC-QUEUE-001", "drop_dead_letters"), ("TC-QUEUE-002", "drop_dead_letters")):
    assert want in killed, f"mutation not killed: {want}"
print(f"  {len(base)} executions pass; triggers agree; dead-letter mutations killed")
PY
echo "PHASE8 ACCEPTANCE: PASS (report: out/$RUN_ID/report.html)"
