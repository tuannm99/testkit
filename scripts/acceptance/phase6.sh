#!/bin/sh
# Phase 6 acceptance (Docker only):
#  1. the release suite runs approved cases (UI test included) with mutations
#     and retries, the gate says GO, the bundle is packed to out/<run_id>.zip
#     with the Zephyr Scale import files and no secret inside;
#  2. a suite with a case that does not catch its defect is NO-GO (exit 1);
#  3. the QC handover files (CSV + Markdown) match the run; the Zephyr Scale
#     port (TK_QC_TOOL=zephyr-scale) exports and uploads to a fake API;
#  4. pack refuses a tampered bundle.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
STAMP=$(date -u +%Y%m%d-%H%M%S)
REL=${REL_RUN:-r$STAMP-p6}

(cd testkit && GOTOOLCHAIN=local go test -count=1 ./core/evidence/ ./core/orchestrator/ ./adapters/qc/... ./core/report/)

echo "== 1. release suite"
if [ -z "${REL_RUN:-}" ]; then
  $TK run --run-id "$REL" testkit/suites/release.yaml
fi
test -f "out/$REL.zip"
unzip -tq "out/$REL.zip"
python3 - "out/$REL" "out/$REL.zip" <<'PY'
import json, sys, zipfile
run = json.load(open(sys.argv[1] + "/run.json"))
g = run["gate"]
print(f"  gate {g['decision']}")
for r in g["rules"]:
    print(f"    {'ok  ' if r['passed'] else 'FAIL'} {r['name']}: {r['detail'][:150]}")
assert g["decision"] == "GO", g
base = [e for e in run["executions"] if not e.get("mutation")]
ui = [e for e in base if e["case_id"] == "TC-UI-001"]
assert ui and ui[0]["result"] == "pass", "UI case missing or red"
muts = [m for e in base for m in e.get("mutations", [])]
assert muts and all(m["killed"] for m in muts), "a mutation survived"
z = zipfile.ZipFile(sys.argv[2])
names = z.namelist()
root = names[0].split("/")[0]
import csv, io
def rows(name):
    return list(csv.DictReader(io.TextIOWrapper(z.open(f"{root}/qc/{name}"), encoding="utf-8-sig")))
res, tc = rows("results.csv"), rows("testcases.csv")
assert len(res) == len(base), (len(res), len(base))
for r in res:
    e = next(e for e in base if e["case_id"] == r["Case ID"] and (e.get("trigger") or "") == r["Trigger"])
    assert r["Result"] == e["result"], (r, e["result"])
case_ids = {e["case_id"] for e in base}
assert {r["Case ID"] for r in tc} == case_ids, "testcases.csv does not list exactly the run's cases"
md = z.read(f"{root}/qc/results.md").decode()
assert "Cổng release: GO" in md, "results.md lacks the gate"
assert any(n.endswith(".png") and "/output/ui/" in n for n in names), "no UI screenshot in the bundle"
print(f"  {len(base)} executions, {len(muts)} mutations killed; qc/results.csv {len(res)} rows, qc/testcases.csv {len(tc)} step rows; {len(names)} files in the zip")
PY
for s in $(sed -n 's/^TK_[A-Z_]*PASSWORD=//p' infra/compose/testkit.env) test-only-webhook-secret; do
  if unzip -p "out/$REL.zip" | grep -aq -- "$s"; then echo "secret found in the bundle" >&2; exit 1; fi
done
echo "  no declared secret in the zip"

echo "== 2. NO-GO suite"
if $TK run --run-id "r$STAMP-p6n" scripts/acceptance/fixtures/phase6/nogo.yaml; then
  echo "NO-GO suite returned GO" >&2; exit 1
fi
python3 -c "import json,sys; g=json.load(open('out/r$STAMP-p6n/run.json'))['gate']; assert g['decision']=='NO-GO'; print('  gate', g['decision'], [r['name'] for r in g['rules'] if not r['passed']])"

echo "== 3. Zephyr Scale port (on a copy of the bundle)"
if $TK qc push "out/$REL" 2>/dev/null; then echo "push accepted with qc.tool: files" >&2; exit 1; fi
Z="out/r$STAMP-p6-zephyr-copy"
cp -r "out/$REL" "$Z"
TK_QC_TOOL=files,zephyr-scale $TK qc export "$Z"
$TK verify "$Z"
python3 scripts/acceptance/fake_zephyr.py 18099 "out/r$STAMP-p6-zephyr.json" &
sleep 1
TK_QC_TOOL=zephyr-scale ZEPHYR_TOKEN=fake-token-for-acceptance TK_QC_API=http://127.0.0.1:18099/v2 $TK qc push "$Z"
wait
rm -rf "$Z"
python3 - "out/r$STAMP-p6-zephyr.json" "$REL" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
assert r["path"].startswith("/v2/automations/executions/custom?") and "projectKey=ORD" in r["path"], r["path"]
assert r["auth"] == "Bearer ", r["auth"]
assert sys.argv[2] in r["testCycle"]["name"] and r["executions"]["version"] == 1
print(f"  uploaded {len(r['executions']['executions'])} executions to test cycle {r['testCycle']['name']!r}")
PY

echo "== 4. tampered bundle"
T="out/r$STAMP-p6-tamper"
cp -r "out/$REL" "$T"
echo "edited" >> "$T/report.html"
if $TK pack "$T"; then echo "tampered bundle packed" >&2; exit 1; fi
rm -rf "$T"
$TK verify "out/$REL"
echo "PHASE6 ACCEPTANCE: PASS (bundle: out/$REL.zip)"
