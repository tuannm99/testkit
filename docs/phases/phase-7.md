# Phase 7 — AI assistant (advisory) and the agent guide

Decisions (2026-10-05): any model (Claude or other), a guide for agents, only redacted data sent (A62–A67).

## What it does
| Command | Output | Who decides |
|---|---|---|
| `testkit ai context <run> --task triage\|summary` | the exact redacted payload, nothing sent | — |
| `testkit ai triage <run>` | `ai/triage.{json,md}`: per red / flaky / weak execution a suggested category, reasoning, cited evidence, next steps | TestKit's rule class stays authoritative; people decide |
| `testkit ai summary <run>` | `ai/summary.md`: TestKit's facts, then the model's sign-off text, with contradiction warnings | the gate |
| `testkit ai draft --service <svc> --req-id <REQ> --requirement "..."` | `scenarios/drafts/<ID>.yaml`, lint-clean, `status: draft` | `testkit admit` + a person's `--approve` |

Providers: `none` (default; prompt written for manual use, answer via `--response`), `anthropic` (official
Go SDK, `claude-opus-5-5`, refusal fallback, refusals are errors), `openai-compatible`, `command`
(e.g. `claude -p`). The report shows AI output in a separate "Gợi ý của AI (tham khảo)" section.

## Data handling
Selected fields only → redaction → guard (refuses if a secret, credential, e-mail or card number remains)
→ audit record (redacted payload, sha256, counts, answer) → provider. The system prompt is TestKit's fixed
text (embedded, versioned). Nothing un-redacted is written to the audit or sent.

## Agent guide
`AGENTS.md` (any agent; `CLAUDE.md` imports it): non-negotiable rules, repository map, commands, workflows
for writing a case, investigating a red run, performance, and what to check before finishing.

## Found on the way
- **Redaction swallowed evidence paths.** The first token rule (`[A-Za-z0-9+/_-]{32,}`) replaced 116 file paths
  with placeholders, which would have broken citation checks. It also rewrote our own system prompt
  (`<secret:NAME>`). Tokens are now path-free segments that look random (hex ≥ 32 or entropy ≥ 4.2),
  and the fixed system text is checked but not rewritten. Both are covered by tests.
- **Triage lacked the decisive fact.** With a real model, the flaky TC-PERF-001 got "cause unknown". Adding
  the deterministic fact "ran concurrently with 12 other executions" led the model to the cause found by
  hand in Phase 6 (contention): run it alone, isolate perf cases.
- **AI tasks rewrote `run.json`** (re-encoding, same values) through the full report re-render. They now
  re-render `report.html` only. The acceptance compares `run.json` byte for byte before and after.
- **`trigger.drain` step semantics.** An AI-drafted case added an explicit drain. Under its mutation
  (`skip_paid_check`), the broken worker kept re-charging (21 charges) and the drain timed out. The step
  turned that into an *environment* error, so the mutation was "not evaluated" instead of red. A drain
  timeout now lets the assertions decide (like the end-of-case drain), and errored mutation runs are labelled
  "not evaluated" in admission.

## Verification
`scripts/acceptance/phase7.sh` (log `out/phase7-acceptance.log`), with `TK_AI_REAL_COMMAND='claude -p'`:
```
== 1. redacted payload            no declared secret / e-mail in the triage and summary payloads
== 2. provider none: nothing sent "redacted prompt is in ..."; bundle verifies
== 3. manual answer               triage: TC-ORDER-030[kafka] test-weakness evidence [.../case.json] dropped ['does/not/exist.json']
                                  report shows the advisory section; verify + pack (secret scan) pass
== 4. OpenAI-compatible endpoint  summary written; request received by the endpoint contains no e-mail
== 5. drafted case                forced id / status draft / REQ-TBD, no owner/admission; lint OK
== 6. results untouched           run.json identical before/after the AI tasks
== 7. real model (claude -p)      TC-ORDER-030[kafka] AI: test-weakness (high) "mutation M1 (no_idempotency_key) sống sót ..."
PHASE7 ACCEPTANCE: PASS
```
Real-model runs, also via `claude -p`:
- **Triage of the flaky perf run:** the model proposed contention and gave isolation steps. All 7 citations were valid.
- **Release summary:** correct numbers and gate, plus the right residual risks (REQ-283, self-faked mocks, draft TC-PERF-901).
- **Drafting:** requirement "job delivered 3 times → charged once, paid, one mail" → `TC-DRAFT-001`, lint OK on the first round.
  - `admit` then rejected it, correctly: under the mutation only A2 went red, while the draft claimed A2 and A3. A3 legitimately stays green because the Idempotency-Key makes the gateway replay.
  - After the reviewer corrected `expect_red` to `[A2]`: **ADMITTED** (run `r20261005-admit-draft3`). It stays `draft` until a person approves it.

The Claude provider (SDK) is verified against a local stand-in of the Messages API: request shape,
headers, `fallbacks: "default"`, effort, refusal handling. No API key was available in this
environment for a direct SDK call to the real API.

## Limits
- Pattern redaction cannot recognise free-text personal names; evidence must hold test data only (it does
  today: synthetic customers with the namespace in the address).
- AI suggestions can be wrong; they are labelled, validated and never used by the gate.
