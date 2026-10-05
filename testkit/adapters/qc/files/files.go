// Package files exports test cases and results as tool-neutral CSV and
// Markdown ("files" QC tool): the CSVs import into Jira test management
// plugins, TestRail or Excel by mapping columns; the Markdown files are for
// people. Steps are one row each with the case fields repeated, which every
// importer accepts.
package files

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/adapters/qc/qcdata"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Dir is where the export lives inside the evidence bundle.
const Dir = "qc"

// Rel shows a case file relative to the project.
type Rel func(string) string

var caseHeader = []string{"Case ID", "QC Key", "Title", "Requirement", "Risk", "Priority", "Status", "Owner", "Service",
	"Triggers", "Purpose", "Preconditions", "Step No", "Step", "Test Data", "Expected Result", "Source File"}

// TestCases returns testcases.csv and testcases.md for the cases.
func TestCases(cases []*scenario.Case, rel Rel) (csvRaw, md []byte, err error) {
	var rows [][]string
	var b strings.Builder
	b.WriteString("# Danh sách testcase\n\n")
	fmt.Fprintf(&b, "%d testcase. Mỗi bước kiểm tra có kỳ vọng cụ thể (toán tử + giá trị); kết quả do TestKit quyết định bằng luật cứng.\n\n", len(cases))
	b.WriteString("| Case | Tiêu đề | Yêu cầu | Rủi ro | Trạng thái | Trigger |\n|---|---|---|---|---|---|\n")
	for _, c := range cases {
		fmt.Fprintf(&b, "| [%s](#%s) | %s | %s | %s | %s | %s |\n", c.ID, strings.ToLower(c.ID), qcdata.MD(c.Title),
			strings.Join(c.Requirement, ", "), c.Risk, c.Status, triggers(c))
	}
	for _, c := range cases {
		steps, err := qcdata.Steps(c)
		if err != nil {
			return nil, nil, err
		}
		src := rel(c.File)
		for _, s := range steps {
			rows = append(rows, []string{c.ID, c.QCKey, c.Title, strings.Join(c.Requirement, ", "), c.Risk, qcdata.Priority(c.Risk),
				c.Status, c.Owner, c.Service, triggers(c), strings.TrimSpace(c.Purpose),
				strings.Join(c.Preconditions, "\n"), fmt.Sprint(s.No), s.Action, s.Data, s.Expected, src})
		}
		fmt.Fprintf(&b, "\n## %s\n\n**%s**\n\n", c.ID, qcdata.MD(c.Title))
		fmt.Fprintf(&b, "- Yêu cầu: %s · Rủi ro: %s · Trạng thái: %s · Người phụ trách: %s\n", strings.Join(c.Requirement, ", "), c.Risk, c.Status, c.Owner)
		fmt.Fprintf(&b, "- Service: %s · Trigger: %s · Tệp: `%s`\n", c.Service, triggers(c), src)
		if c.QCKey != "" {
			fmt.Fprintf(&b, "- Mã trong công cụ QC: %s\n", c.QCKey)
		}
		if p := strings.TrimSpace(c.Purpose); p != "" {
			fmt.Fprintf(&b, "\n%s\n", p)
		}
		if len(c.Preconditions) > 0 {
			b.WriteString("\nTiền điều kiện:\n")
			for _, p := range c.Preconditions {
				fmt.Fprintf(&b, "- %s\n", p)
			}
		}
		b.WriteString("\n| # | Bước | Dữ liệu | Kỳ vọng |\n|---|---|---|---|\n")
		for _, s := range steps {
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", s.No, qcdata.MD(s.Action), qcdata.MD(code(s.Data)), qcdata.MD(s.Expected))
		}
		if len(c.Mutations) > 0 {
			b.WriteString("\nPhản chứng (case phải đỏ khi lỗi này được cài vào):\n")
			for _, m := range c.Mutations {
				fmt.Fprintf(&b, "- %s `%s`: %s\n", m.ID, m.Failpoint, m.Title)
			}
		}
	}
	csvRaw, err = qcdata.CSV(caseHeader, rows)
	return csvRaw, []byte(b.String()), err
}

