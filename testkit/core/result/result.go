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
	ClassCapability  = "capability"  // skipped: the host lacks a declared privilege (NET_ADMIN, netem, docker.sock)
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
	Admission   []*Admission      `json:"admission,omitempty"` // `testkit admit` verdicts
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
	Repetitions   int              `json:"repetitions,omitempty"` // passes in a row (stability check)
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
	Chaos         *ChaosResult     `json:"chaos,omitempty"`
	Perf          *PerfResult      `json:"perf,omitempty"`
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

// PerfResult is a performance test outcome.
type PerfResult struct {
	ID          string               `json:"id"`
	Title       string               `json:"title"`
	Kind        string               `json:"kind"`
	Executor    string               `json:"executor"`
	Result      string               `json:"result"`
	Repeat      int                  `json:"repeat"`
	Metrics     map[string]float64   `json:"metrics"` // medians over repetitions
	Samples     map[string][]float64 `json:"samples"` // one value per repetition
	Thresholds  map[string]float64   `json:"thresholds"`
	BaselineKey string               `json:"baseline_key,omitempty"`
	BaselineEnv string               `json:"baseline_environment,omitempty"`
	BaselineAt  string               `json:"baseline_recorded_at,omitempty"`
	Comparisons []PerfComparison     `json:"comparisons,omitempty"`
	Note        string               `json:"note,omitempty"`
	Dir         string               `json:"dir"`
	Requirement []string             `json:"requirement"`
}

// PerfComparison mirrors perf.Comparison for the report.
type PerfComparison struct {
	Metric     string    `json:"metric"`
	BaseMedian float64   `json:"baseline_median"`
	CurMedian  float64   `json:"current_median"`
	ChangePct  float64   `json:"change_pct"`
	PValue     float64   `json:"p_value"`
	Allowed    float64   `json:"allowed_pct"`
	Regression bool      `json:"regression"`
	Verdict    string    `json:"verdict"`
	Baseline   []float64 `json:"baseline_samples"`
	Current    []float64 `json:"current_samples"`
}

// ChaosResult is a chaos experiment outcome (Phase 5).
type ChaosResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Faults      []string `json:"faults"`
	Result      string   `json:"result"`
	SteadyOK    bool     `json:"steady_state_ok"` // steady state regained after the fault
	Aborted     bool     `json:"aborted"`
	AbortWhy    string   `json:"abort_reason,omitempty"`
	RecoveryS   float64  `json:"recovery_seconds"` // -1 when not measured / not recovered
	MaxRecover  float64  `json:"max_recovery_seconds,omitempty"`
	Dir         string   `json:"dir"`
	Requirement []string `json:"requirement"`
}

// Admission statuses.
const (
	Admitted   = "admitted"   // every rule met: the case may be approved by a person
	Rejected   = "rejected"   // a rule failed: the case stays draft
	Incomplete = "incomplete" // could not be fully evaluated here (missing capability)
)

// Admission is the mutation-gate verdict for one case: it is green on the
// unbroken system, repeatably, and goes red when each declared defect is
// injected. It never approves a case by itself; a person does (`--approve`).
type Admission struct {
	CaseID    string   `json:"case_id"`
	File      string   `json:"file"`
	Status    string   `json:"status"`
	Stability int      `json:"stability"` // passes required per trigger
	Triggers  []string `json:"triggers"`
	Killed    []string `json:"mutations_killed"` // M1[kafka], ...
	Rules     []Rule   `json:"rules"`
}

// Rule is one admission rule and how the case fared.
type Rule struct {
	Name     string   `json:"name"`
	OK       bool     `json:"ok"`
	Skipped  bool     `json:"skipped,omitempty"`
	Detail   string   `json:"detail"`
	Evidence []string `json:"evidence,omitempty"`
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
