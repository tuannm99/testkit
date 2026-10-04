#!/bin/sh
# Phase 1 acceptance: a demo run produces Grafana images + raw data for the
# exact time window, with run_id on metrics and logs and annotations on the
# dashboards. Requires: testkit up --services order-worker --profile core,stores,mocks,observability
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
TK=${TK:-./bin/testkit}
RUN_ID=${RUN_ID:-r$(date -u +%Y%m%d-%H%M%S)-p1}
NS=tk_$(echo "$RUN_ID" | tr -c 'a-z0-9\n' '_')
PG="docker exec -i testkit-postgres-1 psql -U testkit -v ON_ERROR_STOP=1 -q"
KAFKA="docker exec -i testkit-kafka-1 /opt/kafka/bin"
MOCK=http://127.0.0.1:58081
echo "run_id=$RUN_ID ns=$NS"
trap 'docker rm -f "tk-$NS-worker" >/dev/null 2>&1 || true' EXIT

# --- namespace: database, topics, index, mock ---------------------------------
$PG -d testkit -c "CREATE DATABASE $NS"
$PG -d "$NS" < reference-worker/migrations/postgres/001_init.sql
for t in orders orders.dlq order-events; do
  $KAFKA/kafka-topics.sh --bootstrap-server localhost:9092 --create --topic "$NS.$t" --partitions 3 >/dev/null
done
curl -sf -XPUT "127.0.0.1:59200/$NS-orders" -H 'content-type: application/json' -d @reference-worker/es/orders.json >/dev/null
curl -sf -XPUT "$MOCK/_mock/ns/$NS/mocks/payment" -d '{"kind":"http","openapi":"payment.openapi.yaml","api_version":"2024-06-01","verified_at":"2026-10-01","idempotency_header":"Idempotency-Key"}' >/dev/null
curl -sf -XPUT "$MOCK/_mock/ns/$NS/mocks/payment/script" -d '{"rules":[{"match":{"operation":"createCharge"},"responses":[{"status":201,"body":{"id":"ch_${req.body.order_id}","status":"succeeded"},"delay":{"dist":"normal","mean":"40ms","stddev":"15ms","min":"5ms"}}]}]}' >/dev/null

# --- service under test with run labels -----------------------------------------
docker run -d --name "tk-$NS-worker" --network testkit_net \
  --label testkit.managed=true --label testkit.project=testkit \
  --label "testkit.run_id=$RUN_ID" --label "testkit.ns=$NS" --label testkit.service=order-worker \
  --label testkit.metrics=true --label testkit.metrics_port=8080 --label testkit.metrics_path=/metrics \
  -e "TESTKIT_RUN_ID=$RUN_ID" -e "DATABASE_URL=postgres://testkit:testkit@postgres:5432/$NS?sslmode=disable" \
  -e KAFKA_BROKERS=kafka:9092 -e "KAFKA_TOPIC=$NS.orders" -e "KAFKA_GROUP=$NS.order-worker" \
  -e "KAFKA_DLQ_TOPIC=$NS.orders.dlq" -e "OUTBOX_TOPIC=$NS.order-events" \
  -e "PAYMENT_URL=http://mockhub:8081/ns/$NS/payment" -e ES_URL=http://elasticsearch:9200 -e "ES_INDEX=$NS-orders" \
  -e SMTP_ADDR=mailpit:1025 -e PAYMENT_MAX_BACKOFF=1s testkit/reference-worker:dev >/dev/null
for i in $(seq 1 60); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' "tk-$NS-worker")" = healthy ] && break; sleep 1
done

wait_paid() { # $1 = expected number of paid orders
  for i in $(seq 1 120); do
    n=$($PG -d "$NS" -tAc "select count(*) from orders where status='paid'")
    [ "$n" -ge "$1" ] && return 0; sleep 1
  done
  echo "timeout waiting for $1 paid orders (have $n)"; return 1
}
enqueue() { # $1 = first id, $2 = last id
  $PG -d "$NS" -c "INSERT INTO orders(id,customer_email,amount_cents,currency) SELECT 'o'||g, 'c'||g||'@shop.test', 100+g, 'USD' FROM generate_series($1,$2) g"
  $PG -d "$NS" -c "INSERT INTO jobs(job_key,order_id) SELECT 'j-o'||g, 'o'||g FROM generate_series($1,$2) g WHERE g % 2 = 0"
  seq "$1" "$2" | awk '$1 % 2 == 1 {printf "{\"job_id\":\"j-o%d\",\"order_id\":\"o%d\"}\n", $1, $1}' | \
    $KAFKA/kafka-console-producer.sh --bootstrap-server localhost:9092 --topic "$NS.orders" >/dev/null
}

