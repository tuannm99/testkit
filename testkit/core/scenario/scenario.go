// Package scenario is the test-case DSL. A test case — written by a tester,
// a developer or drafted by AI — is YAML using only registered steps and
// checks; `testkit lint` and `testkit plan` gate it before it runs.
package scenario

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Case is one test case file.
type Case struct {
	ID            string         `yaml:"id"`
	Title         string         `yaml:"title"`
	Requirement   Strings        `yaml:"requirement"`
	Risk          string         `yaml:"risk"`
	Status        string         `yaml:"status"` // draft | approved
	Owner         string         `yaml:"owner"`
	Service       string         `yaml:"service"`
	Purpose       string         `yaml:"purpose"`
	Preconditions []string       `yaml:"preconditions"`
	Vars          map[string]any `yaml:"vars"`
	Input         map[string]any `yaml:"input"`
	Trigger       Strings        `yaml:"trigger"`
	Given         yaml.Node      `yaml:"given"` // ordered mapping, expanded into steps
	Steps         []StepSpec     `yaml:"steps"`
	Expect        []ExpectSpec   `yaml:"expect"`
	Within        string         `yaml:"within"`
	Evidence      EvidenceSpec   `yaml:"evidence"`
	Failpoints    []string       `yaml:"failpoints"` // enabled in the service under test (test image)
	SUT           SUTSpec        `yaml:"sut"`
	Mutations     []Mutation     `yaml:"mutations"`
	Chaos         ChaosCase      `yaml:"chaos"`
	Perf          *PerfSpec      `yaml:"perf"`
	Tags          []string       `yaml:"tags"`
	Generated     *Generated     `yaml:"generated"` // provenance when drafted by a tool/AI
	Admission     *Admission     `yaml:"admission"` // written by `testkit admit --approve`

	File string         `yaml:"-"`
	Line map[string]int `yaml:"-"` // field -> line, for lint messages
}

// Strings accepts a scalar or a list.
type Strings []string

func (s *Strings) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = Strings{n.Value}
		return nil
	case yaml.SequenceNode:
		var l []string
		if err := n.Decode(&l); err != nil {
			return err
		}
		*s = l
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list", n.Line)
}

// StepSpec is one explicit step.
type StepSpec struct {
	Step string         `yaml:"step"`
	With map[string]any `yaml:"with"`
	Name string         `yaml:"name"` // optional label for the timeline
	Line int            `yaml:"-"`
}

func (s *StepSpec) UnmarshalYAML(n *yaml.Node) error {
	type raw StepSpec
	var r raw
	if err := decodeStrict(n, &r); err != nil {
		return err
	}
	*s = StepSpec(r)
	s.Line = n.Line
	return nil
}

// ExpectSpec is one assertion. Shorthand operator keys are accepted:
// { check: x, eq: 3 } == { check: x, op: eq, expected: 3 }.
type ExpectSpec struct {
	ID        string
	Check     string
	Op        string
	Expected  any
	Tolerance float64
	Why       string
	Final     bool
	Line      int
	hasExp    bool
}

func (e *ExpectSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expectation must be a mapping", n.Line)
	}
	e.Line = n.Line
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch {
		case k == "id":
			e.ID = v.Value
		case k == "check":
			e.Check = v.Value
		case k == "why":
			e.Why = v.Value
		case k == "op" || k == "operator":
			e.Op = v.Value
		case k == "expected":
			if err := v.Decode(&e.Expected); err != nil {
				return err
			}
			e.hasExp = true
		case k == "tolerance":
			f, err := strconv.ParseFloat(v.Value, 64)
			if err != nil {
				return fmt.Errorf("line %d: tolerance: %v", v.Line, err)
			}
			e.Tolerance = f
		case k == "final":
			e.Final = v.Value == "true"
		case assert.IsOperator(k):
			if e.Op != "" {
				return fmt.Errorf("line %d: two operators (%s and %s)", v.Line, e.Op, k)
			}
			e.Op = k
			if err := v.Decode(&e.Expected); err != nil {
				return err
			}
			e.hasExp = true
		default:
			return fmt.Errorf("line %d: unknown key %q in expectation (id, check, op/expected or <operator>, why, tolerance, final)", n.Content[i].Line, k)
		}
	}
	return nil
}

// EvidenceSpec selects extra evidence for the case.
type EvidenceSpec struct {
	Grafana  []string `yaml:"grafana"`  // panel names declared by the service
	Snapshot []string `yaml:"snapshot"` // tables to dump (default: service snapshot list)
	Logs     *bool    `yaml:"logs"`     // service logs (default true)
}

