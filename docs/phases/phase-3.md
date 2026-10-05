# Phase 3 — Postgres, Kafka, DB-poll and HTTP/webhook adapters; worker semantics

## Plan
1. Complete the adapters started in Phase 2: Mock Hub in-flight journal and `succeeded`/`in_flight`
   checks, webhook connector step (`webhook.send`: valid/invalid/missing signature, duplicates, delay),
   SUT lifecycle steps (`sut.kill/stop/start/restart/pause`) with stable health checks, `sut.metric`,
   `sut.log`, replicas (competing consumers), `assert.during`.
2. Cross-trigger parity in the run model, CLI and report.
3. Shared trigger contract tests (Kafka, DB poll).
4. Reference worker: payment webhook endpoint (HMAC, idempotent by event id, out-of-order safe,
   payload version check), Kafka session timeout config, `poll_busy_loop` failpoint.
5. Scenarios covering the semantics required by §5.6 and acceptance script.

## Scenarios (all in `testkit/scenarios/`)
| Case | Trigger | What it proves |
|------|---------|----------------|
| TC-ORDER-017 | kafka, db-poll | retry on 500/429 + Retry-After, no extra call, one mail, duplicates |
| TC-ORDER-018 | kafka, db-poll | idempotency: late job for a paid order → no charge, no mail, no event |
| TC-ORDER-020 | kafka, db-poll | crash right after DB commit (failpoint) → redelivery completes ES/mail/outbox once, no second charge |
| TC-ORDER-021 | kafka, db-poll | SIGKILL while the payment call hangs → redelivery, same Idempotency-Key, one successful charge |
| TC-ORDER-022 | kafka | poison record → DLQ, job behind it (same partition) processed, offsets committed |
| TC-ORDER-023 | kafka, db-poll | SIGTERM mid-job → in-flight job finished and acked before exit, not reprocessed |
| TC-JOBS-001 | db-poll ×3 replicas | 30 jobs, 3 pollers: no duplicate, no loss (claims = 30) |
| TC-JOBS-002 | db-poll | empty table: backoff, no busy loop (mutation `poll_busy_loop` must go red) |
| TC-JOBS-003 | db-poll | always-failing job stops at max_attempts → dead, order untouched |
| TC-JOBS-004 | db-poll | job not processed before `run_at` (DB clock), processed once after |
| TC-WEBHOOK-001 | — | webhook: bad signature 401, duplicate applied once, out-of-order ignored, old payload 422 |

## Verification
`scripts/acceptance/phase3.sh` (log `out/acceptance/phase3.log`):
```
--- trigger contract tests (same suite for every trigger)
ok  github.com/tuannm99/testkit/testkit/adapters/trigger/contract  1.851s
lint: 11 case(s) OK
run r20261004-185616-p3: 16 pass, 0 fail, 0 error, 0 skipped in 1m38s   (--parallel 4)
  parity  TC-ORDER-017  kafka vs db-poll: match
  parity  TC-ORDER-018  kafka vs db-poll: match
  parity  TC-ORDER-020  kafka vs db-poll: match
  parity  TC-ORDER-021  kafka vs db-poll: match
  parity  TC-ORDER-023  kafka vs db-poll: match
PHASE3 ACCEPTANCE: PASS
```
Evidence that the SIGKILL really happened mid-call (TC-ORDER-021, journal):
```
kafka    1 18:55:02.169 status 0   key order-o4 | client gave up during delay (connection closed)
         2 18:55:11.789 status 201 key order-o4          (redelivered after the 10 s session timeout)
db-poll  1 18:55:03.846 status 0   key order-o4 | client gave up during delay (connection closed)
         2 18:55:14.325 status 201 key order-o4          (job lease 6 s + order claim lease 10 s)
```

## Problems found and fixed on the way (root causes, no assertion relaxed)
- First run of TC-ORDER-021/023: the health check after `docker start` used the old published port
  → environment errors. Fixed in the SUT connector (port resolved on every check).
- TC-ORDER-021 passed for the wrong reason: the hub journaled a call only after answering, so
  `wait.until calls ≥ 1` waited for the end of the 3 s delay and the kill came after the payment.
  Fixed in the Mock Hub (journal at arrival, `in_flight`), scenario waits on `in_flight = 1`.
- After SIGKILL, Kafka redelivery waited 45 s (default session timeout); made configurable in the worker.

## Risks / open points
- Kafka rebalance with several consumers and broker connection loss are exercised in Phase 5 (chaos).
- "Wrong clock" (worker clock vs DB clock) cannot be observed without clock skew between containers;
  the `use_worker_clock` failpoint exists but no case uses it yet (needs libfaketime in the test image).
