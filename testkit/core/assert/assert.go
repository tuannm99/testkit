// Package assert evaluates scenario expectations. Pass/fail is decided here,
// by explicit operators on observed values — never by heuristics or AI.
package assert

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Operators supported in expect[].op (and as shorthand keys).
var Operators = []string{"eq", "ne", "gt", "gte", "lt", "lte", "in", "not_in", "contains", "not_contains",
	"matches", "exists", "not_exists", "between", "approx", "empty", "not_empty", "len_eq"}

// IsOperator reports whether name is a known operator.
func IsOperator(name string) bool {
	for _, o := range Operators {
		if o == name {
			return true
		}
	}
	return false
}

// NeedsExpected reports whether op takes an expected value.
func NeedsExpected(op string) bool {
	switch op {
	case "exists", "not_exists", "empty", "not_empty":
		return false
	}
	return true
}

// Compare applies op to (actual, expected). tolerance is used by approx.
func Compare(op string, actual, expected any, tolerance float64) (bool, error) {
	switch op {
	case "exists":
		return actual != nil, nil
	case "not_exists":
		return actual == nil, nil
	case "empty":
		return length(actual) == 0, nil
	case "not_empty":
		return length(actual) > 0, nil
	case "len_eq":
		n, ok := toFloat(expected)
		if !ok {
			return false, fmt.Errorf("len_eq needs a number")
		}
		return float64(length(actual)) == n, nil
	case "eq":
		return equal(actual, expected), nil
	case "ne":
		return !equal(actual, expected), nil
	case "gt", "gte", "lt", "lte":
		a, ok1 := toFloat(actual)
		e, ok2 := toFloat(expected)
		if !ok1 || !ok2 {
			return false, fmt.Errorf("%s needs numbers, got %v and %v", op, show(actual), show(expected))
		}
		switch op {
		case "gt":
			return a > e, nil
		case "gte":
			return a >= e, nil
		case "lt":
			return a < e, nil
		default:
			return a <= e, nil
		}
	case "between":
		bounds, ok := expected.([]any)
		if !ok || len(bounds) != 2 {
			return false, fmt.Errorf("between needs [min, max]")
		}
		a, ok1 := toFloat(actual)
		lo, ok2 := toFloat(bounds[0])
		hi, ok3 := toFloat(bounds[1])
		if !ok1 || !ok2 || !ok3 {
			return false, fmt.Errorf("between needs numbers")
		}
		return a >= lo && a <= hi, nil
	case "approx":
		a, ok1 := toFloat(actual)
		e, ok2 := toFloat(expected)
		if !ok1 || !ok2 {
			return false, fmt.Errorf("approx needs numbers")
		}
		return math.Abs(a-e) <= tolerance, nil
	case "in", "not_in":
		list, ok := expected.([]any)
		if !ok {
			return false, fmt.Errorf("%s needs a list", op)
		}
		found := false
		for _, e := range list {
			if equal(actual, e) {
				found = true
				break
			}
		}
		return found == (op == "in"), nil
	case "contains", "not_contains":
		found := false
		switch a := actual.(type) {
		case []any:
			for _, x := range a {
				if equal(x, expected) {
					found = true
				}
			}
		default:
			found = strings.Contains(fmt.Sprint(actual), fmt.Sprint(expected))
		}
		return found == (op == "contains"), nil
	case "matches":
		re, err := regexp.Compile(fmt.Sprint(expected))
		if err != nil {
			return false, err
		}
		return re.MatchString(fmt.Sprint(actual)), nil
	}
	return false, fmt.Errorf("unknown operator %q", op)
}

func length(v any) int {
	if v == nil {
		return 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.String, reflect.Array:
		return rv.Len()
	}
	return 1
}

// equal compares loosely: numbers by value, everything else by string form.
func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	fa, ok1 := toFloat(a)
	fb, ok2 := toFloat(b)
	if ok1 && ok2 {
		return fa == fb
	}
	return show(a) == show(b)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		return 0, false
	}
	return 0, false
}

