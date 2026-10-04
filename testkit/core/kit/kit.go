// Package kit defines the contracts between the core (orchestrator,
// scenario, assert) and the adapters (stores, triggers, mocks, chaos,
// collectors). The core only knows these interfaces; adapters register
// themselves into a Registry that cmd/ assembles.
package kit

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
)

// Namespace isolates one test execution in every store and mock.
// One naming convention, shared by connectors and env templates.
type Namespace string

func (n Namespace) String() string           { return string(n) }
func (n Namespace) Database() string         { return string(n) }
func (n Namespace) Topic(name string) string { return string(n) + "." + name }
func (n Namespace) Group(name string) string { return string(n) + "." + name }
func (n Namespace) Index(name string) string { return string(n) + "-" + name }
func (n Namespace) KeyPrefix() string        { return string(n) + ":" }

// Container is the name of instance i (0-based) of a service under test.
func (n Namespace) Container(service string, i, replicas int) string {
	name := fmt.Sprintf("tk-%s-%s", strings.ReplaceAll(strings.TrimPrefix(string(n), "tk_"), "_", "-"), service)
	if replicas > 1 {
		name += fmt.Sprintf("-%d", i+1)
	}
	return name
}

// Env is everything a connector needs to provision one execution.
type Env struct {
	RunID     string
	NS        Namespace
	CaseID    string
	Trigger   string
	Project   *config.Project
	Service   *config.Service
	Runner    config.Endpoints // addresses as seen by the runner
	Internal  config.Endpoints // addresses as seen by containers on the network
	Evidence  *evidence.Dir
	CaseDir   string // directory of this execution, relative to the run dir
	Log       io.Writer
	Vars      map[string]any // rendered scenario vars
	Failpoint []string       // failpoints to enable in the service under test
	TestImage bool           // run the failpoint-enabled image
	Restart   string         // docker restart policy for the service under test
	Replicas  int            // instances of the service under test (competing consumers)
	ExtraEnv  map[string]string
}

// Step is one scenario action routed to a connector.
type Step struct {
	Name  string         `json:"step"`
	With  map[string]any `json:"with,omitempty"`
	Label string         `json:"name,omitempty"` // optional human label
	Line  int            `json:"-"`
}

// Artifact is a file produced as evidence.
type Artifact struct {
	Kind   string `json:"kind"`             // log, journal, snapshot, image, raw, mail, ...
	Path   string `json:"path"`             // relative to the run directory
	Title  string `json:"title"`            // human description
	Source string `json:"source,omitempty"` // connector / check prefix it documents
}

// Result of a step.
type Result struct {
	Output    any        `json:"output,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// TimeWindow bounds what Collect gathers.
type TimeWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Connector is the common interface of every adapter that takes part in an
// execution (store, mock, service under test, chaos tool...).
type Connector interface {
	Name() string
	Provision(ctx context.Context, env *Env) error
	Health(ctx context.Context) error
	Apply(ctx context.Context, step Step) (Result, error)
	Collect(ctx context.Context, w TimeWindow) ([]Artifact, error) // evidence
	Teardown(ctx context.Context) error
}

// Job is a unit of work handed to the service under test through a trigger.
type Job struct {
	ID        string         `json:"id"`
	Fields    map[string]any `json:"fields"` // available to the trigger templates as .job.*
	Duplicate int            `json:"duplicate,omitempty"`
}

// Trigger delivers jobs. Queue (Kafka) and poll (DB table) triggers share the
// same scenario: only Enqueue/Drain differ.
type Trigger interface {
	Enqueue(ctx context.Context, j Job) error
	// Drain blocks until every enqueued job reached a terminal state
	// (consumer lag = 0, no queued/running rows), or ctx expires.
	Drain(ctx context.Context) error
}

// TriggerConnector is a Connector that is also a Trigger.
type TriggerConnector interface {
	Connector
	Trigger
}

// CheckRef is a parsed check expression, e.g.
// postgres.order.o1.status, mock.payment.calls(status=201), mail.to(x).count.
type CheckRef struct {
	Raw      string    `json:"raw"`
	Segments []Segment `json:"segments"`
}

// Segment is one dotted part with optional arguments.
type Segment struct {
	Name string            `json:"name"`
	Args []string          `json:"args,omitempty"` // positional
	KV   map[string]string `json:"kv,omitempty"`   // key=value
}

func (r CheckRef) Prefix() string {
	if len(r.Segments) == 0 {
		return ""
	}
	return r.Segments[0].Name
}

// Observation is the value of a check at a point in time, with what was
// executed to obtain it (the evidence of the assertion).
type Observation struct {
	Value  any       `json:"value"`
	Source string    `json:"source"`        // query / API call that produced the value
	Raw    any       `json:"raw,omitempty"` // raw rows / documents
	At     time.Time `json:"at"`
}

// Checker resolves check expressions owned by a connector.
type Checker interface {
	CheckPrefixes() []string
	Check(ctx context.Context, ref CheckRef) (Observation, error)
}

// ImageInfo is implemented by the connector running the service under test.
type ImageInfo interface {
	Image() (ref, id string)
}

// Factory builds a fresh connector for one execution.
type Factory func() Connector

// StepDef documents a step and validates its parameters (lint).
type StepDef struct {
	Name      string
	Connector string   // connector that applies it ("" = orchestrator built-in)
	Doc       string   // one line, shown by `testkit steps`
	Required  []string // required keys of `with`
	Optional  []string // optional keys of `with`
	Requires  []string // capabilities (docker.sock, NET_ADMIN); missing => skipped
	Open      bool     // accepts extra parameters (e.g. job fields)
}

// Validate checks the `with` map of a step against the definition.
func (d StepDef) Validate(with map[string]any) []string {
	var errs []string
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, d.Required...), d.Optional...) {
		allowed[k] = true
	}
	for _, k := range d.Required {
		if _, ok := with[k]; !ok {
			errs = append(errs, fmt.Sprintf("step %s: missing parameter %q", d.Name, k))
		}
	}
	keys := make([]string, 0, len(with))
	for k := range with {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] && !d.Open {
			errs = append(errs, fmt.Sprintf("step %s: unknown parameter %q (allowed: %s)", d.Name, k,
				strings.Join(append(append([]string{}, d.Required...), d.Optional...), ", ")))
		}
	}
	return errs
}

// CheckDef documents a check prefix (lint + `testkit steps`).
type CheckDef struct {
	Prefix    string
	Connector string
	Doc       string
	Examples  []string
}

// Registry holds every connector factory, step and check known to TestKit.
type Registry struct {
	Connectors map[string]Factory
	Steps      map[string]StepDef
	Checks     map[string]CheckDef
}

func NewRegistry() *Registry {
	return &Registry{Connectors: map[string]Factory{}, Steps: map[string]StepDef{}, Checks: map[string]CheckDef{}}
}

func (r *Registry) AddConnector(name string, f Factory) { r.Connectors[name] = f }
func (r *Registry) AddStep(d StepDef)                   { r.Steps[d.Name] = d }
func (r *Registry) AddCheck(d CheckDef)                 { r.Checks[d.Prefix] = d }

// Lookup returns the definition of a step.
func (r *Registry) Lookup(step string) (StepDef, bool) {
	d, ok := r.Steps[step]
	return d, ok
}

// Param helpers for connectors.

func Str(with map[string]any, k string) string {
	v, ok := with[k]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func Int(with map[string]any, k string, def int) int {
	switch v := with[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return def
}

func Dur(with map[string]any, k string, def time.Duration) time.Duration {
	if s := Str(with, k); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}
