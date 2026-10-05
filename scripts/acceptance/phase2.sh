#!/bin/sh
# Phase 2 acceptance: one P0 case runs end to end and produces a bundle a QC
# reader can follow: report.html (sections in reading order, why it passed with
# evidence links), junit.xml, traceability.csv, manifest.json (verifiable).
# Requires: testkit up --services order-worker --profile core,stores,mocks,observability
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
CASE=testkit/scenarios/order/TC-ORDER-017.yaml
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p2}

$TK lint "$CASE"
$TK plan "$CASE" > "out/plan-$RUN_ID.txt"
echo "plan: $(grep -c '^===' "out/plan-$RUN_ID.txt") execution(s) planned"
$TK run --run-id "$RUN_ID" "$CASE"
OUT=out/$RUN_ID
$TK verify "$OUT"
for f in report.html junit.xml traceability.csv manifest.json run.json; do
  test -s "$OUT/$f" || { echo "FAIL: missing $f"; exit 1; }
done
python3 - "$OUT" <<'PY'
import json, os, sys, re
out = sys.argv[1]
html = open(os.path.join(out, "report.html"), encoding="utf-8").read()
order = ["1. Mục đích", "2. Đầu vào", "3. Các bước", "4. Assertion", "5. Đầu ra", "6. Grafana", "7. Kết luận", "8. Bằng chứng phản chứng"]
pos = [html.find(s) for s in order]
assert all(p > 0 for p in pos) and pos == sorted(pos), f"sections missing or out of order: {pos}"
run = json.load(open(os.path.join(out, "run.json")))
for ex in run["executions"]:
    assert ex["result"] == "pass", ex["id"]
    for a in ex["assertions"]:
        for k in ("id", "result", "check", "operator", "expected", "actual", "why", "evidence", "observed_at"):
            assert k in a, (ex["id"], a["id"], k)
        for e in a["evidence"]:
            assert os.path.exists(os.path.join(out, e)), e
    for s in ex["conclusion"][1:]:
        assert s["evidence"], s["text"]
    for p in ex["grafana"]:
        assert p.get("image") and os.path.exists(os.path.join(out, p["image"])), p
        for r in p["raw"]:
            assert os.path.exists(os.path.join(out, r)), r
    print(f"  {ex['id']}: {len(ex['assertions'])} assertions with evidence, {len(ex['grafana'])} panels, {len(ex['artifacts'])} artifacts")
man = json.load(open(os.path.join(out, "manifest.json")))
assert man["git"]["commit"] and man["images"] and man["files"], "manifest incomplete"
print(f"  manifest: commit {man['git']['commit'][:12]}, {len(man['images'])} pinned images, {len(man['files'])} hashed files")
PY
echo "PHASE2 ACCEPTANCE: PASS (report: $OUT/report.html)"
