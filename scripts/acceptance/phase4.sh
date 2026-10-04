#!/bin/sh
# Phase 4 acceptance: ES partial _bulk failure and ClickHouse retry duplicates
# are detected when the system is broken on purpose (mutation runs go red),
# and pass when it is not. Also runs the mail, cache and WebSocket cases.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p4}
(cd testkit && GOTOOLCHAIN=local go test -count=1 ./adapters/mock/smtp/ ./adapters/mock/socket/)
$TK lint testkit/scenarios
$TK run --run-id "$RUN_ID" --parallel 4 --mutations \
  testkit/scenarios/search testkit/scenarios/mail testkit/scenarios/cache testkit/scenarios/notify || true
python3 - "out/$RUN_ID" <<'PY'
import json, sys
run = json.load(open(sys.argv[1] + "/run.json"))
base = [e for e in run["executions"] if not e.get("mutation")]
bad = [e["id"] for e in base if e["result"] != "pass"]
assert not bad, f"not passing without mutation: {bad}"
total = killed = 0
for e in base:
    for m in e.get("mutations", []):
        total += 1
        killed += m["killed"]
        print(f"  {e['id']:<26} {m['id']} {m['failpoint']:<22} -> case {m['result'].upper():<5} red={m['red_assertions']} {'KILLED' if m['killed'] else 'SURVIVED'}")
assert total and killed == total, f"{total - killed} mutation(s) survived"
must = {("TC-SEARCH-001", "es_ignore_bulk_errors"), ("TC-SEARCH-001", "ch_no_dedup_token"), ("TC-MAIL-001", "ch_no_dedup_token")}
seen = {(e["case_id"], m["failpoint"]) for e in base for m in e.get("mutations", []) if m["killed"]}
assert must <= seen, f"missing: {must - seen}"
print(f"  {len(base)} executions pass; {killed}/{total} mutations killed")
PY
echo "PHASE4 ACCEPTANCE: PASS (report: out/$RUN_ID/report.html)"
