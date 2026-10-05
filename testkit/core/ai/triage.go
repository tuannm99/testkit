package ai

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/result"
)

// MaxTriage bounds the executions sent in one triage request.
const MaxTriage = 20

// TriageItem is the model's suggestion for one execution, after validation.
type TriageItem struct {
	Execution  string   `json:"execution"`
	RuleClass  string   `json:"rule_class"` // TestKit's own classification (authoritative)
	Category   string   `json:"category"`
	Agrees     bool     `json:"agrees_with_rule_class"`
	Summary    string   `json:"summary"`
	Reasoning  string   `json:"reasoning"`
	Evidence   []string `json:"evidence"`
	NextSteps  []string `json:"next_steps"`
	Confidence string   `json:"confidence"`
	Dropped    []string `json:"dropped_citations,omitempty"` // cited files that do not exist: not shown as evidence
}

// Triage is ai/triage.json.
type Triage struct {
	RunID         string       `json:"run_id"`
	Provider      string       `json:"provider"`
	Model         string       `json:"model"`
	PromptVersion string       `json:"prompt_version"`
	Items         []TriageItem `json:"items"`
	Overall       string       `json:"overall"`
	Problems      []string     `json:"validation_problems,omitempty"`
	Note          string       `json:"note"`
}

// Disclaimer is printed with every AI output.
const Disclaimer = "Gợi ý của AI (tham khảo). Không quyết định kết quả: pass/fail và cổng release do luật cứng của TestKit quyết định."

type candidate struct {
	ex       *result.Execution
	kind     string // red | flaky | mutation-survived
	evidence []string
}

// triageCandidates lists what deserves a triage: red or flaky executions and
// surviving mutations, with the evidence files of each.
func triageCandidates(run *result.Run, root string) []candidate {
	var out []candidate
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		kind := ""
		switch {
		case ex.Class == result.ClassFlaky:
			kind = "flaky"
		case ex.Result == result.Fail || ex.Result == result.Error:
			kind = "red"
		default:
			for _, m := range ex.Mutations {
				if !m.Killed && m.Result != result.Error {
					kind = "mutation-survived"
				}
			}
		}
		if kind == "" {
			continue
		}
		out = append(out, candidate{ex: ex, kind: kind, evidence: listFiles(root, ex.Dir, 80)})
		if len(out) == MaxTriage {
			break
		}
	}
	return out
}

// overlapping lists the executions (mutation runs included) that ran at the
// same time as ex: contention on the shared stack is a common cause of
// timing failures.
func overlapping(run *result.Run, ex *result.Execution) []string {
	var out []string
	for _, o := range run.Executions {
		if o == ex || o.StartedAt.IsZero() || o.FinishedAt.IsZero() {
			continue
		}
		if o.StartedAt.Before(ex.FinishedAt) && o.FinishedAt.After(ex.StartedAt) {
			out = append(out, o.ID)
		}
	}
	return out
}

