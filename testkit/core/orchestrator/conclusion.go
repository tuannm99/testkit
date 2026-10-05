package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/result"
)

var opText = map[string]string{
	"eq": "=", "ne": "≠", "gt": ">", "gte": "≥", "lt": "<", "lte": "≤", "in": "∈", "not_in": "∉",
	"contains": "chứa", "not_contains": "không chứa", "matches": "khớp regex", "exists": "tồn tại",
	"not_exists": "không tồn tại", "between": "trong khoảng", "approx": "≈", "empty": "rỗng",
	"not_empty": "không rỗng", "len_eq": "có số phần tử =",
}

// OpText renders an operator for humans.
func OpText(op string) string {
	if t, ok := opText[op]; ok {
		return t
	}
	return op
}

// conclusion builds the "why it passed / why it failed" text from the
// assertions only: every sentence quotes the reason (why), the expected and
// the observed value, and points to the evidence file. No free text, no AI.
func conclusion(ex *result.Execution) []result.Sentence {
	var out []result.Sentence
	start := ex.StartedAt
	if len(ex.Steps) > 0 {
		start = ex.Steps[0].StartedAt
	}
	pass := 0
	for _, a := range ex.Assertions {
		if a.Result == "pass" {
			pass++
		}
	}
	switch ex.Result {
	case result.Pass:
		out = append(out, result.Sentence{Text: fmt.Sprintf("PASS: %d/%d assertion đạt sau khi trigger đã xử lý hết (drain) — giá trị cuối cùng được đo lại, không chỉ lần đầu đạt.", pass, len(ex.Assertions))})
	case result.Skipped:
		return []result.Sentence{{Text: "SKIPPED: " + ex.Reason}}
	default:
		out = append(out, result.Sentence{Text: fmt.Sprintf("%s tại %s: %s", strings.ToUpper(ex.Result), ex.FailedAt, ex.Reason)})
	}
	for _, a := range ex.Assertions {
		verdict := "đạt"
		if a.Result != "pass" {
			verdict = "KHÔNG đạt"
		}
		exp := ""
		if assert.NeedsExpected(a.Operator) {
			exp = " " + assert.Show(a.Expected)
		}
		when := ""
		if !a.ObservedAt.IsZero() {
			when = " lúc " + a.ObservedAt.UTC().Format("15:04:05.000Z")
		}
		if !a.FirstPass.IsZero() && a.Result == "pass" {
			when += fmt.Sprintf(" (đạt lần đầu %s sau bước đầu tiên)", a.FirstPass.Sub(start).Round(10*time.Millisecond))
		}
		text := fmt.Sprintf("%s %s — %s. Kỳ vọng `%s` %s%s; thực tế = %s%s.", a.ID, verdict, strings.TrimSpace(a.Why),
			a.Check, OpText(a.Operator), exp, assert.Show(a.Actual), when)
		if a.Message != "" && a.Result != "pass" {
			text += " " + a.Message
		}
		out = append(out, result.Sentence{Text: text, Evidence: a.Evidence})
	}
	return out
}
