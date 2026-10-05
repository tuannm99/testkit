# Phase 6 — Release suite and gate, Jira (Zephyr Scale), zip bundle, UI tests

Answers to section 9 (2026-10-05): QC handover as **tool-neutral CSV + Markdown first** (A54b), with
Jira + the test-cycle plugin (Zephyr Scale, A54) as an optional exporter for the later port; Docker only
(no Helm, A56), third parties REST + WebSocket and self-faked mocks when there is no sandbox (A57),
SLOs per case with 10 % default regression tolerance (A58), bundle as a zip (A60).

## Plan
1. Suite files (`kind: Suite`) and a release gate computed from hard rules (A59).
2. QC handover: `qc/testcases.{csv,md}` and `qc/results.{csv,md}` in every bundle (A54b); Zephyr Scale
   exporter + upload kept as an option (A54, A55).
3. Zip bundle with integrity check and secret scan (A60).
4. Playwright executor (`ui.run`) and a UI case (A61).
5. Mock provenance `verified_against: sandbox | docs`.

## Commands
```
./tk run testkit/suites/release.yaml          # GO (exit 0) / NO-GO (exit 1) → out/<run_id>/ + out/<run_id>.zip
./tk qc cases                                 # out/qc/testcases.csv + testcases.md
# every bundle: qc/results.csv + results.md, qc/testcases.csv + testcases.md
ZEPHYR_TOKEN=... ./tk qc push out/<run_id>    # only with qc.tool: files, zephyr-scale
./tk pack out/<run_id>                        # manifest check + secret scan + zip
./tk qc export out/<run_id>                   # rewrite the Zephyr files of an intact bundle (after adding qc_key)
```
QC walkthrough: `docs/huong-dan-qc.md`.

## Defects found on the way
- **Secrets in the evidence check was blind.** The first `pack` reported every test password in many
  files. They were not real leaks: env dumps were already redacted. But the test passwords were `testkit`,
  the same as the project and user names, so exact-value scanning could not tell a leak from ordinary text.
  Test credentials now have distinctive values. The scan ignores values shorter than 8 characters and
  warns about them. With that, the bundles pack clean, and grep confirms no secret value inside.
- **Perf measured under contention.** The first release run was NO-GO: TC-PERF-001 reported p95 +659 %
  (p = 0.036) on attempt 1, then passed on retry. It had overlapped 12 other executions, including
  TC-PERF-002's load. Perf cases and cases driving load (`load.start`) now run alone, after the parallel batch,
  like infrastructure chaos. Evidence of the bad run: `out/r20261005-release-1` (not committed).
- **Stale baseline.** After the fix, TC-PERF-001 measured p95 70 ms against a 321 ms baseline. A regression
  back to 300 ms would have passed. Comparisons now flag "baseline có thể đã cũ" when the result is
  significantly better by more than twice the tolerance, and the gate lists it. Both baselines were
  re-recorded on this stack (p95 median 46 ms, MAD 0.4). TC-PERF-901 is still caught: p95 46 → 7472 ms, p = 0.018.
- **`./tk` could run a stale CLI.** It only built the runner image when it was missing. It now rebuilds
  when the CLI sources change (`testkit.source_hash` label, as for the other images).

## Verification
`scripts/acceptance/phase6.sh` (log `out/phase6-acceptance.log`), run `r20261005-024858-p6`:
```
== 1. release suite
release gate: GO
  ok   có testcase để chạy — 30 execution(s)
  ok   không có fail / error — không có
  ok   không có flaky ngoài danh sách cách ly — không có
  ok   không bỏ qua testcase — không có
  ok   thiếu quyền/khả năng trên máy chạy — TC-CHAOS-004[kafka]
  ok   phản chứng: mọi mutation làm case đỏ — 22/22 đỏ; không có
  ok   cùng kết quả qua mọi trigger — không có
  ok   không regression hiệu năng so với baseline — không có
  ok   mọi yêu cầu của release có testcase pass — thiếu/không đạt: không có; chưa kiểm được trên máy này
       (thiếu quyền, được phép): REQ-283
bundle: out/r20261005-024858-p6.zip  sha256 c4722338e892a4d8ffcfaacf4c79ae89f3809898983861d5c47e33178047bbf1
  30 executions, 22 mutations killed; qc/results.csv 30 rows, qc/testcases.csv 208 step rows; 2061 files in the zip
  no declared secret in the zip
== 2. NO-GO suite (weak drafted case)
  FAIL phản chứng: mọi mutation làm case đỏ — 0/1 đỏ; TC-ORDER-030[kafka] M1 (no_idempotency_key)
  gate NO-GO
== 3. Zephyr Scale port (on a copy of the bundle; push refused while qc.tool is files)
  uploaded 22 executions to test cycle 'release · r20261005-024858-p6'
== 4. tampered bundle
  error: bundle does not match its manifest: modified: report.html
PHASE6 ACCEPTANCE: PASS
```
The script checks every row of `qc/results.csv` against `run.json`, and that `qc/testcases.csv` lists exactly
the run's cases.
UI: TC-UI-001 passes; with `no_retry` the page shows "Thanh toán lỗi" and A1, A2, A3 go red.
Screenshots and traces are in the bundle and the screenshots render inline in the report.
Newcomer path: `./tk lint`, `./tk run testkit/scenarios/ui`, `./tk pack`, `./tk qc cases` run inside the runner
container (rebuilt automatically from the current sources).

## Limits / to confirm
- The CSV columns are generic; mapping them in the eventual tool's importer is a one-time setup.
- Zephyr Scale (optional) assumes the **Cloud** API and custom format v1 (format checked against SmartBear's
  zephyr-scale-junit-integration README). The upload was tested against a local stand-in, not a real Jira.
  Data Center / Zephyr Squad / Xray need confirmation (A54).
- `requirement:` ids (REQ-xxx) should become Jira issue keys so "Coverage" links in Zephyr.
- Playwright image is 3.7 GB; the first `ui.run` on a new host pulls it.
- This host has no `sch_netem`: TC-CHAOS-004 / REQ-283 are listed as not verified here.
- Phase 7 (AI drafting / triage, never deciding pass/fail) is next.