FROM=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 4   # a few idle scrapes before load so the panels show the ramp
$TK annotate --run-id "$RUN_ID" --text "step 1: enqueue 300 jobs (kafka + db-poll)" --tags step
S1=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
enqueue 1 300
wait_paid 300
$TK annotate --run-id "$RUN_ID" --time "$S1" --end now --text "step 1 done: 300 orders paid" --tags step

$TK annotate --run-id "$RUN_ID" --text "fault injected: payment API answers 500, then 429 (Retry-After 1s), then 201" --tags fault
curl -sf -XPUT "$MOCK/_mock/ns/$NS/mocks/payment/script" -d '{"rules":[{"match":{"operation":"createCharge"},"responses":[{"status":500},{"status":429,"retry_after":"1"},{"status":201,"body":{"id":"ch_${req.body.order_id}","status":"succeeded"}}]}]}' >/dev/null
S2=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
enqueue 301 400
wait_paid 400
$TK annotate --run-id "$RUN_ID" --time "$S2" --end now --text "fault removed: 400 orders paid" --tags fault
sleep 6   # let the last scrapes land
TO=$(date -u +%Y-%m-%dT%H:%M:%SZ)

OUT="out/$RUN_ID/demo"
$TK collect --run-id "$RUN_ID" --service order-worker --from "$FROM" --to "$TO" --out "$OUT"

# --- checks ------------------------------------------------------------------
python3 - "$OUT" "$FROM" "$TO" "$RUN_ID" <<'EOF'
import json, sys, os, datetime
out, frm, to, run = sys.argv[1:]
ts = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
t0, t1 = ts(frm), ts(to)
panels = json.load(open(os.path.join(out, "grafana", "panels.json")))
bad = []
for p in panels:
    img = os.path.join(out, "grafana", p["image"]) if p.get("image") else None
    if not img or os.path.getsize(img) < 5000 or open(img, "rb").read(4) != b"\x89PNG":
        bad.append(f"{p['name']}: missing/blank image {p.get('image_error')}")
    if f"from={int(t0*1000)}" not in p["link"] or f"to={int(t1*1000)}" not in p["link"]:
        bad.append(f"{p['name']}: link not locked to the window")
    for q in p["queries"]:
        if q.get("error"):
            bad.append(f"{p['name']}: {q['error']}"); continue
        raw = json.load(open(os.path.join(out, "grafana", q["raw"])))
        series = raw["data"]["result"]
        pts = [float(v[0]) for s in series for v in s["values"]]
        if not pts:
            bad.append(f"{p['name']}.{q['ref_id']}: no data"); continue
        if min(pts) < t0 - 0.001 or max(pts) > t1 + 0.001:
            bad.append(f"{p['name']}: samples outside window")
        if p["dashboard"] == "tk-order-worker" and any(s["metric"].get("run_id", run) != run for s in series):
            bad.append(f"{p['name']}: foreign run_id")
    print(f"  {p['name']:<17} {p['title'][:48]:<48} image={'ok' if img and os.path.exists(img) else 'NO'} "
          f"points={sum(st['points'] for q in p['queries'] for st in (q.get('stats') or []))}")
logs = [json.loads(l) for l in open(os.path.join(out, "logs", "loki.jsonl"))]
if not logs or any(l["labels"].get("run_id") != run for l in logs):
    bad.append("loki: no logs or foreign run_id")
print(f"  loki logs with run_id={run}: {len(logs)}")
if bad:
    print("FAIL:\n  " + "\n  ".join(bad)); sys.exit(1)
EOF
ANN=$(curl -s -u admin:testkit "127.0.0.1:53000/api/annotations?tags=testkit&tags=$RUN_ID&limit=100" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
echo "  grafana annotations tagged $RUN_ID: $ANN"
[ "$ANN" -ge 4 ] || { echo "FAIL: annotations missing"; exit 1; }
M=$(curl -s "127.0.0.1:59090/api/v1/query" --data-urlencode "query=count(worker_jobs_total{run_id=\"$RUN_ID\"})" | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)')
echo "  prometheus series with run_id=$RUN_ID (worker_jobs_total): $M"
[ "$M" -gt 0 ] || { echo "FAIL: no metrics labelled with run_id"; exit 1; }

docker rm -f "tk-$NS-worker" >/dev/null
echo "PHASE1 ACCEPTANCE: PASS (evidence in $OUT)"