// triggers lists the job delivery paths of a case ("—" when it uses none).
func triggers(c *scenario.Case) string {
	var t []string
	for _, x := range c.Triggers() {
		if x != "" {
			t = append(t, x)
		}
	}
	if len(t) == 0 {
		return "—"
	}
	return strings.Join(t, ", ")
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

var resultHeader = []string{"Run ID", "Suite", "Case ID", "QC Key", "Title", "Requirement", "Risk", "Case Result", "Trigger",
	"Result", "Classification", "Reason", "Mutations Killed", "Mutations Total", "Started (UTC)", "Duration (s)", "Evidence"}

// Results returns results.csv (one row per execution) and results.md.
func Results(run *result.Run, cases []*scenario.Case) (csvRaw, md []byte, err error) {
	byID := map[string]*scenario.Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	agg := qcdata.ByCase(run)
	var rows [][]string
	for _, r := range agg {
		c := byID[r.CaseID]
		var key, title, risk string
		if c != nil {
			key, title, risk = c.QCKey, c.Title, c.Risk
		}
		for _, ex := range r.Executions {
			killed, total := 0, len(ex.Mutations)
			for _, m := range ex.Mutations {
				if m.Killed {
					killed++
				}
			}
			if title == "" {
				title = ex.Title
			}
			rows = append(rows, []string{run.RunID, run.Suite, r.CaseID, key, title, strings.Join(ex.Requirement, ", "), risk,
				r.Verdict, ex.Trigger, ex.Result, ex.Class, ex.Reason, fmt.Sprint(killed), fmt.Sprint(total),
				ex.StartedAt.UTC().Format(time.RFC3339), fmt.Sprintf("%.1f", ex.FinishedAt.Sub(ex.StartedAt).Seconds()),
				path.Join(ex.Dir, "case.json")})
		}
	}
	csvRaw, err = qcdata.CSV(resultHeader, rows)
	if err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Kết quả run %s\n\n", run.RunID)
	if run.Suite != "" {
		fmt.Fprintf(&b, "- Suite: %s\n", run.Suite)
	}
	fmt.Fprintf(&b, "- Thời gian: %s → %s (UTC)\n", run.StartedAt.UTC().Format(time.RFC3339), run.FinishedAt.UTC().Format(time.RFC3339))
	counts := map[string]int{}
	for _, r := range agg {
		counts[r.Verdict]++
	}
	fmt.Fprintf(&b, "- Testcase: %d pass, %d fail, %d error, %d bỏ qua\n", counts["pass"], counts["fail"], counts["error"], counts["skipped"])
	if run.Gate != nil {
		fmt.Fprintf(&b, "- **Cổng release: %s**\n\n| Luật | Kết quả | Chi tiết |\n|---|---|---|\n", run.Gate.Decision)
		for _, g := range run.Gate.Rules {
			mark := "đạt"
			if !g.Passed {
				mark = "**không đạt**"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", qcdata.MD(g.Name), mark, qcdata.MD(g.Detail))
		}
	}
	for _, n := range run.Notes {
		fmt.Fprintf(&b, "\n> %s\n", qcdata.MD(n))
	}
	b.WriteString("\n## Theo testcase\n\n| Case | Tiêu đề | Yêu cầu | Kết quả | Theo trigger | Phản chứng | Bằng chứng |\n|---|---|---|---|---|---|---|\n")
	for _, r := range agg {
		title := ""
		if c := byID[r.CaseID]; c != nil {
			title = c.Title
		}
		var trig, ev []string
		var reqs []string
		for _, ex := range r.Executions {
			t := ex.Trigger
			if t == "" {
				t = "—"
			}
			s := t + ": " + strings.ToUpper(ex.Result)
			if ex.Result != result.Pass && ex.Reason != "" {
				s += " (" + ex.Reason + ")"
			}
			trig = append(trig, s)
			ev = append(ev, "["+path.Base(ex.Dir)+"]("+path.Join("..", ex.Dir, "case.json")+")")
			reqs = ex.Requirement
		}
		mut := "—"
		if r.Mutations > 0 {
			mut = fmt.Sprintf("%d/%d đỏ", r.Killed, r.Mutations)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | **%s** | %s | %s | %s |\n", r.CaseID, qcdata.MD(title), strings.Join(reqs, ", "),
			strings.ToUpper(r.Verdict), qcdata.MD(strings.Join(trig, "\n")), mut, strings.Join(ev, " "))
	}
	b.WriteString("\nKết quả do TestKit quyết định bằng luật cứng (assertion, ngưỡng SLO, cổng release). Bằng chứng đầy đủ: `report.html` trong gói; toàn vẹn: `manifest.json` (sha256 từng tệp).\n")
	return csvRaw, []byte(b.String()), nil
}

// Export writes qc/testcases.{csv,md} and qc/results.{csv,md} into the bundle.
func Export(dir *evidence.Dir, run *result.Run, cases []*scenario.Case, rel Rel) ([]string, error) {
	// Only the cases this run is about (the suite's selection), in id order.
	in := map[string]bool{}
	for _, ex := range run.Executions {
		in[ex.CaseID] = true
	}
	var sel []*scenario.Case
	for _, c := range cases {
		if in[c.ID] {
			sel = append(sel, c)
		}
	}
	tcCSV, tcMD, err := TestCases(sel, rel)
	if err != nil {
		return nil, err
	}
	rCSV, rMD, err := Results(run, cases)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, data := range map[string][]byte{"testcases.csv": tcCSV, "testcases.md": tcMD, "results.csv": rCSV, "results.md": rMD} {
		p, err := dir.WriteFile(path.Join(Dir, name), data)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
