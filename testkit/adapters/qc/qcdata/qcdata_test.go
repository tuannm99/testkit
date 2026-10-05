package qcdata

import (
	"bytes"
	"encoding/csv"
	"testing"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

func TestByCase(t *testing.T) {
	run := &result.Run{Executions: []*result.Execution{
		{CaseID: "A", Result: "pass", Mutations: []result.MutationResult{{Killed: true}, {Killed: false}}},
		{CaseID: "A", Result: "fail"},
		{CaseID: "A", Mutation: "M1", Result: "fail"},
		{CaseID: "B", Result: "pass"}, {CaseID: "B", Result: "skipped"},
		{CaseID: "C", Result: "skipped"},
		{CaseID: "D", Result: "error"}, {CaseID: "D", Result: "pass"},
	}}
	got := map[string]CaseResult{}
	for _, r := range ByCase(run) {
		got[r.CaseID] = r
	}
	if got["A"].Verdict != "fail" || got["A"].Killed != 1 || got["A"].Mutations != 2 || len(got["A"].Executions) != 2 ||
		got["B"].Verdict != "pass" || got["C"].Verdict != "skipped" || got["D"].Verdict != "error" {
		t.Fatalf("%+v", got)
	}
}

func TestCSVGuardsFormulas(t *testing.T) {
	raw, err := CSV([]string{"a"}, [][]string{{"=HYPERLINK(\"x\")"}, {"-1"}, {"-cmd"}, {"+1"}, {"ok"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))).ReadAll()
	want := []string{"a", "'=HYPERLINK(\"x\")", "-1", "'-cmd", "'+1", "ok"}
	for i, w := range want {
		if rows[i][0] != w {
			t.Errorf("row %d: %q, want %q", i, rows[i][0], w)
		}
	}
}

func TestSteps(t *testing.T) {
	c := &scenario.Case{ID: "X", Steps: []scenario.StepSpec{{Step: "mock.script", Name: "cổng lỗi", With: map[string]any{"mock": "p"}}},
		Expect: []scenario.ExpectSpec{{ID: "A1", Check: "postgres.order.o1.status", Op: "eq", Expected: "paid", Why: "phải paid"}}}
	s, err := Steps(c)
	if err != nil || len(s) != 2 || s[0].Action != "mock.script — cổng lỗi" || s[0].Data != `{"mock":"p"}` ||
		s[1].No != 2 || s[1].Expected != "eq paid — phải paid" {
		t.Fatalf("%+v %v", s, err)
	}
}
