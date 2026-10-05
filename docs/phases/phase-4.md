# Phase 4 — Elasticsearch, ClickHouse, Redis, Mongo, reconcile, SMTP faults, socket mock

## Plan
1. Reference worker: idempotent side effects keyed by event id — ClickHouse insert with
   `insert_deduplication_token`, Mongo audit (E11000 = already recorded), Redis status cache, ES `_bulk`
   writing two indices (so one item can fail while the other succeeds), WebSocket partner notifier
   (at-least-once from a Postgres table, ack by id, heartbeat, reconnect + resend). Failpoints for each
   safety mechanism.
2. Connectors: ClickHouse (flush async inserts before checks, duplicates, parts, OPTIMIZE FINAL),
   Redis (prefix isolation), Mongo (database per namespace), reconcile (id sets across stores),
   Mock Hub SMTP server (451/550/disconnect mid-DATA/slow, relay to Mailpit), socket partners
   (WebSocket + TCP: close, reset, half-open, drop acks, fragment, coalesce, stall).
3. Scenarios and acceptance with mutation runs.

## Scenarios
| Case | What it proves |
|------|----------------|
| TC-SEARCH-001 (kafka, db-poll) | `_bulk` HTTP 200 with one failed item is retried; ClickHouse/Mongo not duplicated; all stores reconcile |
| TC-MAIL-001 (kafka, db-poll) | SMTP 451 then disconnect mid-DATA: one mail, 3 sessions, no duplicated analytics/audit |
| TC-CACHE-001 | refund webhook refreshes Redis cache and ES |
| TC-NOTIFY-001 | WebSocket reset before ack: reconnect, resend, 3/3 acked |
| TC-NOTIFY-002 | half-open partner: heartbeat detects silence, reconnect, 2/2 acked |
| TC-JOBS-001 (extended) | 30 orders reconcile across Postgres, 2 ES indices, ClickHouse, Mongo |

## Verification
`scripts/acceptance/phase4.sh` (log `out/acceptance/phase4.log`):
```
run r20261004-191733-p4: 7 pass, 0 fail, 0 error, 0 skipped   (--mutations, --parallel 4)
  TC-CACHE-001               M1 skip_cache_invalidate  -> case FAIL  red=['A2'] KILLED
  TC-MAIL-001[kafka]         M1 ch_no_dedup_token      -> case FAIL  red=['A4'] KILLED
  TC-MAIL-001[kafka]         M2 mongo_dup_is_error     -> case FAIL  red=['A1', 'A2', 'A3'] KILLED
  TC-MAIL-001[db-poll]       M1 ch_no_dedup_token      -> case FAIL  red=['A4'] KILLED
  TC-MAIL-001[db-poll]       M2 mongo_dup_is_error     -> case FAIL  red=['A1', 'A2', 'A3'] KILLED
  TC-NOTIFY-001[kafka]       M1 ws_no_resend           -> case FAIL  red=['A1', 'A2'] KILLED
  TC-NOTIFY-002[kafka]       M1 ws_no_heartbeat        -> case FAIL  red=['A1', 'A2'] KILLED
  TC-SEARCH-001[kafka]       M1 es_ignore_bulk_errors  -> case FAIL  red=['A1', 'A2'] KILLED
  TC-SEARCH-001[kafka]       M2 ch_no_dedup_token      -> case FAIL  red=['A2', 'A3', 'A4'] KILLED
  TC-SEARCH-001[db-poll]     M1 es_ignore_bulk_errors  -> case FAIL  red=['A1', 'A2'] KILLED
  TC-SEARCH-001[db-poll]     M2 ch_no_dedup_token      -> case FAIL  red=['A2', 'A3', 'A4'] KILLED
  7 executions pass; 11/11 mutations killed
PHASE4 ACCEPTANCE: PASS
```
The faults really happened (from the evidence): `es bulk: 1/2 items failed (HTTP 200): cluster_block_exception …`,
partner journal `fault: reset` / `fault: half_open` then a second session resending, SMTP `451` then `EOF`.
The whole earlier suite (16 executions) still passes with the new integrations.

## Found on the way
The mutation gate caught a weak test: TC-NOTIFY-002 stayed green with heartbeats disabled. Root cause in
the mock, not the worker: the half-open fault used the WebSocket library's `CloseRead`, which closes the
connection on any data message — the client saw an I/O error instead of silence. The mock now stays
silent until reset; a unit test asserts writes succeed and reads time out. The mutation is now killed.

## Risks / open points
- See A42 (Redis lock expiry, change streams, IMAP not covered by the reference worker).
- Socket codecs beyond JSON/line/length32 wait for the answer to section 9 (protocols of the real partners).