// SUTSpec tunes the service under test container for this case.
type SUTSpec struct {
	Restart  string            `yaml:"restart"`  // docker restart policy (on-failure:5 for crash tests)
	Env      map[string]string `yaml:"env"`      // extra env (templated)
	Replicas int               `yaml:"replicas"` // number of instances (competing consumers)
}

// ChaosCase lists the dependencies routed through Toxiproxy for this case.
type ChaosCase struct {
	Proxies     []string `yaml:"proxies"`
	MaxRecovery string   `yaml:"max_recovery"` // recovery time allowed after the fault is removed (gate)
}

// Mutation is a deliberate break of the system the case must detect.
type Mutation struct {
	ID        string   `yaml:"id"`
	Failpoint string   `yaml:"failpoint"`
	Title     string   `yaml:"title"`
	ExpectRed []string `yaml:"expect_red"` // assertion ids that must fail (empty: any)
}

// Admission records the mutation-gate run that preceded a person's approval.
type Admission struct {
	RunID     string   `yaml:"run_id"`
	At        string   `yaml:"at"`
	By        string   `yaml:"by"` // the person who approved
	Stability int      `yaml:"stability"`
	Killed    []string `yaml:"mutations_killed"`
	Manifest  string   `yaml:"manifest_sha256"` // seals the evidence bundle of that run
}

// Generated records provenance of drafted cases.
type Generated struct {
	By      string   `yaml:"by"`
	At      string   `yaml:"at"`
	Sources []string `yaml:"sources"`
}

func decodeStrict(n *yaml.Node, v any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(n); err != nil {
		return err
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	return nil
}

// Load parses one case file (strict: unknown keys are errors).
func Load(path string) (*Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("%s: empty file", path)
	}
	c := &Case{File: path, Line: map[string]int{}}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		c.Line[m.Content[i].Value] = m.Content[i].Line
	}
	c.File, _ = filepath.Abs(path)
	return c, nil
}

// LoadDir loads every *.yaml case below dir (suite files are skipped).
func LoadDir(dir string) ([]*Case, error) {
	var out []*Case
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		if isSuite(p) {
			return nil
		}
		c, err := Load(p)
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func isSuite(p string) bool {
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return regexp.MustCompile(`(?m)^kind:\s*Suite\s*$`).Match(raw)
}

// WithinDuration returns the assertion deadline (default 30s).
func (c *Case) WithinDuration() time.Duration {
	if c.Within == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(c.Within)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// Triggers returns the trigger dimension ("" when the case has none).
func (c *Case) Triggers() []string {
	if len(c.Trigger) == 0 {
		return []string{""}
	}
	return c.Trigger
}

// Expectations converts the specs.
func (c *Case) Expectations() []assert.Expectation {
	out := make([]assert.Expectation, len(c.Expect))
	for i, e := range c.Expect {
		out[i] = assert.Expectation{ID: e.ID, Check: e.Check, Op: e.Op, Expected: e.Expected,
			Tolerance: e.Tolerance, Why: e.Why, Final: e.Final}
	}
	return out
}

// GivenEntry is one key of `given`, in file order.
type GivenEntry struct {
	Key   string
	Value any
	Line  int
}

// GivenEntries returns the `given` mapping in file order.
func (c *Case) GivenEntries() ([]GivenEntry, error) {
	n := &c.Given
	if n.Kind == 0 {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: given must be a mapping", n.Line)
	}
	var out []GivenEntry
	for i := 0; i+1 < len(n.Content); i += 2 {
		var v any
		if err := n.Content[i+1].Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, GivenEntry{Key: n.Content[i].Value, Value: v, Line: n.Content[i].Line})
	}
	return out, nil
}

// Expand turns `given` into explicit steps (fixtures first, then mocks, then
// jobs) followed by the explicit steps. The result is what `plan` prints and
// what the orchestrator executes.
func (c *Case) Expand() ([]StepSpec, error) {
	given, err := c.GivenEntries()
	if err != nil {
		return nil, err
	}
	type prio struct {
		p int
		s []StepSpec
	}
	var groups []prio
	for _, g := range given {
		steps, p, err := expandGiven(g)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: given.%s: %w", filepath.Base(c.File), g.Line, g.Key, err)
		}
		groups = append(groups, prio{p, steps})
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].p < groups[j].p })
	var out []StepSpec
	for _, g := range groups {
		out = append(out, g.s...)
	}
	return append(out, c.Steps...), nil
}

