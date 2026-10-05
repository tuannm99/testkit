package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/result"
)

func runFixture(t *testing.T) (*result.Run, string) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "TC-A", "kafka", "logs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "TC-A", "kafka", "case.json"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "TC-A", "kafka", "logs", "svc.log"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "TC-A", "kafka", "timeline.json"), []byte(`[{"at":"2026-10-05T01:00:00Z","kind":"step","name":"1. job","status":"ok"}]`), 0o644)
	run := &result.Run{RunID: "r1", Suite: "release", Gate: &result.Gate{Decision: "NO-GO", Rules: []result.GateRule{{Name: "no fail", Passed: false, Detail: "TC-A"}}},
		Executions: []*result.Execution{
			{ID: "TC-A[kafka]", CaseID: "TC-A", Title: "retry", Trigger: "kafka", Result: "fail", Class: "product", Reason: "assertion(s) failed: A1", Dir: "TC-A/kafka",
				LogTail:    []string{"ERROR charge failed for cust+x@shop.test"},
				Assertions: []assert.Outcome{{ID: "A1", Result: "fail", Check: "postgres.order.o1.status", Operator: "eq", Expected: "paid", Actual: "failed"}}},
			{ID: "TC-B", CaseID: "TC-B", Result: "pass", Dir: "TC-B", Mutations: []result.MutationResult{{ID: "M1", Killed: false, Result: "pass"}}},
			{ID: "TC-C", CaseID: "TC-C", Result: "pass", Dir: "TC-C"},
		}}
	return run, root
}

func TestTriage(t *testing.T) {
	run, root := runFixture(t)
	req, ok := TriageRequest(run, root)
	if !ok || !strings.Contains(req.Prompt, "=== Execution TC-A[kafka] (red)") || !strings.Contains(req.Prompt, "TC-B (mutation-survived)") ||
		strings.Contains(req.Prompt, "TC-C") || !strings.Contains(req.Prompt, "TC-A/kafka/logs/svc.log") {
		t.Fatalf("request:\n%s", req.Prompt)
	}
	c := &Client{Secrets: map[string]string{}}
	prep, _, err := c.Prepare(req)
	if err != nil || strings.Contains(prep.Prompt, "cust+x@shop.test") {
		t.Fatalf("not redacted: %v", err)
	}
	answer := "```json\n" + `{"items":[
	 {"execution":"TC-A[kafka]","category":"product","agrees_with_rule_class":true,"summary":"s","evidence":["TC-A/kafka/logs/svc.log","TC-A/kafka/made-up.log"],"next_steps":["x"],"confidence":"high"},
	 {"execution":"TC-Z","category":"product"},
	 {"execution":"TC-B","category":"guess","confidence":"sure"}],"overall":"o"}` + "\n```"
	tr, err := ParseTriage(run, root, answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Items) != 2 || tr.Items[0].RuleClass != "product" || len(tr.Items[0].Evidence) != 1 || len(tr.Items[0].Dropped) != 1 ||
		tr.Items[1].Category != "unknown" || tr.Items[1].Confidence != "low" || len(tr.Problems) != 3 {
		t.Fatalf("%+v", tr)
	}
	if md := tr.Markdown(); !strings.Contains(md, Disclaimer) || strings.Contains(md, "made-up") {
		t.Fatalf("md:\n%s", md)
	}
	if _, err := ParseTriage(run, root, "I think it is the database"); err == nil {
		t.Fatal("prose accepted as triage JSON")
	}
}

func TestSummaryFlagsContradiction(t *testing.T) {
	run, _ := runFixture(t)
	md, warn := SummaryMarkdown(run, nil, "### Tóm tắt\nRelease GO, mọi thứ ổn.", "p", "m")
	if len(warn) != 1 || !strings.Contains(md, "gate decision: NO-GO") || !strings.Contains(md, "Cảnh báo") {
		t.Fatalf("%v\n%s", warn, md)
	}
	if _, warn := SummaryMarkdown(run, nil, "Kết luận: NO-GO vì TC-A đỏ.", "p", "m"); len(warn) != 0 {
		t.Fatalf("false warning %v", warn)
	}
}

func TestFinalizeDraft(t *testing.T) {
	answer := "Here:\n```yaml\n# keeps my comment\nid: TC-WRONG\ntitle: x\nstatus: approved\nqc_key: ORD-T9\nadmission: {run_id: r, by: me}\nexpect:\n  - { id: A1, check: x, eq: 1 }\n```\n"
	answer = strings.Replace(answer, "title: x\n", "title: x\nowner: qc-lead\nrequirement: REQ-INVENTED\n", 1)
	out, err := FinalizeDraft(answer, "TC-DRAFT-001", "", "anthropic/claude-opus-5-5", []string{"requirement: text"}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"id: TC-DRAFT-001", "status: draft", "requirement: REQ-TBD", "by: anthropic/claude-opus-5-5", "keeps my comment", "Drafted by"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"approved", "ORD-T9", "admission", "TC-WRONG", "qc-lead", "REQ-INVENTED"} {
		if strings.Contains(s, bad) {
			t.Errorf("kept %q:\n%s", bad, s)
		}
	}
	if _, err := FinalizeDraft("not: [yaml", "X", "", "b", nil, time.Now()); err == nil {
		t.Fatal("broken YAML accepted")
	}
}