func listFiles(root, rel string, max int) []string {
	var out []string
	_ = filepath.WalkDir(filepath.Join(root, rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(out) >= max {
			return nil
		}
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasSuffix(r, ".png") && !strings.HasSuffix(r, ".zip") {
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// TriageRequest builds the request; ok is false when nothing needs a triage.
func TriageRequest(run *result.Run, root string) (Request, bool) {
	cands := triageCandidates(run, root)
	if len(cands) == 0 {
		return Request{}, false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s", run.RunID)
	if run.Suite != "" {
		fmt.Fprintf(&b, " (suite %s)", run.Suite)
	}
	b.WriteString("\n")
	for _, c := range cands {
		ex := c.ex
		fmt.Fprintf(&b, "\n=== Execution %s (%s)\n", ex.ID, c.kind)
		fmt.Fprintf(&b, "case: %s — %s\nservice: %s, trigger: %s, requirement: %s, risk: %s\n", ex.CaseID, ex.Title, ex.Service, dash(ex.Trigger), strings.Join(ex.Requirement, ", "), ex.Risk)
		fmt.Fprintf(&b, "result: %s, rule class: %s\nreason: %s\n", ex.Result, dash(ex.Class), dash(ex.Reason))
		if ex.FailedAt != "" {
			fmt.Fprintf(&b, "failed at: %s\n", ex.FailedAt)
		}
		fmt.Fprintf(&b, "ran %s → %s (%s)\n", ex.StartedAt.UTC().Format("15:04:05"), ex.FinishedAt.UTC().Format("15:04:05"), ex.FinishedAt.Sub(ex.StartedAt).Round(time.Second))
		if ov := overlapping(run, ex); len(ov) > 0 {
			fmt.Fprintf(&b, "ran concurrently with %d other execution(s) on the same stack: %s\n", len(ov), strings.Join(ov, ", "))
		} else {
			b.WriteString("ran alone (no other execution overlapped in time)\n")
		}
		b.WriteString("assertions:\n")
		for _, a := range ex.Assertions {
			fmt.Fprintf(&b, "- %s %s: %s %s %s, actual %s (attempts %d)", a.ID, strings.ToUpper(a.Result), a.Check, a.Operator, js(a.Expected), js(a.Actual), a.Attempts)
			if a.Why != "" {
				fmt.Fprintf(&b, " — why: %s", a.Why)
			}
			if a.Message != "" {
				fmt.Fprintf(&b, " — %s", a.Message)
			}
			b.WriteString("\n")
		}
		if len(ex.Mutations) > 0 {
			b.WriteString("mutations (the case must go red when the defect is injected):\n")
			for _, m := range ex.Mutations {
				fmt.Fprintf(&b, "- %s failpoint %s (%s): case %s, red %v, required red %v, killed %v\n", m.ID, m.Failpoint, m.Title, m.Result, m.RedIDs, m.Expected, m.Killed)
			}
		}
		if raw, err := os.ReadFile(filepath.Join(root, ex.Dir, "timeline.json")); err == nil {
			var tl []result.Event
			if json.Unmarshal(raw, &tl) == nil {
				b.WriteString("timeline:\n")
				for _, e := range tl {
					if e.Kind == "teardown" {
						continue
					}
					fmt.Fprintf(&b, "- %s %s %s %s %s\n", e.At.Format("15:04:05.000"), e.Kind, e.Name, dash(e.Status), trim(e.Detail, 200))
				}
			}
		}
		if len(ex.LogTail) > 0 {
			b.WriteString("service log (last lines):\n")
			tail := ex.LogTail
			if len(tail) > 40 {
				tail = tail[len(tail)-40:]
			}
			for _, l := range tail {
				fmt.Fprintf(&b, "  %s\n", trim(l, 300))
			}
		}
		fmt.Fprintf(&b, "evidence files:\n  %s\n", strings.Join(c.evidence, "\n  "))
	}
	return Request{Task: "triage", System: SystemPrompt(), Prompt: prompt("triage") + "\n\n" + b.String(), MaxTokens: 16000}, true
}

// ParseTriage validates the model's answer against what was asked: known
// executions only, a known category, cited files that exist. TestKit's own
// classification is attached and stays authoritative.
func ParseTriage(run *result.Run, root, answer string) (*Triage, error) {
	var raw struct {
		Items []struct {
			Execution  string   `json:"execution"`
			Category   string   `json:"category"`
			Agrees     bool     `json:"agrees_with_rule_class"`
			Summary    string   `json:"summary"`
			Reasoning  string   `json:"reasoning"`
			Evidence   []string `json:"evidence"`
			NextSteps  []string `json:"next_steps"`
			Confidence string   `json:"confidence"`
		} `json:"items"`
		Overall string `json:"overall"`
	}
	if err := json.Unmarshal([]byte(extractBlock(answer, "json")), &raw); err != nil {
		return nil, fmt.Errorf("triage answer is not the requested JSON: %w", err)
	}
	cands := map[string]candidate{}
	for _, c := range triageCandidates(run, root) {
		cands[c.ex.ID] = c
	}
	t := &Triage{RunID: run.RunID, PromptVersion: PromptVersion, Overall: raw.Overall, Note: Disclaimer}
	seen := map[string]bool{}
	cats := map[string]bool{"product": true, "environment": true, "test": true, "flaky": true, "test-weakness": true, "unknown": true}
	confs := map[string]bool{"low": true, "medium": true, "high": true}
	for _, it := range raw.Items {
		c, ok := cands[it.Execution]
		if !ok {
			t.Problems = append(t.Problems, fmt.Sprintf("answer mentions %q, which was not asked about: ignored", it.Execution))
			continue
		}
		seen[it.Execution] = true
		item := TriageItem{Execution: it.Execution, RuleClass: c.ex.Class, Category: it.Category, Agrees: it.Agrees, Summary: it.Summary,
			Reasoning: it.Reasoning, NextSteps: it.NextSteps, Confidence: it.Confidence}
		if c.kind == "mutation-survived" && item.RuleClass == "" {
			item.RuleClass = "mutation survived"
		}
		if !cats[item.Category] {
			t.Problems = append(t.Problems, fmt.Sprintf("%s: unknown category %q → unknown", it.Execution, item.Category))
			item.Category = "unknown"
		}
		if !confs[item.Confidence] {
			item.Confidence = "low"
		}
		have := map[string]bool{}
		for _, f := range c.evidence {
			have[f] = true
		}
		for _, f := range it.Evidence {
			f = path.Clean(strings.TrimPrefix(f, "./"))
			if have[f] {
				item.Evidence = append(item.Evidence, f)
			} else {
				item.Dropped = append(item.Dropped, f)
			}
		}
		if len(item.Dropped) > 0 {
			t.Problems = append(t.Problems, fmt.Sprintf("%s: %d cited file(s) do not exist in the bundle: dropped", it.Execution, len(item.Dropped)))
		}
		t.Items = append(t.Items, item)
	}
	for id := range cands {
		if !seen[id] {
			t.Problems = append(t.Problems, fmt.Sprintf("no suggestion for %s", id))
		}
	}
	sort.Strings(t.Problems)
	return t, nil
}

// Markdown renders ai/triage.md.
func (t *Triage) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Gợi ý phân loại lỗi — run %s\n\n> %s\n>\n> Mô hình: %s (%s), prompt %s.\n\n", t.RunID, Disclaimer, t.Model, t.Provider, t.PromptVersion)
	for _, it := range t.Items {
		fmt.Fprintf(&b, "## %s\n\n- Phân loại theo luật TestKit: **%s**\n- AI gợi ý: **%s** (độ tin cậy %s)%s\n\n%s\n\n", it.Execution, dash(it.RuleClass),
			it.Category, it.Confidence, map[bool]string{true: "", false: " — khác với phân loại theo luật"}[it.Agrees], it.Summary)
		if it.Reasoning != "" {
			fmt.Fprintf(&b, "%s\n\n", it.Reasoning)
		}
		if len(it.Evidence) > 0 {
			b.WriteString("Bằng chứng được trích:\n")
			for _, e := range it.Evidence {
				fmt.Fprintf(&b, "- [%s](../%s)\n", e, e)
			}
			b.WriteString("\n")
		}
		if len(it.NextSteps) > 0 {
			b.WriteString("Việc nên làm:\n")
			for _, s := range it.NextSteps {
				fmt.Fprintf(&b, "- %s\n", s)
			}
			b.WriteString("\n")
		}
	}
	if t.Overall != "" {
		fmt.Fprintf(&b, "## Nhận xét chung\n\n%s\n\n", t.Overall)
	}
	if len(t.Problems) > 0 {
		b.WriteString("## Kiểm tra câu trả lời của AI\n\n")
		for _, p := range t.Problems {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func js(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