// Given keys:
//
//	<store>.<entity>: [rows]    seed rows             (postgres.order, mongo.x, es.x, redis.x)
//	mock.<name>: [responses]    scripted responses    500 | "429(retry-after=2)" | "500*2" | reset | hang | {status: ...}
//	job: {id, duplicate, ...}   enqueue through the execution's trigger, then wait for drain
func expandGiven(g GivenEntry) ([]StepSpec, int, error) {
	src, rest, _ := strings.Cut(g.Key, ".")
	switch {
	case g.Key == "job" || g.Key == "jobs":
		jobs, ok := g.Value.([]any)
		if !ok {
			jobs = []any{g.Value}
		}
		var steps []StepSpec
		for _, j := range jobs {
			switch m := j.(type) {
			case map[string]any:
				steps = append(steps, StepSpec{Step: "trigger.enqueue", With: m, Line: g.Line})
			case string:
				// A template such as "{{ .input.job }}", resolved per execution.
				steps = append(steps, StepSpec{Step: "trigger.enqueue", With: map[string]any{"from": m}, Line: g.Line})
			default:
				return nil, 0, fmt.Errorf("job must be a mapping or a template reference")
			}
		}
		return steps, 90, nil
	case src == "mock" && rest != "":
		list, ok := g.Value.([]any)
		if !ok {
			list = []any{g.Value}
		}
		responses, err := ParseResponses(list)
		if err != nil {
			return nil, 0, err
		}
		return []StepSpec{{Step: "mock.script", With: map[string]any{"mock": rest, "responses": responses}, Line: g.Line}}, 50, nil
	case (src == "postgres" || src == "mongo" || src == "es" || src == "clickhouse" || src == "redis") && rest != "":
		var rows any = g.Value
		switch v := g.Value.(type) {
		case []any:
		case string:
			// a template such as "{{ .input.orders }}" resolving to a list
		default:
			rows = []any{v}
		}
		return []StepSpec{{Step: src + ".insert", With: map[string]any{"entity": rest, "rows": rows}, Line: g.Line}}, 10, nil
	}
	return nil, 0, fmt.Errorf("unknown given key (job, mock.<name>, <store>.<entity>)")
}

var respRe = regexp.MustCompile(`^(\d{3}|reset|hang|close)(\(([^)]*)\))?(\*(\d+))?$`)

// ParseResponses converts the response shorthand into Mock Hub responses.
//
//	500                     -> {status: 500}
//	"429(retry-after=2)"    -> {status: 429, retry_after: "2"}
//	"500*3"                 -> three times {status: 500}
//	"200(delay=300ms)"      -> {status: 200, delay: {fixed: 300ms}}
//	reset | hang | close    -> faults
//	{status: 201, body: {...}, headers: {...}}  -> as is
func ParseResponses(list []any) ([]any, error) {
	var out []any
	for _, item := range list {
		switch v := item.(type) {
		case map[string]any:
			out = append(out, v)
		case int:
			out = append(out, map[string]any{"status": v})
		case string:
			m := respRe.FindStringSubmatch(strings.TrimSpace(v))
			if m == nil {
				return nil, fmt.Errorf("bad response shorthand %q (e.g. 500, \"429(retry-after=2)\", \"500*3\", reset, hang)", v)
			}
			r := map[string]any{}
			switch m[1] {
			case "reset", "hang", "close":
				r["fault"] = m[1]
			default:
				st, _ := strconv.Atoi(m[1])
				r["status"] = st
			}
			for _, kv := range strings.Split(m[3], ",") {
				k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
				if !ok {
					continue
				}
				switch strings.ToLower(k) {
				case "retry-after", "retry_after":
					r["retry_after"] = val
				case "delay":
					r["delay"] = map[string]any{"fixed": val}
				case "fault":
					r["fault"] = val
				default:
					return nil, fmt.Errorf("unknown response option %q in %q", k, v)
				}
			}
			n := 1
			if m[5] != "" {
				n, _ = strconv.Atoi(m[5])
			}
			for i := 0; i < n; i++ {
				out = append(out, r)
			}
		default:
			return nil, fmt.Errorf("bad response %v", v)
		}
	}
	return out, nil
}

// Render resolves {{ ... }} templates in every string of v with data.
func Render(v any, data map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return RenderString(x, data)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			r, err := Render(val, data)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			r, err := Render(val, data)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// Rendered steps for one execution.
func RenderSteps(steps []StepSpec, data map[string]any) ([]kit.Step, error) {
	out := make([]kit.Step, len(steps))
	for i, s := range steps {
		w, err := Render(map[string]any(s.With), data)
		if err != nil {
			return nil, fmt.Errorf("step %d (%s): %w", i+1, s.Step, err)
		}
		wm, _ := w.(map[string]any)
		out[i] = kit.Step{Name: s.Step, With: wm, Label: s.Name, Line: s.Line}
	}
	return out, nil
}
