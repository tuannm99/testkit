package orchestrator

import (
	"testing"
	"time"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

func ex(id, res, class string, req ...string) *result.Execution {
	return &result.Execution{ID: id, CaseID: id, Result: res, Class: class, Requirement: req}
}

func failedRules(g *result.Gate) map[string]bool {
	m := map[string]bool{}
	for _, r := range g.Rules {
		if !r.Passed {
			m[r.Name] = true
		}
	}
	return m
}

func TestGate(t *testing.T) {
	today := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	suite := &scenario.Suite{Requirements: []string{"R1", "R2"}, Mutations: true}

	ok := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1"), ex("B", "pass", "", "R2")}}
	if g := Gate(ok, suite, nil, today); g.Decision != "GO" {
		t.Fatalf("all green: %+v", g.Rules)
	}

	failing := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1"), ex("B", "fail", result.ClassProduct, "R2")}}
	if g := Gate(failing, suite, nil, today); g.Decision != "NO-GO" || !failedRules(g)["không có fail / error"] ||
		!failedRules(g)["mọi yêu cầu của release có testcase pass"] {
		t.Fatalf("fail: %+v", g.Rules)
	}

	flaky := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1", "R2"), ex("F", "fail", result.ClassFlaky)}}
	if g := Gate(flaky, suite, nil, today); g.Decision != "NO-GO" {
		t.Fatal("flaky outside quarantine must be NO-GO")
	}
	q := *suite
	q.Quarantine = []scenario.Quarantine{{Case: "F", Owner: "dev", Ticket: "ORD-1", Deadline: "2026-10-10"}}
	if g := Gate(flaky, &q, nil, today); g.Decision != "GO" || len(g.Flaky) != 1 {
		t.Fatalf("quarantined flaky: %+v", g)
	}
	q.Quarantine[0].Deadline = "2026-10-01"
	if g := Gate(flaky, &q, nil, today); g.Decision != "NO-GO" {
		t.Fatal("expired quarantine must be NO-GO")
	}

	capSkip := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1"), ex("B", "pass", "", "R2"), ex("N", "skipped", result.ClassCapability, "R3")}}
	suite = &scenario.Suite{Requirements: []string{"R1", "R2", "R3"}, Mutations: true}
	if g := Gate(capSkip, suite, nil, today); g.Decision != "NO-GO" {
		t.Fatal("capability skip blocks by default")
	}
	allow := *suite
	allow.Gate.AllowSkippedCapability = true
	if g := Gate(capSkip, &allow, nil, today); g.Decision != "GO" {
		t.Fatalf("allowed capability skip: %+v", g.Rules)
	}

	suite = &scenario.Suite{Requirements: []string{"R1", "R2"}, Mutations: true}
	surv := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1", "R2")}}
	surv.Executions[0].Mutations = []result.MutationResult{{ID: "M1", Killed: false}}
	if g := Gate(surv, suite, nil, today); g.Decision != "NO-GO" {
		t.Fatal("surviving mutation must be NO-GO")
	}

	reg := &result.Run{Executions: []*result.Execution{ex("A", "pass", "", "R1", "R2")},
		Perf: []*result.PerfResult{{ID: "A", Comparisons: []result.PerfComparison{{Metric: "p95_ms", Regression: true}}}}}
	if g := Gate(reg, suite, nil, today); g.Decision != "NO-GO" {
		t.Fatal("perf regression must be NO-GO")
	}
}
