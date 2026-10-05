package ai

import (
	"fmt"
	"strings"

	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/result"
)

// Facts are the deterministic numbers of a run; the summary file starts with
// them, so the AI text can never be the only source of a number.
func Facts(run *result.Run, man *evidence.Manifest) string {
	var b strings.Builder
	c := run.Counts()
	fmt.Fprintf(&b, "run: %s\n", run.RunID)
	if run.Suite != "" {
		fmt.Fprintf(&b, "suite: %s\n", run.Suite)
	}
	fmt.Fprintf(&b, "executions: %d pass, %d fail, %d error, %d skipped\n", c["pass"], c["fail"], c["error"], c["skipped"])
	if run.Gate != nil {
		fmt.Fprintf(&b, "gate decision: %s\ngate rules:\n", run.Gate.Decision)
		for _, r := range run.Gate.Rules {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", map[bool]string{true: "ok", false: "FAIL"}[r.Passed], r.Name, r.Detail)
		}
		for _, q := range run.Gate.Flaky {
			fmt.Fprintf(&b, "- quarantined flaky: %s (owner %s, until %s, %s)\n", q.CaseID, q.Owner, q.Deadline, q.Ticket)
		}
	} else {
		b.WriteString("gate decision: none (not a suite run)\n")
	}
	b.WriteString("cases:\n")
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		killed, total := 0, len(ex.Mutations)
		for _, m := range ex.Mutations {
			if m.Killed {
				killed++
			}
		}
		fmt.Fprintf(&b, "- %s [%s] %s: %s", ex.ID, strings.Join(ex.Requirement, ","), ex.Title, strings.ToUpper(ex.Result))
		if ex.Result != result.Pass && ex.Reason != "" {
			fmt.Fprintf(&b, " — %s", ex.Reason)
		}
		if total > 0 {
			fmt.Fprintf(&b, " (mutations red %d/%d)", killed, total)
		}
		b.WriteString("\n")
	}
	for _, p := range run.Perf {
		for _, cmp := range p.Comparisons {
			fmt.Fprintf(&b, "- perf %s %s: baseline median %.4g, now %.4g, worse by %.1f%%, p=%.3f, regression %v, stale baseline %v\n",
				p.ID, cmp.Metric, cmp.BaseMedian, cmp.CurMedian, cmp.ChangePct, cmp.PValue, cmp.Regression, cmp.Stale)
		}
	}
	for _, n := range run.Notes {
		fmt.Fprintf(&b, "- note: %s\n", n)
	}
	if man != nil {
		for _, m := range man.Mocks {
			if m.Against == "docs" {
				fmt.Fprintf(&b, "- mock %s/%s (%s, API %s) is self-faked from documentation: no provider sandbox\n", m.Service, m.Mock, m.Kind, m.APIVersion)
			}
		}
	}
	return b.String()
}

// SummaryRequest builds the request.
func SummaryRequest(run *result.Run, man *evidence.Manifest) Request {
	return Request{Task: "summary", System: SystemPrompt(), Prompt: prompt("summary") + "\n\nFacts:\n" + Facts(run, man), MaxTokens: 8000}
}

// SummaryMarkdown renders ai/summary.md: the facts, then the AI text, then
// the consistency warnings (an AI text that contradicts the gate is flagged).
func SummaryMarkdown(run *result.Run, man *evidence.Manifest, answer, provider, model string) (string, []string) {
	text := strings.TrimSpace(extractBlock(answer, "markdown"))
	var warn []string
	if run.Gate != nil {
		up := strings.ToUpper(text)
		switch {
		case run.Gate.Decision == "GO" && strings.Contains(up, "NO-GO"):
			warn = append(warn, "the AI text mentions NO-GO but the gate decision is GO")
		case run.Gate.Decision == "NO-GO" && !strings.Contains(up, "NO-GO"):
			warn = append(warn, "the gate decision is NO-GO but the AI text does not say so")
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Tóm tắt run %s\n\n## Số liệu (do TestKit tính, nguồn chính thức)\n\n```\n%s```\n\n## Tóm tắt của AI\n\n> %s Mô hình: %s (%s), prompt %s.\n\n%s\n",
		run.RunID, Facts(run, man), Disclaimer, model, provider, PromptVersion, text)
	if len(warn) > 0 {
		b.WriteString("\n## Cảnh báo kiểm tra tự động\n\n")
		for _, w := range warn {
			fmt.Fprintf(&b, "- %s — tin số liệu ở trên, không tin đoạn AI.\n", w)
		}
	}
	return b.String(), warn
}
