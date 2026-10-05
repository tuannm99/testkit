#!/bin/sh
# Phase 7 acceptance (no model needed; set TK_AI_REAL_COMMAND='claude -p' to add a real model run):
#  1. what would be sent is redacted: no declared secret, e-mail or card number;
#  2. provider none sends nothing and leaves the redacted prompt for manual use; the bundle stays sealed;
#  3. an answer obtained by hand (--response) becomes ai/triage.{json,md}, invalid citations are dropped,
#     the report shows the advisory section, the bundle verifies and packs (secret scan included);
#  4. an OpenAI-compatible endpoint (stand-in) writes the summary; the request it received is redacted;
#  5. a drafted case is forced to status draft, REQ-TBD, no owner/admission, and passes lint;
#  6. AI output never changes run.json (results and gate).
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
STAMP=$(date -u +%Y%m%d-%H%M%S)
TMP="out/r$STAMP-p7"
mkdir -p "$TMP"

(cd testkit && GOTOOLCHAIN=local go test -count=1 ./core/ai/ ./adapters/ai/... ./core/report/ ./core/orchestrator/)

# Bundles to work on (copies): a NO-GO run with a weak case and a GO release run.
WEAK=${WEAK_RUN:-$(ls -d out/r*-p6n | tail -1)}
GO=${GO_RUN:-$(ls -d out/r*-p6 | tail -1)}
cp -r "$WEAK" "$TMP/weak"; cp -r "$GO" "$TMP/go"
$TK verify "$TMP/weak" >/dev/null; $TK verify "$TMP/go" >/dev/null
cp "$TMP/weak/run.json" "$TMP/weak.run.json.before"; cp "$TMP/go/run.json" "$TMP/go.run.json.before"

echo "== 1. redacted payload"
$TK ai context "$TMP/go" --task summary > "$TMP/ctx-summary.txt"
$TK ai context "$TMP/weak" --task triage > "$TMP/ctx-triage.txt"
for s in $(sed -n 's/^TK_[A-Z_]*PASSWORD=//p' infra/compose/testkit.env) test-only-webhook-secret; do
  if grep -q -- "$s" "$TMP"/ctx-*.txt; then echo "secret in payload" >&2; exit 1; fi
done
if sed '1,/^=== prompt/d' "$TMP"/ctx-*.txt | grep -Eiq '[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}'; then echo "e-mail in payload" >&2; exit 1; fi
echo "  $(tail -1 "$TMP/ctx-triage.txt")"

echo "== 2. provider none: nothing sent"
if TK_AI_PROVIDER=none $TK ai triage "$TMP/weak" > "$TMP/none.log" 2>&1; then echo "none provider produced an answer" >&2; exit 1; fi
grep -q "redacted prompt is in" "$TMP/none.log"
ls "$TMP"/weak/ai/requests/01-triage.json >/dev/null
$TK verify "$TMP/weak"

echo "== 3. manual answer"
EX=$(python3 -c "import json,sys;r=json.load(open('$TMP/weak/run.json'));print([e['id'] for e in r['executions'] if not e.get('mutation')][0])")
cat > "$TMP/answer.json" <<JSON
{"items":[{"execution":"$EX","category":"test-weakness","agrees_with_rule_class":true,
 "summary":"Mutation sống sót: case không kiểm Idempotency-Key.","reasoning":"M1 pass.",
 "evidence":["$(python3 -c "import json;r=json.load(open('$TMP/weak/run.json'));print([e['dir'] for e in r['executions'] if not e.get('mutation')][0])")/case.json","does/not/exist.json"],
 "next_steps":["Thêm kiểm tra mock.payment.idempotency_keys"],"confidence":"high"}],"overall":"o"}
