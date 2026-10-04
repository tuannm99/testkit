// Package result is the data model of a finished run: what the report,
// JUnit, traceability matrix, QC connectors and the release gate read.
package result

import (
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// Execution results.
const (
	Pass    = "pass"
	Fail    = "fail"
	Error   = "error"   // could not be evaluated (environment, provisioning, test bug)
	Skipped = "skipped" // missing capability, filtered out
)

// Failure classes (preliminary triage, rule based).
const (
	ClassProduct     = "product"     // an assertion failed while the environment was healthy
	ClassEnvironment = "environment" // provisioning / infrastructure / dependency of the kit failed
	ClassFlaky       = "flaky"       // failed then passed on retry with no change
	ClassTest        = "test"        // the scenario itself is invalid at run time (template, step params)
)

// Run is one `testkit run`.
type Run struct {
	RunID       string            `json:"run_id"`
	Project     string            `json:"project"`
	Suite       string            `json:"suite,omitempty"`
	Command     []string          `json:"command"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	Executions  []*Execution      `json:"executions"`
	Notes       []string          `json:"notes,omitempty"` // skipped groups, missing capabilities, ...
	Gate        *Gate             `json:"gate,omitempty"`
	Perf        []*PerfResult     `json:"perf,omitempty"`
	Chaos       []*ChaosResult    `json:"chaos,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	// Parity compares the executions of one case across triggers: the same
	// scenario through Kafka and DB poll must give the same results.
	Parity []Parity `json:"trigger_parity,omitempty"`
}

// Parity of one case across its triggers.
type Parity struct {
	CaseID   string   `json:"case_id"`
	Triggers []string `json:"triggers"`
	Match    bool     `json:"match"`
	Diffs    []string `json:"diffs,omitempty"`
}

// Execution is one case × trigger.
type Execution struct {
	ID            string           `json:"id"` // TC-ORDER-017[kafka]
	CaseID        string           `json:"case_id"`
	Title         string           `json:"title"`
	Requirement   []string         `json:"requirement"`
	Risk          string           `json:"risk"`
	Status        string           `json:"status"` // draft | approved
	Owner         string           `json:"owner,omitempty"`
	Purpose       string           `json:"purpose"`
	Preconditions []string         `json:"preconditions"`
	Service       string           `json:"service"`
	Trigger       string           `json:"trigger,omitempty"`
	NS            string           `json:"namespace"`
	File          string           `json:"file"`
	Input         any              `json:"input"`
	Vars          map[string]any   `json:"vars,omitempty"`
	Image         string           `json:"image"`
	ImageID       string           `json:"image_id,omitempty"`
	Failpoints    []string         `json:"failpoints,omitempty"`
	Steps         []StepRecord     `json:"steps"`
	Assertions    []assert.Outcome `json:"assertions"`
	Result        string           `json:"result"`
	Class         string           `json:"classification,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	FailedAt      string           `json:"failed_at,omitempty"` // step / phase where it diverged
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Dir           string           `json:"dir"` // relative to the run directory
	Artifacts     []kit.Artifact   `json:"artifacts"`
	Panels        []Panel          `json:"grafana,omitempty"`
	Timeline      []Event          `json:"-"` // written to timeline.json
	LogTail       []string         `json:"log_tail,omitempty"`
	Conclusion    []Sentence       `json:"conclusion,omitempty"`
	Mutations     []MutationResult `json:"mutations,omitempty"`
	Attempt       int              `json:"attempt"`
	Mutation      string           `json:"mutation,omitempty"` // set when this execution is a mutation run
}

// StepRecord is one executed step.
type StepRecord struct {
	N          int            `json:"n"`
	Name       string         `json:"step"`
	Label      string         `json:"name,omitempty"`
	With       map[string]any `json:"with,omitempty"`
	Result     string         `json:"result"`
	Error      string         `json:"error,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Output     any            `json:"output,omitempty"`
	Artifacts  []kit.Artifact `json:"artifacts,omitempty"`
}

// Event is one line of timeline.json.
type Event struct {
	At     time.Time `json:"at"`
	End    time.Time `json:"end,omitempty"`
	Kind   string    `json:"kind"` // provision, step, assert, drain, collect, teardown, annotation, fault
	Name   string    `json:"name"`
	Status string    `json:"status,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Panel is a captured Grafana panel.
type Panel struct {
	Name   string   `json:"name"`
	Title  string   `json:"title"`
	Image  string   `json:"image,omitempty"`
	Link   string   `json:"link"`
	Raw    []string `json:"raw"`
	CSV    []string `json:"csv,omitempty"`
	Error  string   `json:"error,omitempty"`
	Window string   `json:"window"`
	Stats  []string `json:"stats,omitempty"`
}

// Sentence of the "why it passed" conclusion; each points to evidence.
type Sentence struct {
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"`
}

// MutationResult is counter-evidence: with the system deliberately broken,
// the case must go red.
type MutationResult struct {
	ID        string   `json:"id"`
	Failpoint string   `json:"failpoint"`
	Title     string   `json:"title"`
	Trigger   string   `json:"trigger,omitempty"`
	Killed    bool     `json:"killed"` // the case went red as required
	RedIDs    []string `json:"red_assertions"`
	Expected  []string `json:"expected_red,omitempty"`
	Result    string   `json:"result"`
	Dir       string   `json:"dir"`
	Detail    string   `json:"detail,omitempty"`
}

// Gate is the release decision computed from hard rules.
type Gate struct {
	Decision string       `json:"decision"` // GO | NO-GO
	Rules    []GateRule   `json:"rules"`
	Flaky    []Quarantine `json:"quarantine,omitempty"`
}

type GateRule struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail"`
	Required string `json:"required"`
}

type Quarantine struct {
	CaseID   string `json:"case_id"`
	Reason   string `json:"reason"`
	Owner    string `json:"owner"`
	Deadline string `json:"deadline"`
	Ticket   string `json:"ticket"`
}

// PerfResult is a performance test outcome (Phase 5).
type PerfResult struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Kind     string             `json:"kind"`
	Result   string             `json:"result"`
	Metrics  map[string]float64 `json:"metrics"`
	Checks   []assert.Outcome   `json:"checks"`
	Baseline map[string]float64 `json:"baseline,omitempty"`
	Dir      string             `json:"dir"`
}

// ChaosResult is a chaos experiment outcome (Phase 5).
type ChaosResult struct {
	ID         string           `json:"id"`
	Title      string           `json:"title"`
	Fault      string           `json:"fault"`
	Result     string           `json:"result"`
	SteadyOK   bool             `json:"steady_state_ok"`
	Aborted    bool             `json:"aborted"`
	RecoveryS  float64          `json:"recovery_seconds"`
	MaxRecover float64          `json:"max_recovery_seconds"`
	Checks     []assert.Outcome `json:"checks"`
	Dir        string           `json:"dir"`
}

// Counts returns executions by result.
func (r *Run) Counts() map[string]int {
	m := map[string]int{}
	for _, e := range r.Executions {
		if e.Mutation != "" {
			continue
		}
		m[e.Result]++
	}
	return m
}
