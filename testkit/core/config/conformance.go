package config

import (
	"sort"

	"gopkg.in/yaml.v3"
)

// Conformance is the `conformance:` section of a service descriptor: what a
// service must tell TestKit so that the standard case pack (duplicate
// delivery, poison message, crash mid-job, dependency fault, out of order,
// steady load) can be generated for it by `testkit gen`.
//
// It describes the service's own business, once: how to create the data of a
// job, what "done" looks like, which side effects must happen exactly once.
// The patterns bring the disruptive scenarios; nobody computes expected values
// case by case.
type Conformance struct {
	Owner       string   `yaml:"owner"`       // owner written into the generated cases
	Requirement string   `yaml:"requirement"` // requirement id (default REQ-STD)
	Within      string   `yaml:"within"`      // time budget of a small case (default 45s)
	Evidence    []string `yaml:"evidence"`    // Grafana panels attached to every generated case
	Triggers    []string `yaml:"triggers"`    // triggers to cover (default: every trigger of the service)

	// Given creates the business data of ONE job (lists of rows per step, same syntax as a
	// case's `given`). `{{ .key }}` is the job's unique key (k1, k2...). Runtime templates
	// ({{ .ns }}, {{ .vars.x }}) are kept.
	Given yaml.Node `yaml:"given"`
	// Shared is given once per case (mock scripts, anything not per job).
	Shared yaml.Node `yaml:"shared"`
	// Job adds fields to the job handed to the trigger (`.job.<field>` in trigger templates).
	Job map[string]any `yaml:"job"`

	// Done: what holds once a job is complete. Effects: side effects that must happen
	// exactly once. An item is { check, why } plus either an operator (eq: 1) or
	// `per_job: N` (expected = N × number of jobs). A check containing `{{ .key }}`
	// is asserted for every job of small cases; others are totals (also used in big cases).
	Done    []map[string]any `yaml:"done"`
	Effects []map[string]any `yaml:"effects"`

	Patterns map[string]*ConformancePattern `yaml:"patterns"`
}

// ConformancePattern configures one standard pattern. A pattern is generated only if
// its block exists (an empty block `{}` takes the defaults).
type ConformancePattern struct {
	Triggers  []string              `yaml:"triggers"`  // restrict this pattern (default: conformance.triggers)
	Jobs      int                   `yaml:"jobs"`      // number of jobs (default depends on the pattern)
	Mutations []ConformanceMutation `yaml:"mutations"` // failpoints that must turn the case red

	// duplicate-delivery
	Copies int `yaml:"copies"` // deliveries of the same job (default 3)

	// poison-message
	Body   string                `yaml:"body"`    // an unreadable message body (default "{not json")
	DBPoll *ConformancePoisonSQL `yaml:"db-poll"` // db-poll has no message body: SQL inserting an unusable job

	// crash-mid-job
	Failpoint string `yaml:"failpoint"` // failpoint that kills the service (fires once)
	Log       string `yaml:"log"`       // log line proving the crash happened
	Restart   string `yaml:"restart"`   // restart policy (default on-failure:3)

	// dependency-fault
	Faults       []ConformanceFault `yaml:"faults"`
	Rate         int                `yaml:"rate"`          // jobs per second of the background load (default 10)
	MaxRecovery  string             `yaml:"max_recovery"`  // allowed recovery time after the fault is removed (default 30s)
	AbortBacklog int                `yaml:"abort_backlog"` // blast radius: stop the experiment above this backlog (0 = none)

	// steady-load
	Duration   string         `yaml:"duration"`
	Warmup     string         `yaml:"warmup"`
	Repeat     int            `yaml:"repeat"`
	Thresholds map[string]any `yaml:"thresholds"` // the team's SLOs (p95_ms, error_rate, min_throughput, max_dropped)
	Baseline   string         `yaml:"baseline"`   // baseline key prefix (default std-<service>-<rate>rps)
}

// ConformancePoisonSQL is the db-poll form of a poison message.
type ConformancePoisonSQL struct {
	SQL string `yaml:"sql"`
}

// ConformanceFault is one fault injected through a proxied dependency (chaos.proxies).
type ConformanceFault struct {
	Proxy string         `yaml:"proxy"`
	Fault string         `yaml:"fault"` // a chaos step of Toxiproxy: down | latency | timeout | reset_peer | bandwidth | slicer
	With  map[string]any `yaml:"with"`  // extra parameters of that step (latency: 1500)
	For   string         `yaml:"for"`   // how long the fault is held (default 5s)
	// Mutations that must turn this fault's case red (a failpoint is specific to a fault:
	// outage_is_failure only matters for a database outage). Default: the pattern's mutations.
	Mutations []ConformanceMutation `yaml:"mutations"`
}

