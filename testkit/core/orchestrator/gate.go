package orchestrator

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Gate decides GO / NO-GO for a release suite from hard rules only. Each
// rule says what was required and what was observed; one failed rule is NO-GO.
func Gate(run *result.Run, s *scenario.Suite, cases []*scenario.Case, today time.Time) *result.Gate {
	g := &result.Gate{Decision: "GO"}
	add := func(name, required string, ok bool, detail string) {
		g.Rules = append(g.Rules, result.GateRule{Name: name, Required: required, Passed: ok, Detail: detail})
		if !ok {
			g.Decision = "NO-GO"
		}
	}
	quarantined := map[string]scenario.Quarantine{}
	for _, q := range s.Quarantine {
		quarantined[q.Case] = q
	}
	day := today.UTC().Format("2006-01-02")

	var base []*result.Execution
	for _, ex := range run.Executions {
		if ex.Mutation == "" {
			base = append(base, ex)
		}
	}
	add("có testcase để chạy", "≥ 1 execution", len(base) > 0, fmt.Sprintf("%d execution(s)", len(base)))

	var failed, flaky, expired, skipped, capSkipped []string
	for _, ex := range base {
		switch {
		case ex.Class == result.ClassFlaky:
			q, ok := quarantined[ex.CaseID]
			switch {
			case !ok:
				flaky = append(flaky, ex.ID)
			case q.Deadline < day:
				expired = append(expired, fmt.Sprintf("%s (hạn %s)", ex.ID, q.Deadline))
			default:
				g.Flaky = append(g.Flaky, result.Quarantine{CaseID: ex.ID, Reason: q.Reason + " — " + ex.Reason, Owner: q.Owner, Deadline: q.Deadline, Ticket: q.Ticket})
			}
		case ex.Result == result.Fail || ex.Result == result.Error:
			failed = append(failed, fmt.Sprintf("%s %s: %s", ex.ID, ex.Result, ex.Reason))
		case ex.Result == result.Skipped && ex.Class == result.ClassCapability:
			capSkipped = append(capSkipped, ex.ID)
		case ex.Result == result.Skipped:
			skipped = append(skipped, fmt.Sprintf("%s: %s", ex.ID, ex.Reason))
		}
	}
	add("không có fail / error", "0", len(failed) == 0, orNone(failed))
	add("không có flaky ngoài danh sách cách ly", "0 (flaky không bao giờ tính là pass)", len(flaky) == 0 && len(expired) == 0,
		orNone(append(flaky, prefix("hết hạn cách ly: ", expired)...)))
	add("không bỏ qua testcase", "0 skipped", len(skipped) == 0, orNone(skipped))
	if len(capSkipped) > 0 {
		req := "0 (gate.allow_skipped_capability: false)"
		if s.Gate.AllowSkippedCapability {
			req = "được phép, liệt kê ở đây (gate.allow_skipped_capability: true)"
		}
		add("thiếu quyền/khả năng trên máy chạy", req, s.Gate.AllowSkippedCapability, strings.Join(capSkipped, "; "))
	}

	if s.Mutations {
		total, survived := 0, []string{}
		for _, ex := range base {
			for _, m := range ex.Mutations {
				total++
				if !m.Killed {
					survived = append(survived, fmt.Sprintf("%s %s (%s)", ex.ID, m.ID, m.Failpoint))
				}
			}
		}
		add("phản chứng: mọi mutation làm case đỏ", "100%", len(survived) == 0,
			fmt.Sprintf("%d/%d đỏ; %s", total-len(survived), total, orNone(survived)))
	}

	var mismatch []string
	for _, p := range run.Parity {
		if !p.Match {
			mismatch = append(mismatch, p.CaseID+": "+strings.Join(p.Diffs, "; "))
		}
	}
	add("cùng kết quả qua mọi trigger", "khớp", len(mismatch) == 0, orNone(mismatch))

	var regress, stale []string
	for _, p := range run.Perf {
		for _, c := range p.Comparisons {
			if c.Regression {
				regress = append(regress, fmt.Sprintf("%s %s tệ đi %.1f%% (p=%.3f)", p.ID, c.Metric, c.ChangePct, c.PValue))
			}
			if c.Stale {
				stale = append(stale, fmt.Sprintf("%s %s", p.ID, c.Metric))
			}
		}
	}
	if len(run.Perf) > 0 {
		detail := orNone(regress)
		if len(stale) > 0 {
			detail += "; cảnh báo: tốt hơn baseline rõ rệt, baseline có thể đã cũ (ghi lại baseline): " + strings.Join(stale, ", ")
		}
		add("không regression hiệu năng so với baseline", "0", len(regress) == 0, detail)
	}

	if len(s.Requirements) > 0 {
		passed, seen, capOnly := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, ex := range base {
			for _, r := range ex.Requirement {
				if !seen[r] {
					seen[r], passed[r], capOnly[r] = true, true, true
				}
				// covered only when every execution tracing to it passed
				if ex.Result != result.Pass {
					passed[r] = false
				}
				if ex.Class != result.ClassCapability {
					capOnly[r] = false
				}
			}
		}
		var missing, unverified []string
		for _, r := range s.Requirements {
			switch {
			case seen[r] && capOnly[r] && s.Gate.AllowSkippedCapability:
				unverified = append(unverified, r)
			case !seen[r] || !passed[r]:
				missing = append(missing, r)
			}
		}
		sort.Strings(missing)
		detail := "thiếu/không đạt: " + orNone(missing)
		if len(unverified) > 0 {
			detail += "; chưa kiểm được trên máy này (thiếu quyền, được phép): " + strings.Join(unverified, ", ")
		}
		add("mọi yêu cầu của release có testcase pass", fmt.Sprintf("%d yêu cầu", len(s.Requirements)), len(missing) == 0, detail)
	}
	return g
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "không có"
	}
	return strings.Join(l, "; ")
}

func prefix(p string, l []string) []string {
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = p + s
	}
	return out
}