// show renders a value for messages and the report.
func show(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

// Show is the exported value renderer.
func Show(v any) string { return show(v) }

// ParseCheck parses a check expression such as
// mock.payment.calls(status=201) or mail.to(cust@x).count.
func ParseCheck(expr string) (kit.CheckRef, error) {
	ref := kit.CheckRef{Raw: expr}
	s := strings.TrimSpace(expr)
	for len(s) > 0 {
		i := 0
		for i < len(s) && s[i] != '.' && s[i] != '(' {
			i++
		}
		seg := kit.Segment{Name: s[:i]}
		if seg.Name == "" {
			return ref, fmt.Errorf("check %q: empty segment", expr)
		}
		if !segRe.MatchString(seg.Name) {
			return ref, fmt.Errorf("check %q: invalid segment %q", expr, seg.Name)
		}
		s = s[i:]
		if strings.HasPrefix(s, "(") {
			end := strings.IndexByte(s, ')')
			if end < 0 {
				return ref, fmt.Errorf("check %q: missing ')'", expr)
			}
			for _, a := range splitArgs(s[1:end]) {
				if k, v, ok := strings.Cut(a, "="); ok {
					if seg.KV == nil {
						seg.KV = map[string]string{}
					}
					seg.KV[strings.TrimSpace(k)] = strings.TrimSpace(v)
				} else if a != "" {
					seg.Args = append(seg.Args, a)
				}
			}
			s = s[end+1:]
		}
		ref.Segments = append(ref.Segments, seg)
		if strings.HasPrefix(s, ".") {
			s = s[1:]
			if s == "" {
				return ref, fmt.Errorf("check %q: trailing '.'", expr)
			}
		} else if s != "" {
			return ref, fmt.Errorf("check %q: unexpected %q", expr, s)
		}
	}
	if len(ref.Segments) < 2 {
		return ref, fmt.Errorf("check %q: expected <source>.<...>", expr)
	}
	return ref, nil
}

var segRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func splitArgs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// Expectation is one assertion of a scenario (already rendered).
type Expectation struct {
	ID        string  `json:"id"`
	Check     string  `json:"check"`
	Op        string  `json:"operator"`
	Expected  any     `json:"expected"`
	Tolerance float64 `json:"tolerance,omitempty"`
	Why       string  `json:"why"`
	// Final: evaluate only after the triggers are drained (e.g. "no extra call").
	Final bool `json:"final,omitempty"`
}

// Outcome of one assertion; written into case.json.
type Outcome struct {
	ID         string    `json:"id"`
	Result     string    `json:"result"` // pass | fail | error | skipped
	Check      string    `json:"check"`
	Operator   string    `json:"operator"`
	Expected   any       `json:"expected"`
	Actual     any       `json:"actual"`
	Why        string    `json:"why"`
	Evidence   []string  `json:"evidence"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	Attempts   int       `json:"attempts"`
	FirstPass  time.Time `json:"first_pass,omitempty"`
	Message    string    `json:"message,omitempty"`
}

// Resolver resolves a check reference to an observation.
type Resolver func(ctx context.Context, ref kit.CheckRef) (kit.Observation, error)

// Evaluate observes one expectation once.
func Evaluate(ctx context.Context, e Expectation, resolve Resolver) Outcome {
	o := Outcome{ID: e.ID, Check: e.Check, Operator: e.Op, Expected: e.Expected, Why: e.Why}
	ref, err := ParseCheck(e.Check)
	if err != nil {
		o.Result, o.Message = "error", err.Error()
		return o
	}
	obs, err := resolve(ctx, ref)
	o.ObservedAt = obs.At
	o.Source = obs.Source
	o.Actual = obs.Value
	if err != nil {
		o.Result, o.Message = "error", err.Error()
		return o
	}
	ok, err := Compare(e.Op, obs.Value, e.Expected, e.Tolerance)
	switch {
	case err != nil:
		o.Result, o.Message = "error", err.Error()
	case ok:
		o.Result = "pass"
	default:
		o.Result = "fail"
		o.Message = fmt.Sprintf("expected %s %s %s, got %s", e.Check, e.Op, show(e.Expected), show(obs.Value))
	}
	return o
}

// Poll is the polling policy of Eventually (no sleep-based waiting in tests).
type Poll struct {
	Within   time.Duration
	Interval time.Duration // first interval, doubled up to MaxInterval
	MaxInt   time.Duration
}

// Eventually re-evaluates every expectation until all pass or the deadline
// expires. It returns the last outcome of each expectation (in input order).
func Eventually(ctx context.Context, exps []Expectation, resolve Resolver, p Poll, onRound func([]Outcome)) []Outcome {
	if p.Interval == 0 {
		p.Interval = 200 * time.Millisecond
	}
	if p.MaxInt == 0 {
		p.MaxInt = time.Second
	}
	deadline := time.Now().Add(p.Within)
	out := make([]Outcome, len(exps))
	firstPass := make([]time.Time, len(exps))
	attempts := 0
	interval := p.Interval
	for {
		attempts++
		all := true
		for i, e := range exps {
			o := Evaluate(ctx, e, resolve)
			o.Attempts = attempts
			if o.Result == "pass" && firstPass[i].IsZero() {
				firstPass[i] = o.ObservedAt
			}
			o.FirstPass = firstPass[i]
			out[i] = o
			if o.Result != "pass" {
				all = false
			}
		}
		if onRound != nil {
			onRound(out)
		}
		if all || !time.Now().Before(deadline) || ctx.Err() != nil {
			return out
		}
		wait := min(interval, time.Until(deadline))
		select {
		case <-ctx.Done():
			return out
		case <-time.After(wait):
		}
		interval = min(interval*2, p.MaxInt)
	}
}

// Summary counts outcomes by result.
func Summary(out []Outcome) map[string]int {
	m := map[string]int{}
	for _, o := range out {
		m[o.Result]++
	}
	return m
}

// SortedIDs returns ids in order (helper for reports).
func SortedIDs(out []Outcome) []string {
	ids := make([]string, len(out))
	for i, o := range out {
		ids[i] = o.ID
	}
	sort.Strings(ids)
	return ids
}