JSON
TK_AI_PROVIDER=none $TK ai triage "$TMP/weak" --response "$TMP/answer.json"
python3 - "$TMP/weak/ai/triage.json" <<'PY'
import json, sys
t = json.load(open(sys.argv[1]))
it = t["items"][0]
assert it["category"] == "test-weakness" and len(it["evidence"]) == 1 and it["dropped_citations"] == ["does/not/exist.json"], it
print("  triage:", it["execution"], it["category"], "evidence", it["evidence"], "dropped", it["dropped_citations"])
PY
grep -q "Gợi ý của AI (tham khảo)" "$TMP/weak/report.html"
$TK verify "$TMP/weak"
$TK pack "$TMP/weak" | tail -1

echo "== 4. OpenAI-compatible endpoint (stand-in)"
printf '### Tóm tắt\nQuyết định gate: GO.\n\n### Rủi ro còn lại\n- REQ-283 chưa kiểm chứng.\n\n### Đề xuất\n- Chạy TC-CHAOS-004 trên máy có netem.\n' > "$TMP/summary-answer.md"
python3 scripts/acceptance/fake_chat.py 18098 "$TMP/chat-request.json" "$TMP/summary-answer.md" &
sleep 1
TK_AI_PROVIDER=openai-compatible TK_AI_BASE_URL=http://127.0.0.1:18098/v1 TK_AI_MODEL=any-model $TK ai summary "$TMP/go"
wait
python3 - "$TMP/chat-request.json" "$TMP/go/ai/summary.md" <<'PY'
import json, re, sys
r = json.load(open(sys.argv[1]))
assert r["path"] == "/v1/chat/completions" and r["body"]["model"] == "any-model", r["path"]
sent = r["body"]["messages"][1]["content"]
assert not re.search(r"[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}", sent, re.I), "e-mail sent"
md = open(sys.argv[2]).read()
assert "gate decision: GO" in md and "Cảnh báo" not in md, md[:400]
print("  summary written; request redacted; facts block present")
PY
$TK verify "$TMP/go"

echo "== 5. drafted case"
cat > "$TMP/draft-answer.md" <<'YAML'
```yaml
id: TC-SOMETHING-ELSE
title: Draft từ AI (kiểm thử)
requirement: REQ-INVENTED
risk: P1
status: approved
owner: someone
service: order-worker
purpose: >
  Đơn được thanh toán khi cổng trả 201.
preconditions:
  - Đơn z1 pending; cổng thanh toán (mock) trả 201
input:
  order: { id: z1, customer_email: "cust-z1+{{ .ns }}@shop.test", amount_cents: 1000, currency: USD, status: pending }
trigger: [kafka]
given:
  postgres.order: ["{{ .input.order }}"]
  mock.payment: [201]
  job: { id: z1 }
expect:
  - { id: A1, check: postgres.order.z1.status, eq: paid, why: "Đơn phải paid" }
evidence:
  grafana: [payment_calls_total]
within: 30s
admission: { run_id: fake, by: ai }
mutations:
  - { id: M1, failpoint: no_retry, title: "x", expect_red: [A1] }
```
YAML
$TK ai draft --service order-worker --id TC-DRAFT-900 --requirement "đơn được thanh toán" --out "$TMP/drafts" --response "$TMP/draft-answer.md"
D="$TMP/drafts/TC-DRAFT-900.yaml"
grep -q '^id: TC-DRAFT-900$' "$D"; grep -q '^status: draft$' "$D"; grep -q '^requirement: REQ-TBD$' "$D"
if grep -Eq '^(owner|admission|qc_key):' "$D"; then echo "draft kept owner/admission" >&2; exit 1; fi
$TK lint "$D"

echo "== 6. results untouched"
cmp "$TMP/weak/run.json" "$TMP/weak.run.json.before"
cmp "$TMP/go/run.json" "$TMP/go.run.json.before"
echo "  run.json identical before/after the AI tasks"

if [ -n "${TK_AI_REAL_COMMAND:-}" ]; then
  echo "== 7. real model ($TK_AI_REAL_COMMAND)"
  cp -r "$WEAK" "$TMP/real"
  TK_AI_PROVIDER=command TK_AI_COMMAND="$TK_AI_REAL_COMMAND" $TK ai triage "$TMP/real"
  $TK verify "$TMP/real"
fi
echo "PHASE7 ACCEPTANCE: PASS ($TMP)"
