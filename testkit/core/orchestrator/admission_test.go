package orchestrator

import (
	"testing"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

func admitCase(id string, muts ...string) *scenario.Case {
	c := &scenario.Case{ID: id, Trigger: scenario.Strings{"kafka"}}
	for _, m := range muts {
		c.Mutations = append(c.Mutations, scenario.Mutation{ID: m, Failpoint: "fp_" + m})
	}
	return c
}

func base(id, res string, reps int, muts ...result.MutationResult) *result.Execution {
	return &result.Execution{ID: id + "[kafka]", CaseID: id, Trigger: "kafka", Result: res, Repetitions: reps, Mutations: muts, Dir: id + "/kafka"}
}

func TestAdmitRules(t *testing.T) {
	killed := result.MutationResult{ID: "M1", Killed: true, Result: result.Fail, RedIDs: []string{"A1"}}
	survived := result.MutationResult{ID: "M1", Killed: false, Result: result.Pass}
	cases := []*scenario.Case{
		admitCase("OK", "M1"), admitCase("SURV", "M1"), admitCase("FLAKY", "M1"), admitCase("ONCE", "M1"),
		admitCase("NOMUT"), admitCase("SKIP", "M1"), admitCase("MISSING", "M1", "M2"),
	}
	flaky := base("FLAKY", result.Fail, 0)
	flaky.Class = result.ClassFlaky
	run := &result.Run{Executions: []*result.Execution{
		base("OK", result.Pass, 2, killed), base("SURV", result.Pass, 2, survived), flaky,
		base("ONCE", result.Pass, 1, killed), base("SKIP", result.Skipped, 0), base("MISSING", result.Pass, 2, killed),
	}}
	want := map[string]string{"OK": result.Admitted, "SURV": result.Rejected, "FLAKY": result.Rejected, "ONCE": result.Rejected,
		"NOMUT": result.Rejected, "SKIP": result.Incomplete, "MISSING": result.Rejected}
	for _, a := range Admit(run, cases, 2) {
		if a.Status != want[a.CaseID] {
			t.Errorf("%s: %s, want %s: %+v", a.CaseID, a.Status, want[a.CaseID], a.Rules)
		}
	}
	if a := Admit(run, cases[:1], 2)[0]; len(a.Killed) != 1 || a.Killed[0] != "M1[kafka]" {
		t.Errorf("killed = %v", a.Killed)
	}
}

func TestAdmitPerfNeedsBaseline(t *testing.T) {
	c := &scenario.Case{ID: "P", Trigger: scenario.Strings{"kafka"}, Perf: &scenario.PerfSpec{}}
	ex := base("P", result.Pass, 1)
	ex.Perf = &result.PerfResult{Note: "no baseline for this environment"}
	if a := Admit(&result.Run{Executions: []*result.Execution{ex}}, []*scenario.Case{c}, 1)[0]; a.Status != result.Rejected {
		t.Fatalf("perf without baseline admitted: %+v", a)
	}
	ex.Perf.Comparisons = []result.PerfComparison{{Metric: "p95_ms"}}
	if a := Admit(&result.Run{Executions: []*result.Execution{ex}}, []*scenario.Case{c}, 1)[0]; a.Status != result.Admitted {
		t.Fatalf("perf with baseline rejected: %+v", a)
	}
}
