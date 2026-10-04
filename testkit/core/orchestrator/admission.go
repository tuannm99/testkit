package orchestrator

import (
	"fmt"
	"path"
	"strings"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// AdmissionPrecheck returns why a case cannot enter the mutation gate at all
// (nothing is run for it), or "" when it can.
func AdmissionPrecheck(c *scenario.Case) string {
	if len(c.Mutations) == 0 && c.Perf == nil {
		return "no mutation declared: without a defect the case must catch, a green result proves nothing " +
			"(add mutations: [{id, failpoint, expect_red}])"
	}
	return ""
}

// Admit evaluates the deterministic admission rules of each case from a run
// made with Options{Mutations: true, Stability: stability}:
//
//  1. green on the unbroken system for every trigger, `stability` times in a row;
//  2. every declared mutation turns it red, through every trigger, with the
//     expected assertions red (the case detects the defect it claims to);
//  3. performance cases are compared with a recorded baseline of this environment.
//
// A skipped execution (missing capability) makes the case incomplete, never admitted.
func Admit(run *result.Run, cases []*scenario.Case, stability int) []*result.Admission {
	if stability < 1 {
		stability = 1
	}
	var out []*result.Admission
	for _, c := range cases {
		a := &result.Admission{CaseID: c.ID, File: c.File, Stability: stability, Triggers: c.Triggers()}
		if why := AdmissionPrecheck(c); why != "" {
			a.Status = result.Rejected
			a.Rules = append(a.Rules, result.Rule{Name: "has-mutations", Detail: why})
			out = append(out, a)
			continue
		}
		var base []*result.Execution
		for _, ex := range run.Executions {
			if ex.CaseID == c.ID && ex.Mutation == "" {
				base = append(base, ex)
			}
		}
		skipped := false
		green := result.Rule{Name: "green-and-stable", OK: len(base) > 0}
		var details []string
		for _, ex := range base {
			green.Evidence = append(green.Evidence, path.Join(ex.Dir, "case.json"))
			reps := max(ex.Repetitions, 1)
			switch {
			case ex.Result == result.Skipped:
				skipped = true
				details = append(details, fmt.Sprintf("%s skipped: %s", ex.ID, ex.Reason))
			case ex.Result != result.Pass:
				green.OK = false
				details = append(details, fmt.Sprintf("%s %s: %s", ex.ID, ex.Result, ex.Reason))
			case reps < stability:
				green.OK = false
				details = append(details, fmt.Sprintf("%s passed %d/%d times", ex.ID, reps, stability))
			default:
				details = append(details, fmt.Sprintf("%s passed %d/%d times", ex.ID, reps, stability))
			}
		}
		if len(base) == 0 {
			details = append(details, "no execution found in the run")
		}
		green.Detail = strings.Join(details, "; ")
		green.Skipped = skipped && green.OK
		a.Rules = append(a.Rules, green)

		if len(c.Mutations) > 0 {
			mr := result.Rule{Name: "mutations-killed", OK: true}
			details = nil
			for _, ex := range base {
				if ex.Result == result.Skipped {
					continue
				}
				if ex.Result != result.Pass {
					mr.OK = false
					details = append(details, fmt.Sprintf("%s: not evaluated (the case is not green)", ex.ID))
					continue
				}
				got := map[string]result.MutationResult{}
				for _, m := range ex.Mutations {
					got[m.ID] = m
				}
				for _, m := range c.Mutations {
					r, ok := got[m.ID]
					label := m.ID + suffixOf(ex.Trigger)
					switch {
					case !ok:
						mr.OK = false
						details = append(details, label+" not run")
					case !r.Killed:
						mr.OK = false
						details = append(details, fmt.Sprintf("%s SURVIVED (%s; red: %v, required: %v)", label, r.Result, r.RedIDs, r.Expected))
						mr.Evidence = append(mr.Evidence, path.Join(r.Dir, "case.json"))
					default:
						a.Killed = append(a.Killed, label)
						details = append(details, fmt.Sprintf("%s killed (red: %v)", label, r.RedIDs))
						mr.Evidence = append(mr.Evidence, path.Join(r.Dir, "case.json"))
					}
				}
			}
			mr.Detail = strings.Join(details, "; ")
			a.Rules = append(a.Rules, mr)
		}

		if c.Perf != nil {
			pr := result.Rule{Name: "baseline-compared", OK: true}
			details = nil
			for _, ex := range base {
				switch {
				case ex.Result == result.Skipped:
				case ex.Perf == nil || len(ex.Perf.Comparisons) == 0:
					pr.OK = false
					note := "no comparison"
					if ex.Perf != nil && ex.Perf.Note != "" {
						note = ex.Perf.Note
					}
					details = append(details, fmt.Sprintf("%s: %s (record one with `testkit run --record-baseline`)", ex.ID, note))
				default:
					details = append(details, fmt.Sprintf("%s: compared with baseline %s (%s)", ex.ID, ex.Perf.BaselineKey, ex.Perf.BaselineEnv))
					pr.Evidence = append(pr.Evidence, path.Join(ex.Dir, "perf", "summary.json"))
				}
			}
			pr.Detail = strings.Join(details, "; ")
			a.Rules = append(a.Rules, pr)
		}

		a.Status = result.Admitted
		for _, r := range a.Rules {
			if !r.OK {
				a.Status = result.Rejected
			}
		}
		if a.Status == result.Admitted && skipped {
			a.Status = result.Incomplete
		}
		out = append(out, a)
	}
	return out
}

func suffixOf(trigger string) string {
	if trigger == "" {
		return ""
	}
	return "[" + trigger + "]"
}
