// Package qcdata turns cases and runs into the tool-neutral rows every QC
// exporter needs: the steps of a case (given, steps, checks with their
// expected values) and one verdict per case across its triggers.
package qcdata

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Step is one manual-readable step of a case.
type Step struct {
	No       int
	Action   string
	Data     string
	Expected string
}

// Steps lists what the case does, in order: preparation (given), actions
// (steps), then one check per assertion with its expected value and reason.
func Steps(c *scenario.Case) ([]Step, error) {
	var out []Step
	add := func(action, data, expected string) {
		out = append(out, Step{No: len(out) + 1, Action: action, Data: data, Expected: expected})
	}
	given, err := c.GivenEntries()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.ID, err)
	}
	for _, g := range given {
		add("Chuẩn bị: "+g.Key, Compact(g.Value), "")
	}
	for _, s := range c.Steps {
		action := s.Step
		if s.Name != "" {
			action += " — " + s.Name
		}
		add(action, Compact(s.With), "")
	}
	for _, e := range c.Expect {
		exp := strings.TrimSpace(e.Op + " " + Compact(e.Expected))
		if e.Why != "" {
			exp += " — " + e.Why
		}
		add("Kiểm tra "+e.ID+": "+e.Check, "", exp)
	}
	if len(out) == 0 {
		add("Chạy kịch bản "+c.ID, "", "pass")
	}
	return out, nil
}

// Compact renders a YAML value on one line.
func Compact(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// Priority maps the case risk to the usual High/Normal/Low.
func Priority(risk string) string {
	if p := map[string]string{"P0": "High", "P1": "High", "P2": "Normal", "P3": "Low"}[risk]; p != "" {
		return p
	}
	return "Normal"
}

// CaseResult is the verdict of one case across its triggers.
type CaseResult struct {
	CaseID     string
	Verdict    string // pass | fail | error | skipped
	Executions []*result.Execution
	Killed     int // mutations that turned the case red
	Mutations  int
}

// ByCase aggregates the executions of a run per case (mutation runs excluded):
// pass only if every executed trigger passed; skipped if nothing was executed.
func ByCase(run *result.Run) []CaseResult {
	idx := map[string]*CaseResult{}
	var order []string
	for _, ex := range run.Executions {
		if ex.Mutation != "" {
			continue
		}
		r, ok := idx[ex.CaseID]
		if !ok {
			r = &CaseResult{CaseID: ex.CaseID}
			idx[ex.CaseID] = r
			order = append(order, ex.CaseID)
		}
		r.Executions = append(r.Executions, ex)
		for _, m := range ex.Mutations {
			r.Mutations++
			if m.Killed {
				r.Killed++
			}
		}
	}
	sort.Strings(order)
	out := make([]CaseResult, 0, len(order))
	for _, id := range order {
		r := idx[id]
		r.Verdict = result.Skipped
		for _, ex := range r.Executions {
			switch {
			case ex.Result == result.Error && r.Verdict != result.Fail:
				r.Verdict = result.Error
			case ex.Result == result.Fail:
				r.Verdict = result.Fail
			case ex.Result == result.Pass && r.Verdict == result.Skipped:
				r.Verdict = result.Pass
			}
		}
		out = append(out, *r)
	}
	return out
}

// CSV writes rows as UTF-8 with a BOM (Excel opens it with the right
// encoding). Cells that a spreadsheet would run as a formula are prefixed
// with an apostrophe.
func CSV(header []string, rows [][]string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&b)
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, r := range rows {
		safe := make([]string, len(r))
		for i, c := range r {
			safe[i] = cell(c)
		}
		if err := w.Write(safe); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + s
	case '-':
		if len(s) > 1 && (s[1] < '0' || s[1] > '9') {
			return "'" + s
		}
	}
	return s
}

// MD escapes text for a Markdown table cell.
func MD(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	return strings.ReplaceAll(s, "\n", "<br>")
}