// ConformanceMutation maps a failpoint to the assertion groups that must turn red.
type ConformanceMutation struct {
	Failpoint string   `yaml:"failpoint"`
	Title     string   `yaml:"title"`
	ExpectRed []string `yaml:"expect_red"` // assertion groups of the pattern (see docs), e.g. effects, dlq
}

// Pattern names.
const (
	PatDuplicate = "duplicate-delivery"
	PatPoison    = "poison-message"
	PatCrash     = "crash-mid-job"
	PatFault     = "dependency-fault"
	PatOrder     = "out-of-order"
	PatLoad      = "steady-load"
)

// PatternNames lists the standard patterns in generation order.
var PatternNames = []string{PatDuplicate, PatPoison, PatCrash, PatFault, PatOrder, PatLoad}

var faultSteps = map[string]bool{"down": true, "latency": true, "timeout": true, "reset_peer": true, "bandwidth": true, "slicer": true}

// validateConformance checks the structure and the references to the rest of the
// descriptor (operators and assertion groups are checked by the generator).
func (s *Service) validateConformance(add func(string, ...any)) {
	c := s.Conformance
	if c == nil {
		return
	}
	known := map[string]bool{}
	for _, n := range PatternNames {
		known[n] = true
	}
	for name := range c.Patterns {
		if !known[name] {
			add("conformance.patterns.%s: unknown pattern (%v)", name, PatternNames)
		}
	}
	if len(c.Patterns) == 0 {
		add("conformance.patterns: declare at least one pattern (an empty block {} takes the defaults)")
	}
	if c.Given.Kind != yaml.MappingNode {
		add("conformance.given: required (the rows that create ONE job's business data, keyed by step)")
	}
	checkTriggers := func(where string, list []string) {
		for _, t := range list {
			if _, ok := s.Triggers[t]; !ok {
				add("%s: trigger %q is not declared in triggers", where, t)
			}
		}
	}
	checkTriggers("conformance.triggers", c.Triggers)
	for i, kind := range [][]map[string]any{c.Done, c.Effects} {
		field := []string{"done", "effects"}[i]
		for j, it := range kind {
			if _, ok := it["check"].(string); !ok {
				add("conformance.%s[%d]: check is required", field, j)
			}
		}
	}
	names := make([]string, 0, len(c.Patterns))
	for n := range c.Patterns {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.Patterns[name]
		if p == nil {
			p = &ConformancePattern{}
			c.Patterns[name] = p
		}
		where := "conformance.patterns." + name
		checkTriggers(where+".triggers", p.Triggers)
		for i, m := range p.Mutations {
			if _, ok := s.Failpoints[m.Failpoint]; !ok {
				add("%s.mutations[%d]: failpoint %q is not declared in failpoints", where, i, m.Failpoint)
			}
			if m.Title == "" {
				add("%s.mutations[%d]: title is required", where, i)
			}
		}
		switch name {
		case PatCrash:
			if _, ok := s.Failpoints[p.Failpoint]; !ok {
				add("%s.failpoint: %q is not declared in failpoints", where, p.Failpoint)
			}
			if p.Log == "" {
				add("%s.log: the log line proving the crash happened is required", where)
			}
		case PatFault:
			if len(p.Faults) == 0 {
				add("%s.faults: declare at least one fault ({proxy, fault})", where)
			}
			for i, f := range p.Faults {
				for j, m := range f.Mutations {
					if _, ok := s.Failpoints[m.Failpoint]; !ok {
						add("%s.faults[%d].mutations[%d]: failpoint %q is not declared in failpoints", where, i, j, m.Failpoint)
					}
				}
				if _, ok := s.Chaos.Proxies[f.Proxy]; !ok {
					add("%s.faults[%d]: proxy %q is not declared in chaos.proxies", where, i, f.Proxy)
				}
				if !faultSteps[f.Fault] {
					add("%s.faults[%d]: fault %q must be one of down | latency | timeout | reset_peer | bandwidth | slicer", where, i, f.Fault)
				}
			}
			if s.Perf.Fixture == "" {
				add("%s: needs perf.fixture (SQL creating the data of jobs prefix+from..to)", where)
			}
		case PatLoad:
			if len(p.Thresholds) == 0 {
				add("%s.thresholds: the SLOs are the team's to set (p95_ms, error_rate, min_throughput, max_dropped)", where)
			}
			if s.Perf.Fixture == "" || s.Perf.Completion == "" {
				add("%s: needs perf.fixture and perf.completion (the trigger executor measures end-to-end latency with them)", where)
			}
		}
	}
}
