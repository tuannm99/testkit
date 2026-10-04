package report

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
)

func sampleRun() *result.Run {
	t0 := time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)
	pass := &result.Execution{ID: "TC-A-1[kafka]", CaseID: "TC-A-1", Title: "ok case", Requirement: []string{"REQ-1", "REQ-2"},
		Risk: "P0", Service: "svc", Trigger: "kafka", Result: result.Pass, Dir: "TC-A-1/kafka", StartedAt: t0, FinishedAt: t0.Add(time.Second),
		Assertions: []assert.Outcome{{ID: "A1", Result: "pass", Check: "postgres.order.o1.status", Operator: "eq", Expected: "paid",
			Actual: "paid", Why: "must be paid", Evidence: []string{"TC-A-1/kafka/assertions/A1.json"}}},
		Conclusion: []result.Sentence{{Text: "A1 đạt", Evidence: []string{"TC-A-1/kafka/assertions/A1.json"}}},
		Mutations:  []result.MutationResult{{ID: "M1", Failpoint: "no_retry", Killed: true, RedIDs: []string{"A1"}}}}
	fail := &result.Execution{ID: "TC-B-2", CaseID: "TC-B-2", Title: "bad <case>", Requirement: []string{"REQ-1"}, Risk: "P1",
		Service: "svc", Result: result.Fail, Class: result.ClassProduct, Reason: "assertion(s) failed: A1", Dir: "TC-B-2",
		StartedAt: t0, FinishedAt: t0.Add(2 * time.Second), LogTail: []string{"ERROR boom"},
		Assertions: []assert.Outcome{{ID: "A1", Result: "fail", Check: "mock.p.calls", Operator: "eq", Expected: 3, Actual: 4, Why: "no extra call"}}}
	mut := &result.Execution{ID: "TC-A-1[kafka]{mutation M1}", CaseID: "TC-A-1", Mutation: "M1", Result: result.Fail}
	return &result.Run{RunID: "r20261004-180000-abcd", StartedAt: t0, FinishedAt: t0.Add(3 * time.Second),
		Executions: []*result.Execution{pass, fail, mut}}
}

func TestJUnitCountsAndSkipsMutations(t *testing.T) {
	var b bytes.Buffer
	if err := JUnit(&b, sampleRun()); err != nil {
		t.Fatal(err)
	}
	var s junitSuites
	if err := xml.Unmarshal(b.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Tests != 2 || s.Failures != 1 || len(s.Suites) != 1 || s.Suites[0].Cases[1].Failure == nil {
		t.Fatalf("%+v", s)
	}
}

func TestTraceabilityOneRowPerRequirement(t *testing.T) {
	var b bytes.Buffer
	if err := Traceability(&b, sampleRun()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "REQ-1,TC-A-1") || !strings.Contains(lines[1], "1/1") ||
		!strings.HasPrefix(lines[3], "REQ-2,TC-A-1") {
		t.Fatalf("%s", b.String())
	}
}

func TestHTMLEscapesAndContainsSections(t *testing.T) {
	var b bytes.Buffer
	dir := &evidence.Dir{Root: t.TempDir()}
	if err := HTML(&b, dir, sampleRun(), &evidence.Manifest{}); err != nil {
		t.Fatal(err)
	}
	h := b.String()
	for _, want := range []string{"1. Mục đích", "4. Assertion", "7. Kết luận", "8. Bằng chứng phản chứng", "bad &lt;case&gt;",
		"ERROR boom", "nghi lỗi thật của sản phẩm", "case đã đỏ"} {
		if !strings.Contains(h, want) {
			t.Errorf("report misses %q", want)
		}
	}
	if strings.Contains(h, "bad <case>") {
		t.Error("title not escaped")
	}
}
