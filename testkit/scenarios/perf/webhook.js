// k6 load script: signed refund webhooks to the service under test.
// TestKit wraps it with open-model scenarios (see the case's perf.stages).
import http from 'k6/http';
import crypto from 'k6/crypto';
import exec from 'k6/execution';
import { check } from 'k6';

const target = __ENV.TARGET;
const secret = __ENV.WEBHOOK_SECRET;
const orders = parseInt(__ENV.ORDERS || '100', 10);
const rep = __ENV.TESTKIT_REP || '1'; // repetition number: event ids stay unique across repetitions

export default function () {
  const n = exec.scenario.iterationInTest;
  const order = `W${(n % orders) + 1}`;
  const body = JSON.stringify({
    id: `evt-${rep}-${exec.scenario.name}-${n}`,
    type: 'charge.refunded',
    api_version: '2024-06-01',
    seq: n + 1,
    data: { order_id: order },
  });
  const sig = 'sha256=' + crypto.hmac('sha256', secret, body, 'hex');
  const res = http.post(`${target}/webhooks/payment`, body, {
    headers: { 'Content-Type': 'application/json', 'X-Signature': sig },
  });
  check(res, { 'status is 200': (r) => r.status === 200 });
}
