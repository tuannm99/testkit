package files

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

func read(t *testing.T, p string) [][]string {
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestExport(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	cases := []*scenario.Case{
		{ID: "TC-A", Title: "Retry | pipe", QCKey: "ORD-T1", File: "/repo/testkit/scenarios/a.yaml", Risk: "P0", Status: "approved",
			Requirement: scenario.Strings{"REQ-1"}, Trigger: scenario.Strings{"kafka", "db-poll"},
			Expect:    []scenario.ExpectSpec{{ID: "A1", Check: "x", Op: "eq", Expected: 1}, {ID: "A2", Check: "y", Op: "eq", Expected: 2}},
			Mutations: []scenario.Mutation{{ID: "M1", Failpoint: "no_retry", Title: "no retry"}}},
		{ID: "TC-B", Title: "Not in this run", File: "/repo/testkit/scenarios/b.yaml"},
	}
	run := &result.Run{RunID: "r1", Suite: "release", StartedAt: t0, FinishedAt: t0.Add(time.Minute),
		Gate: &result.Gate{Decision: "GO", Rules: []result.GateRule{{Name: "no fail", Passed: true, Detail: "none"}}},
		Executions: []*result.Execution{
			{ID: "TC-A[kafka]", CaseID: "TC-A", Trigger: "kafka", Result: "pass", Dir: "TC-A/kafka", Requirement: []string{"REQ-1"},
				StartedAt: t0, FinishedAt: t0.Add(1500 * time.Millisecond), Mutations: []result.MutationResult{{ID: "M1", Killed: true}}},
			{ID: "TC-A[db-poll]", CaseID: "TC-A", Trigger: "db-poll", Result: "fail", Class: "product", Reason: "A1", Dir: "TC-A/db-poll",
				Requirement: []string{"REQ-1"}, StartedAt: t0, FinishedAt: t0.Add(time.Second)},
		}}
	dir := &evidence.Dir{Root: t.TempDir()}
	rel := func(p string) string { return strings.TrimPrefix(p, "/repo/") }
	if _, err := Export(dir, run, cases, rel); err != nil {
		t.Fatal(err)
	}
	tc := read(t, filepath.Join(dir.Root, "qc", "testcases.csv"))
	if len(tc) != 3 || tc[1][0] != "TC-A" || tc[1][1] != "ORD-T1" || tc[2][12] != "2" || tc[2][13] != "Kiểm tra A2: y" ||
		tc[1][16] != "testkit/scenarios/a.yaml" || tc[1][9] != "kafka, db-poll" {
		t.Fatalf("testcases.csv: %q", tc)
	}
	res := read(t, filepath.Join(dir.Root, "qc", "results.csv"))
	if len(res) != 3 || res[1][7] != "fail" || res[1][9] != "pass" || res[1][12] != "1" || res[2][11] != "A1" ||
		res[1][15] != "1.5" || res[1][16] != "TC-A/kafka/case.json" {
		t.Fatalf("results.csv: %q", res)
	}
	md, _ := os.ReadFile(filepath.Join(dir.Root, "qc", "results.md"))
	for _, want := range []string{"Cổng release: GO", "**FAIL**", `Retry \| pipe`, "1/1 đỏ", "(../TC-A/db-poll/case.json)"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("results.md misses %q:\n%s", want, md)
		}
	}
	tmd, _ := os.ReadFile(filepath.Join(dir.Root, "qc", "testcases.md"))
	if !strings.Contains(string(tmd), "## TC-A") || strings.Contains(string(tmd), "TC-B") || !strings.Contains(string(tmd), "M1 `no_retry`") {
		t.Errorf("testcases.md:\n%s", tmd)
	}
}
