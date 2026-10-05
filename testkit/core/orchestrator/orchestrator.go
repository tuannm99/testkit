// Package orchestrator runs scenarios: for every case × trigger it provisions
// an isolated namespace, starts the service under test, executes the steps,
// evaluates the assertions (polling, never sleeping), drains the triggers,
// re-checks, collects evidence, and tears everything down.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/evidence"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/perf"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// Observability is implemented by the Grafana/Prometheus collectors; nil when
// the observability profile is not running (evidence then omits panels).
type Observability interface {
	Annotate(ctx context.Context, from, to time.Time, tags []string, text string) error
	Capture(ctx context.Context, dir, name string, p config.PanelSpec, from, to time.Time, vars map[string]string) result.Panel
	// WaitScraped blocks until Prometheus scraped `targets` instances of the
	// namespace at or after t (bounded); it gives panels a baseline sample
	// before the steps and a final sample after them.
	WaitScraped(ctx context.Context, ns string, targets int, t time.Time) error
}

// Options of one run.
type Options struct {
	RunID        string
	Parallel     int
	Keep         bool     // keep namespaces and containers after the run (debug)
	Triggers     []string // restrict the trigger dimension
	Retries      int      // re-run failed executions to detect flaky tests
	Mutations    bool     // also run each case's mutations (counter-evidence)
	Stability    int      // a passing execution is re-run until it passed this many times (admission)
	OnlyApproved bool     // release suites only accept approved cases
	Suite        string
	Command      []string
}

// Runner executes cases.
type Runner struct {
	Project  *config.Project
	Registry *kit.Registry
	Services map[string]*config.Service
	Obs      Observability
	Out      io.Writer
	Caps     map[string]bool // capability name -> available

	// Performance baselines (nil: no baseline comparison).
	Baselines      *perf.Store
	Fingerprint    perf.Fingerprint
	RecordBaseline bool // testkit baseline record: store instead of compare
	BaselineRuns   int  // override the number of repetitions
	Commit         string

	mu  sync.Mutex
	seq int
}

// Run executes the cases and returns the run model plus its evidence dir.
func (r *Runner) Run(ctx context.Context, cases []*scenario.Case, opt Options) (*result.Run, *evidence.Dir, error) {
	if opt.RunID == "" {
		opt.RunID = evidence.NewRunID(time.Now())
	}
	if opt.Parallel <= 0 {
		opt.Parallel = 1
	}
	dir, err := evidence.Open(r.Project.Abs(r.Project.OutDir), opt.RunID)
	if err != nil {
		return nil, nil, err
	}
	run := &result.Run{RunID: opt.RunID, Project: r.Project.Name, Suite: opt.Suite, Command: opt.Command, StartedAt: time.Now().UTC()}
	if r.Obs == nil {
		run.Notes = append(run.Notes, "observability profile not running: no Grafana panels/annotations in this run (raw logs and store snapshots are still collected)")
	}

	type job struct {
		c        *scenario.Case
		trigger  string
		mutation *scenario.Mutation
	}
	var jobs []job
	for _, c := range cases {
		if opt.OnlyApproved && c.Status != "approved" {
			run.Notes = append(run.Notes, fmt.Sprintf("%s skipped: status %s (release suites run approved cases only)", c.ID, c.Status))
			continue
		}
		for _, t := range c.Triggers() {
			if len(opt.Triggers) > 0 && t != "" && !contains(opt.Triggers, t) {
				continue
			}
			jobs = append(jobs, job{c: c, trigger: t})
			if opt.Mutations {
				for i := range c.Mutations {
					jobs = append(jobs, job{c: c, trigger: t, mutation: &c.Mutations[i]})
				}
			}
		}
	}

	results := make([]*result.Execution, len(jobs))
	// Cases that break shared infrastructure (e.g. pause Postgres) run alone,
	// after the parallel batch, so they cannot disturb other executions.
	var shared, exclusive []int
	for i, j := range jobs {
		if Exclusive(j.c) {
			exclusive = append(exclusive, i)
		} else {
			shared = append(shared, i)
		}
	}
	runOne := func(i int) {
		j := jobs[i]
		ex := r.execute(ctx, dir, opt, j.c, j.trigger, j.mutation, 1)
		for attempt := 2; attempt <= opt.Retries+1 && j.mutation == nil &&
			ex.Result != result.Pass && ex.Class == result.ClassProduct && ctx.Err() == nil; attempt++ {
			retry := r.execute(ctx, dir, opt, j.c, j.trigger, nil, attempt)
			if retry.Result == result.Pass {
				ex.Class = result.ClassFlaky
				ex.Reason = fmt.Sprintf("failed on attempt 1, passed on attempt %d (%s): quarantine and fix", attempt, retry.Dir)
				break
			}
		}
		// Stability: a pass must repeat; one red repetition makes it flaky.
		for attempt := 2; attempt <= opt.Stability && j.mutation == nil && ex.Result == result.Pass && ctx.Err() == nil; attempt++ {
			again := r.execute(ctx, dir, opt, j.c, j.trigger, nil, attempt)
			if again.Result != result.Pass {
				ex.Result, ex.Class = result.Fail, result.ClassFlaky
				ex.Reason = fmt.Sprintf("passed on attempt 1, %s on repetition %d (%s: %s): not stable", again.Result, attempt, again.Dir, again.Reason)
				break
			}
			ex.Repetitions = attempt
		}
		results[i] = ex
	}
	sem := make(chan struct{}, opt.Parallel)
	var wg sync.WaitGroup
	for _, i := range shared {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			runOne(i)
		}(i)
	}
	wg.Wait()
	for _, i := range exclusive {
		runOne(i)
	}

	// Attach mutation outcomes to the execution they challenge.
	byKey := map[string]*result.Execution{}
	for _, ex := range results {
		if ex.Mutation == "" {
			byKey[ex.CaseID+"|"+ex.Trigger] = ex
		}
	}
	for i, ex := range results {
		if ex.Mutation == "" {
			continue
		}
		base := byKey[ex.CaseID+"|"+ex.Trigger]
		m := jobs[i].mutation
		mr := result.MutationResult{ID: m.ID, Failpoint: m.Failpoint, Title: m.Title, Trigger: ex.Trigger,
			Expected: m.ExpectRed, Result: ex.Result, Dir: ex.Dir}
		for _, a := range ex.Assertions {
			if a.Result != "pass" {
				mr.RedIDs = append(mr.RedIDs, a.ID)
			}
		}
		mr.Killed = ex.Result == result.Fail && containsAll(mr.RedIDs, m.ExpectRed)
		switch {
		case ex.Result == result.Error:
			mr.Detail = "mutation run could not be evaluated: " + ex.Reason
		case !mr.Killed:
			mr.Detail = "SURVIVED: the case stayed green with the system broken — the case does not protect against this defect"
		}
		if base != nil {
			base.Mutations = append(base.Mutations, mr)
		}
	}
	run.Executions = results
	run.Parity = parity(results)
	for _, ex := range results {
		if ex.Class == result.ClassCapability && ex.Mutation == "" {
			run.Notes = append(run.Notes, fmt.Sprintf("%s skipped (not failed): %s", ex.ID, ex.Reason))
		}
	}
	for _, ex := range results {
		if ex.Chaos != nil && ex.Mutation == "" {
			run.Chaos = append(run.Chaos, ex.Chaos)
		}
		if ex.Perf != nil && ex.Mutation == "" {
			ex.Perf.Result = ex.Result
			run.Perf = append(run.Perf, ex.Perf)
		}
	}
	run.FinishedAt = time.Now().UTC()
	return run, dir, nil
}

// parity compares, per case, the result and the observed value of every
// assertion across triggers.
func parity(execs []*result.Execution) []result.Parity {
	byCase := map[string][]*result.Execution{}
	var order []string
	for _, ex := range execs {
		if ex.Mutation != "" || ex.Trigger == "" || ex.Attempt > 1 {
			continue
		}
		if _, ok := byCase[ex.CaseID]; !ok {
			order = append(order, ex.CaseID)
		}
		byCase[ex.CaseID] = append(byCase[ex.CaseID], ex)
	}
	var out []result.Parity
	for _, id := range order {
		list := byCase[id]
		comparable := len(list) >= 2
		for _, ex := range list {
			if ex.Result == result.Error || ex.Result == result.Skipped {
				comparable = false // an execution that did not run cannot be compared
			}
		}
		if !comparable {
			continue
		}
		p := result.Parity{CaseID: id, Match: true}
		ref := list[0]
		for _, ex := range list {
			p.Triggers = append(p.Triggers, ex.Trigger)
		}
		for _, ex := range list[1:] {
			if ex.Result != ref.Result {
				p.Diffs = append(p.Diffs, fmt.Sprintf("result: %s=%s, %s=%s", ref.Trigger, ref.Result, ex.Trigger, ex.Result))
			}
			got := map[string]assert.Outcome{}
			for _, a := range ex.Assertions {
				got[a.ID] = a
			}
			for _, a := range ref.Assertions {
				b, ok := got[a.ID]
				switch {
				case !ok:
					p.Diffs = append(p.Diffs, fmt.Sprintf("%s: missing in %s", a.ID, ex.Trigger))
				case a.Result != b.Result || assert.Show(a.Actual) != assert.Show(b.Actual):
					p.Diffs = append(p.Diffs, fmt.Sprintf("%s: %s=%s (%s), %s=%s (%s)", a.ID, ref.Trigger, assert.Show(a.Actual), a.Result,
						ex.Trigger, assert.Show(b.Actual), b.Result))
				}
			}
		}
		p.Match = len(p.Diffs) == 0
		out = append(out, p)
	}
	return out
}

// namespace is short, unique per execution and valid in every store.
func (r *Runner) namespace(runID string) kit.Namespace {
	r.mu.Lock()
	r.seq++
	n := r.seq
	r.mu.Unlock()
	id := strings.ReplaceAll(strings.TrimPrefix(runID, "r"), "-", "")
	return kit.Namespace(fmt.Sprintf("tk_%s_%d", strings.ToLower(id), n))
}

func (r *Runner) logf(f string, a ...any) {
	if r.Out != nil {
		r.mu.Lock()
		fmt.Fprintf(r.Out, f+"\n", a...)
		r.mu.Unlock()
	}
}

// execution holds the state of one case × trigger.
type execution struct {
	r        *Runner
	ex       *result.Execution
	env      *kit.Env
	dir      *evidence.Dir
	conns    []kit.Connector
	byName   map[string]kit.Connector
	trigger  kit.Trigger
	checkers map[string]kit.Checker
	timeline []result.Event
	tlMu     sync.Mutex
	obsVars  map[string]string
	keep     bool
	caseCtx  context.Context
	x        *experiment
	caseFile string
	data     map[string]any
}

func (e *execution) event(ev result.Event) {
	e.tlMu.Lock()
	defer e.tlMu.Unlock()
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	e.timeline = append(e.timeline, ev)
}

func (e *execution) annotate(from, to time.Time, kind, text string) {
	if e.r.Obs == nil {
		return
	}
	tags := []string{"testkit", e.env.RunID, kind, e.ex.CaseID}
	if err := e.r.Obs.Annotate(e.caseCtx, from, to, tags, fmt.Sprintf("%s %s", e.ex.ID, text)); err != nil {
		e.event(result.Event{Kind: "annotation", Name: text, Status: "error", Detail: err.Error()})
	}
}

func (r *Runner) execute(ctx context.Context, dir *evidence.Dir, opt Options, c *scenario.Case, trigger string,
	mut *scenario.Mutation, attempt int) *result.Execution {
	svc := r.Services[c.Service]
	ns := r.namespace(opt.RunID)
	id := c.ID
	if trigger != "" {
		id += "[" + trigger + "]"
	}
	caseDir := c.ID
	if trigger != "" {
		caseDir = path.Join(c.ID, trigger)
	}
	if mut != nil {
		id += "{mutation " + mut.ID + "}"
		caseDir = path.Join(c.ID, "mutations", mut.ID+suffix(trigger))
	}
	if attempt > 1 {
		caseDir = path.Join(caseDir, fmt.Sprintf("attempt-%d", attempt))
	}
	ex := &result.Execution{ID: id, CaseID: c.ID, Title: c.Title, Requirement: c.Requirement, Risk: c.Risk,
		Status: c.Status, Owner: c.Owner, Purpose: strings.TrimSpace(c.Purpose), Preconditions: c.Preconditions,
		Service: c.Service, Trigger: trigger, NS: string(ns), File: relTo(r.Project.Root, c.File),
		StartedAt: time.Now().UTC(), Dir: caseDir, Attempt: attempt}
	if mut != nil {
		ex.Mutation = mut.ID
	}
	r.logf("▶ %s  ns=%s", id, ns)

	e := &execution{r: r, ex: ex, dir: dir, byName: map[string]kit.Connector{}, checkers: map[string]kit.Checker{},
		keep: opt.Keep, caseCtx: ctx, caseFile: c.File}
	defer func() {
		ex.FinishedAt = time.Now().UTC()
		ex.Timeline = e.timeline
		if _, err := dir.WriteJSON(path.Join(caseDir, "timeline.json"), e.timeline); err != nil {
			r.logf("  write timeline: %v", err)
		}
		if _, err := dir.WriteJSON(path.Join(caseDir, "case.json"), ex); err != nil {
			r.logf("  write case.json: %v", err)
		}
		r.logf("■ %s  %s %s %s", id, strings.ToUpper(ex.Result), ex.Class, ex.Reason)
	}()

	// --- data and steps ---------------------------------------------------------
	data, err := c.Data(opt.RunID, string(ns), trigger)
	if err != nil {
		e.fail(result.Error, result.ClassTest, "render", err)
		return ex
	}
	// Address of the service under test on the network (webhooks call it).
	replicas := max(c.SUT.Replicas, 1)
	sutData := map[string]any{"host": ns.Container(svc.Name, 0, replicas)}
	for name, port := range svc.Ports {
		sutData[name] = fmt.Sprintf("http://%s:%d", ns.Container(svc.Name, 0, replicas), port)
	}
	data["sut"] = sutData
	// Test-only secrets of the mocks (e.g. to sign webhooks in a load script).
	mocks := map[string]any{}
	for name, m := range svc.Mocks {
		mocks[name] = map[string]any{"secret": m.Secret}
	}
	data["mocks"] = mocks
	e.data = data
	ex.Input = data["input"]
	ex.Vars, _ = data["vars"].(map[string]any)
	specs, err := c.Expand()
	if err != nil {
		e.fail(result.Error, result.ClassTest, "expand", err)
		return ex
	}
	steps, err := scenario.RenderSteps(specs, data)
	if err != nil {
		e.fail(result.Error, result.ClassTest, "render", err)
		return ex
	}
	_, _ = dir.WriteJSON(path.Join(caseDir, "input", "input.json"), map[string]any{"input": ex.Input, "vars": ex.Vars,
		"steps": steps, "namespace": ns, "trigger": trigger})

	// --- capabilities ---------------------------------------------------------------
	for _, s := range steps {
		if def, ok := r.Registry.Lookup(s.Name); ok {
			for _, need := range def.Requires {
				if !r.Caps[need] {
					ex.Result, ex.Reason = result.Skipped, fmt.Sprintf("step %s needs capability %s, not available on this host (testkit doctor)", s.Name, need)
					ex.Class = result.ClassCapability
					return ex
				}
			}
		}
	}

	failpoints := append([]string{}, c.Failpoints...)
	if mut != nil {
		failpoints = append(failpoints, mut.Failpoint)
	}
	ex.Failpoints = failpoints
	e.env = &kit.Env{RunID: opt.RunID, NS: ns, CaseID: c.ID, Trigger: trigger, Project: r.Project, Service: svc,
		Runner: r.Project.Endpoints(config.RunnerInNetwork()), Internal: r.Project.Endpoints(true),
		Evidence: dir, CaseDir: caseDir, Log: io.Discard, Vars: ex.Vars, Failpoint: failpoints,
		TestImage: len(failpoints) > 0, Restart: c.SUT.Restart}
	e.env.Replicas = max(c.SUT.Replicas, 1)
	if len(c.SUT.Env) > 0 {
		e.env.ExtraEnv = map[string]string{}
		for k, v := range c.SUT.Env {
			r, err := scenario.RenderString(v, data)
			if err != nil {
				e.fail(result.Error, result.ClassTest, "render", err)
				return ex
			}
			e.env.ExtraEnv[k] = fmt.Sprint(r)
		}
	}
	e.obsVars = map[string]string{"run_id": opt.RunID, "ns": string(ns)}
	e.env.Proxies = c.Chaos.Proxies
	e.checkers["experiment"] = experimentChecker{e: e}

	// --- provision ----------------------------------------------------------------------
	names := r.connectorNames(svc, trigger, c.Perf != nil && c.Perf.Executor == "k6", c.UsesUI())
	for _, n := range names {
		f, ok := r.Registry.Connectors[n]
		if !ok {
			e.fail(result.Error, result.ClassEnvironment, "provision", fmt.Errorf("no connector registered for %q", n))
			e.teardown()
			return ex
		}
		conn := f()
		start := time.Now().UTC()
		err := conn.Provision(ctx, e.env)
		e.event(result.Event{At: start, End: time.Now().UTC(), Kind: "provision", Name: conn.Name(), Status: status(err), Detail: errString(err)})
		e.conns = append(e.conns, conn) // teardown even partially provisioned connectors
		e.byName[n] = conn
		if err != nil {
			e.fail(result.Error, result.ClassEnvironment, "provision "+conn.Name(), err)
			e.collect(c, start)
			e.teardown()
			return ex
		}
		if ck, ok := conn.(kit.Checker); ok {
			for _, p := range ck.CheckPrefixes() {
				e.checkers[p] = ck
			}
		}
		if t, ok := conn.(kit.Trigger); ok && strings.HasPrefix(n, "trigger:") {
			e.trigger = t
		}
		if ii, ok := conn.(kit.ImageInfo); ok {
			ex.Image, ex.ImageID = ii.Image()
		}
	}
	if r.Obs != nil && svc.Metrics.Path != "" {
		t0 := time.Now().UTC()
		err := r.Obs.WaitScraped(ctx, string(ns), e.env.Replicas, t0)
		e.event(result.Event{At: t0, End: time.Now().UTC(), Kind: "provision", Name: "prometheus scraped the service (baseline sample)", Status: status(err), Detail: errString(err)})
	}
	caseStart := time.Now().UTC()
	e.annotate(caseStart, time.Time{}, "case", "start (ns "+string(ns)+")")

	// --- steps ------------------------------------------------------------------------------
	within := c.WithinDuration()
	stepsOK := e.runSteps(ctx, steps, within)
	if e.x != nil && e.x.loadDone != nil {
		e.stopLoad(false, 0)
	}
	var perfOuts []assert.Outcome
	if stepsOK && c.Perf != nil {
		perfOuts = e.runPerf(ctx, c)
	}

	// --- assertions: eventually, drain, final ------------------------------------------------
	if stepsOK {
		exps := c.Expectations()
		resolve := e.resolve
		t0 := time.Now().UTC()
		outs := assert.Eventually(ctx, exps, resolve, assert.Poll{Within: within}, nil)
		e.event(result.Event{At: t0, End: time.Now().UTC(), Kind: "assert", Name: "eventually", Status: summary(outs),
			Detail: fmt.Sprintf("within %s, %d attempt(s)", within, attemptsOf(outs))})
		if e.trigger != nil {
			d0 := time.Now().UTC()
			dctx, cancel := context.WithTimeout(ctx, within)
			err := e.trigger.Drain(dctx)
			cancel()
			e.event(result.Event{At: d0, End: time.Now().UTC(), Kind: "drain", Name: "trigger " + trigger, Status: status(err), Detail: errString(err)})
		}
		f0 := time.Now().UTC()
		final := make([]assert.Outcome, len(exps))
		for i, x := range exps {
			o := assert.Evaluate(ctx, x, resolve)
			o.Attempts = outs[i].Attempts + 1
			o.FirstPass = outs[i].FirstPass
			if outs[i].Result == "pass" && o.Result != "pass" {
				o.Message = fmt.Sprintf("held at %s but changed after the triggers drained: %s", outs[i].ObservedAt.Format(time.RFC3339Nano), o.Message)
			}
			final[i] = o
		}
		e.event(result.Event{At: f0, End: time.Now().UTC(), Kind: "assert", Name: "final (after drain)", Status: summary(final)})
		ex.Assertions = append(final, perfOuts...)
		e.annotate(t0, time.Now().UTC(), "assert", "assertions: "+summary(final))
	}
	caseEnd := time.Now().UTC()
	e.annotate(caseStart, caseEnd, "case", "end: "+summary(ex.Assertions))

	// --- evidence ---------------------------------------------------------------------------
	e.collect(c, caseStart)
	e.linkAssertionEvidence()

	// --- verdict ----------------------------------------------------------------------------
	if ex.Result == "" {
		failed := []string{}
		errored := []string{}
		for _, o := range ex.Assertions {
			switch o.Result {
			case "fail":
				failed = append(failed, o.ID)
			case "error":
				errored = append(errored, o.ID)
			}
		}
		switch {
		case len(errored) > 0:
			ex.Result, ex.Class = result.Error, result.ClassEnvironment
			ex.Reason = "assertion(s) could not be evaluated: " + strings.Join(errored, ", ")
			ex.FailedAt = "assert " + errored[0]
		case len(failed) > 0:
			ex.Result, ex.Class = result.Fail, result.ClassProduct
			ex.Reason = "assertion(s) failed: " + strings.Join(failed, ", ")
			ex.FailedAt = "assert " + failed[0]
		default:
			ex.Result = result.Pass
		}
	}
	if ex.Result != result.Pass {
		ex.LogTail = e.logTail(60)
	}
	ex.Conclusion = conclusion(ex)
	ex.Chaos = e.chaosResult(c)
	e.teardown()
	return ex
}

// chaosResult summarises a chaos experiment (nil when the case injected no fault).
func (e *execution) chaosResult(c *scenario.Case) *result.ChaosResult {
	var faults []string
	for _, s := range e.ex.Steps {
		if strings.HasPrefix(s.Name, "chaos.") && s.Name != "chaos.hold" && s.Name != "chaos.recover" && s.Name != "chaos.clear" {
			faults = append(faults, fmt.Sprintf("%s %s", s.Name, compactJSON(s.With)))
		}
	}
	if len(faults) == 0 {
		return nil
	}
	cr := &result.ChaosResult{ID: e.ex.ID, Title: e.ex.Title, Faults: faults, Result: e.ex.Result, Dir: e.ex.Dir,
		RecoveryS: -1, Requirement: e.ex.Requirement}
	if e.x != nil {
		cr.Aborted, cr.AbortWhy = e.x.aborted, e.x.abortWhy
		if e.x.recovered {
			cr.SteadyOK, cr.RecoveryS = true, e.x.recovery.Seconds()
		}
	}
	if d, err := time.ParseDuration(c.Chaos.MaxRecovery); err == nil {
		cr.MaxRecover = d.Seconds()
	}
	return cr
}

func compactJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// runSteps executes steps in order; it returns false on the first failure.
func (e *execution) runSteps(ctx context.Context, steps []kit.Step, within time.Duration) bool {
	for i, s := range steps {
		rec := result.StepRecord{N: i + 1, Name: s.Name, Label: s.Label, With: redact(s.With), StartedAt: time.Now().UTC()}
		res, err := e.applyStep(ctx, s, within)
		rec.FinishedAt = time.Now().UTC()
		rec.Result = "ok"
		if err != nil {
			rec.Result, rec.Error = "error", err.Error()
		}
		rec.Output, rec.Artifacts = res.Output, res.Artifacts
		e.ex.Steps = append(e.ex.Steps, rec)
		e.ex.Artifacts = append(e.ex.Artifacts, res.Artifacts...)
		kind := "step"
		if strings.HasPrefix(s.Name, "chaos.") || strings.Contains(s.Name, "fault") {
			kind = "fault"
		}
		label := s.Name
		if s.Label != "" {
			label += " — " + s.Label
		}
		e.event(result.Event{At: rec.StartedAt, End: rec.FinishedAt, Kind: kind, Name: fmt.Sprintf("%d. %s", i+1, label),
			Status: rec.Result, Detail: firstNonEmpty(rec.Error, res.Note)})
		e.annotate(rec.StartedAt, rec.FinishedAt, kind, fmt.Sprintf("step %d %s %s", i+1, s.Name, rec.Result))
		if err != nil {
			class := result.ClassEnvironment
			var te *TestError
			if errors.As(err, &te) {
				class = result.ClassTest
			}
			var ae *AssertError
			if errors.As(err, &ae) {
				e.ex.Result, e.ex.Class, e.ex.FailedAt = result.Fail, result.ClassProduct, fmt.Sprintf("step %d %s", i+1, s.Name)
				e.ex.Reason = err.Error()
				e.ex.Assertions = append(e.ex.Assertions, ae.Outcomes...)
				return false
			}
			e.fail(result.Error, class, fmt.Sprintf("step %d %s", i+1, s.Name), err)
			return false
		}
	}
	return true
}

// TestError marks an error caused by the scenario itself.
type TestError struct{ Err error }

func (t *TestError) Error() string { return t.Err.Error() }
func (t *TestError) Unwrap() error { return t.Err }

// AssertError is returned by inline assertion steps that fail.
type AssertError struct {
	Outcomes []assert.Outcome
}

func (a *AssertError) Error() string {
	var ids []string
	for _, o := range a.Outcomes {
		if o.Result != "pass" {
			ids = append(ids, o.ID+": "+o.Message)
		}
	}
	return "inline assertion failed: " + strings.Join(ids, "; ")
}

func (e *execution) applyStep(ctx context.Context, s kit.Step, within time.Duration) (kit.Result, error) {
	switch s.Name {
	case "load.start":
		return e.startLoad(ctx, s)
	case "load.stop":
		res := e.stopLoad(false, 0)
		return res, e.loadError(false)
	case "load.wait":
		res := e.stopLoad(true, kit.Dur(s.With, "timeout", 5*time.Minute))
		return res, e.loadError(true)
	case "chaos.hold":
		return e.hold(ctx, s)
	case "chaos.recover":
		return e.recoverStep(ctx, s, within)
	case "trigger.enqueue":
		if e.trigger == nil {
			return kit.Result{}, &TestError{fmt.Errorf("trigger.enqueue: the case has no trigger")}
		}
		if from, ok := s.With["from"]; ok {
			m, ok := from.(map[string]any)
			if !ok {
				return kit.Result{}, &TestError{fmt.Errorf("trigger.enqueue: from must resolve to a mapping, got %T", from)}
			}
			with := map[string]any{}
			for k, v := range m {
				with[k] = v
			}
			for k, v := range s.With {
				if k != "from" {
					with[k] = v
				}
			}
			s.With = with
		}
		job := kit.Job{ID: kit.Str(s.With, "id"), Fields: map[string]any{}, Duplicate: kit.Int(s.With, "duplicate", 0)}
		for k, v := range s.With {
			job.Fields[k] = v
		}
		if _, ok := job.Fields["order_id"]; !ok {
			job.Fields["order_id"] = job.ID
		}
		n := max(job.Duplicate, 1)
		return kit.Result{Note: fmt.Sprintf("enqueued job %s ×%d via %s", job.ID, n, e.ex.Trigger)}, e.trigger.Enqueue(ctx, job)
	case "trigger.drain":
		if e.trigger == nil {
			return kit.Result{}, &TestError{fmt.Errorf("trigger.drain: the case has no trigger")}
		}
		dctx, cancel := context.WithTimeout(ctx, kit.Dur(s.With, "timeout", within))
		defer cancel()
		return kit.Result{}, e.trigger.Drain(dctx)
	case "assert.during":
		// The expectations must hold continuously for `for` (observed, not slept).
		exps, err := inlineExpectations(s)
		if err != nil {
			return kit.Result{}, &TestError{err}
		}
		d := kit.Dur(s.With, "for", 3*time.Second)
		end := time.Now().Add(d)
		var last []assert.Outcome
		for n := 1; ; n++ {
			last = last[:0]
			for _, x := range exps {
				o := assert.Evaluate(ctx, x, e.resolve)
				o.Attempts = n
				last = append(last, o)
				if o.Result != "pass" {
					return kit.Result{Output: last}, &AssertError{Outcomes: last}
				}
			}
			if !time.Now().Before(end) {
				return kit.Result{Output: last, Note: fmt.Sprintf("held for %s (%d observations)", d, n)}, nil
			}
			select {
			case <-ctx.Done():
				return kit.Result{}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	case "wait.until", "assert":
		exps, err := inlineExpectations(s)
		if err != nil {
			return kit.Result{}, &TestError{err}
		}
		outs := assert.Eventually(ctx, exps, e.resolve, assert.Poll{Within: kit.Dur(s.With, "within", within)}, nil)
		for _, o := range outs {
			if o.Result != "pass" {
				if s.Name == "assert" {
					return kit.Result{Output: outs}, &AssertError{Outcomes: outs}
				}
				return kit.Result{Output: outs}, fmt.Errorf("wait.until %s %s %v: not reached within %s (last: %v)",
					o.Check, o.Operator, o.Expected, kit.Dur(s.With, "within", within), assert.Show(o.Actual))
			}
		}
		return kit.Result{Output: outs, Note: summary(outs)}, nil
	}
	def, ok := e.r.Registry.Lookup(s.Name)
	if !ok {
		return kit.Result{}, &TestError{fmt.Errorf("unknown step %q", s.Name)}
	}
	if errs := def.Validate(s.With); len(errs) > 0 {
		return kit.Result{}, &TestError{errors.New(strings.Join(errs, "; "))}
	}
	conn, ok := e.byName[def.Connector]
	if !ok {
		return kit.Result{}, &TestError{fmt.Errorf("step %s needs connector %s, not provisioned for service %s", s.Name, def.Connector, e.ex.Service)}
	}
	return conn.Apply(ctx, s)
}

func inlineExpectations(s kit.Step) ([]assert.Expectation, error) {
	list, ok := s.With["expect"].([]any)
	if !ok {
		if s.With["check"] != nil {
			list = []any{s.With}
		} else {
			return nil, fmt.Errorf("%s needs expect: [...] or check/op/expected", s.Name)
		}
	}
	var out []assert.Expectation
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: expectation %d is not a mapping", s.Name, i+1)
		}
		x := assert.Expectation{ID: kit.Str(m, "id"), Check: kit.Str(m, "check"), Why: kit.Str(m, "why"), Op: kit.Str(m, "op")}
		if x.ID == "" {
			x.ID = fmt.Sprintf("W%d", i+1)
		}
		if v, ok := m["expected"]; ok {
			x.Expected = v
		}
		for _, op := range assert.Operators {
			if v, ok := m[op]; ok {
				x.Op, x.Expected = op, v
			}
		}
		if x.Op == "" {
			return nil, fmt.Errorf("%s: expectation %s has no operator", s.Name, x.ID)
		}
		out = append(out, x)
	}
	return out, nil
}

// resolve routes a check to the connector owning its prefix.
func (e *execution) resolve(ctx context.Context, ref kit.CheckRef) (kit.Observation, error) {
	ck, ok := e.checkers[ref.Prefix()]
	if !ok {
		return kit.Observation{At: time.Now().UTC()}, fmt.Errorf("no connector provides checks %q in this execution", ref.Prefix())
	}
	// Positional args naming a scenario var resolve to its value (mail.to(customer)).
	ref = substituteVars(ref, e.ex.Vars)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	obs, err := ck.Check(cctx, ref)
	if obs.At.IsZero() {
		obs.At = time.Now().UTC()
	}
	return obs, err
}

func substituteVars(ref kit.CheckRef, vars map[string]any) kit.CheckRef {
	out := kit.CheckRef{Raw: ref.Raw}
	for _, s := range ref.Segments {
		ns := kit.Segment{Name: s.Name}
		for _, a := range s.Args {
			if v, ok := vars[a]; ok {
				a = fmt.Sprint(v)
			}
			ns.Args = append(ns.Args, a)
		}
		if s.KV != nil {
			ns.KV = map[string]string{}
			for k, v := range s.KV {
				if vv, ok := vars[v]; ok {
					v = fmt.Sprint(vv)
				}
				ns.KV[k] = v
			}
		}
		out.Segments = append(out.Segments, ns)
	}
	return out
}

// collect gathers evidence from every connector and Grafana.
func (e *execution) collect(c *scenario.Case, from time.Time) {
	w := kit.TimeWindow{From: from, To: time.Now().UTC().Add(time.Second)}
	t0 := time.Now().UTC()
	for _, conn := range e.conns {
		arts, err := conn.Collect(e.caseCtx, w)
		if err != nil {
			e.event(result.Event{Kind: "collect", Name: conn.Name(), Status: "error", Detail: err.Error()})
		}
		e.ex.Artifacts = append(e.ex.Artifacts, arts...)
	}
	if e.r.Obs != nil && len(c.Evidence.Grafana) > 0 {
		svc := e.r.Services[c.Service]
		// Panel window: from the baseline sample before the first step to two
		// seconds after the case, once Prometheus has scraped that far; the
		// annotations mark the exact step boundaries inside it.
		panelFrom, panelTo := from.Add(-2*time.Second), time.Now().UTC().Add(2*time.Second)
		if err := e.r.Obs.WaitScraped(e.caseCtx, string(e.env.NS), e.env.Replicas, panelTo); err != nil {
			e.event(result.Event{Kind: "collect", Name: "final scrape", Status: "error", Detail: err.Error()})
		}
		for _, name := range c.Evidence.Grafana {
			p := e.r.Obs.Capture(e.caseCtx, e.dir.Path(e.ex.Dir, "grafana"), name, svc.Panels[name], panelFrom, panelTo, e.obsVars)
			p.Image = prefixed(e.ex.Dir+"/grafana", p.Image)
			for i := range p.Raw {
				p.Raw[i] = prefixed(e.ex.Dir+"/grafana", p.Raw[i])
			}
			for i := range p.CSV {
				p.CSV[i] = prefixed(e.ex.Dir+"/grafana", p.CSV[i])
			}
			e.ex.Panels = append(e.ex.Panels, p)
		}
	}
	e.event(result.Event{At: t0, End: time.Now().UTC(), Kind: "collect", Name: "evidence", Status: "ok",
		Detail: fmt.Sprintf("%d artifact(s), %d panel(s)", len(e.ex.Artifacts), len(e.ex.Panels))})
}

func prefixed(dir, p string) string {
	if p == "" {
		return ""
	}
	return dir + "/" + p
}

// linkAssertionEvidence writes one evidence file per assertion and links the
// artifacts of the connector that produced the observed value.
func (e *execution) linkAssertionEvidence() {
	for i := range e.ex.Assertions {
		o := &e.ex.Assertions[i]
		rel := path.Join(e.ex.Dir, "assertions", o.ID+".json")
		if _, err := e.dir.WriteJSON(rel, o); err == nil {
			o.Evidence = append([]string{rel}, o.Evidence...)
		}
		ref, err := assert.ParseCheck(o.Check)
		if err != nil {
			continue
		}
		for _, a := range e.ex.Artifacts {
			if a.Source == ref.Prefix() || (ref.Prefix() == "mock" && a.Source == "mock:"+ref.Segments[1].Name) {
				o.Evidence = append(o.Evidence, a.Path)
			}
		}
		// rewrite with evidence links
		_, _ = e.dir.WriteJSON(rel, o)
	}
}

func (e *execution) teardown() {
	if e.keep {
		e.event(result.Event{Kind: "teardown", Name: "skipped (--keep)", Status: "ok"})
		return
	}
	for i := len(e.conns) - 1; i >= 0; i-- {
		t0 := time.Now().UTC()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := e.conns[i].Teardown(ctx)
		cancel()
		e.event(result.Event{At: t0, End: time.Now().UTC(), Kind: "teardown", Name: e.conns[i].Name(), Status: status(err), Detail: errString(err)})
	}
}

func (e *execution) fail(res, class, at string, err error) {
	if e.ex.Result != "" {
		return
	}
	e.ex.Result, e.ex.Class, e.ex.FailedAt, e.ex.Reason = res, class, at, err.Error()
	e.ex.LogTail = e.logTail(60)
}

// logTail returns the last lines of the service log collected so far.
func (e *execution) logTail(n int) []string {
	for _, a := range e.ex.Artifacts {
		if a.Kind == "log" && a.Source == "sut" {
			raw, err := os.ReadFile(e.dir.Path(a.Path))
			if err != nil {
				continue
			}
			lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			return lines
		}
	}
	return nil
}

// connectorNames lists the connectors of a service in provisioning order:
// stores, mocks, the trigger, then the service under test.
func (r *Runner) connectorNames(svc *config.Service, trigger string, perfK6, ui bool) []string {
	names := append([]string{}, svc.Stores.Names()...)
	kinds := map[string]bool{}
	for _, m := range svc.Mocks {
		kinds[m.Kind] = true
	}
	if kinds["http"] || kinds["webhook"] {
		names = append(names, "mock")
	}
	if kinds["smtp"] {
		names = append(names, "mail")
	}
	if kinds["socket"] {
		names = append(names, "socket")
	}
	if len(svc.Reconcile) > 0 {
		names = append(names, "reconcile")
	}
	if r.Registry.Connectors["chaos"] != nil {
		names = append(names, "chaos") // before the service: proxies must exist when it starts
	}
	if perfK6 && r.Registry.Connectors["k6"] != nil {
		names = append(names, "k6")
	}
	if ui && r.Registry.Connectors["ui"] != nil {
		names = append(names, "ui")
	}
	if trigger != "" {
		names = append(names, "trigger:"+trigger)
	}
	names = append(names, "sut")
	return names
}

func status(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func summary(outs []assert.Outcome) string {
	m := assert.Summary(outs)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	if len(parts) == 0 {
		return "no assertions"
	}
	return strings.Join(parts, ", ")
}

func attemptsOf(outs []assert.Outcome) int {
	if len(outs) == 0 {
		return 0
	}
	return outs[0].Attempts
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !contains(have, w) {
			return false
		}
	}
	return true
}

func suffix(trigger string) string {
	if trigger == "" {
		return ""
	}
	return "-" + trigger
}

func relTo(root, p string) string {
	if strings.HasPrefix(p, root+"/") {
		return strings.TrimPrefix(p, root+"/")
	}
	return p
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// redact hides values of secret-looking keys in step parameters.
func redact(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "token") {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = v
	}
	return out
}

// Exclusive reports whether a case must run alone, after the parallel batch:
// it breaks shared infrastructure (an infrastructure container rather than
// the service under test), or it measures timing under load (perf cases,
// load.start) — other executions on the shared stores would contaminate the
// measurement and make it incomparable with a baseline recorded alone.
func Exclusive(c *scenario.Case) bool {
	if c.Perf != nil {
		return true
	}
	steps, err := c.Expand()
	if err != nil {
		return false
	}
	for _, s := range steps {
		if s.Step == "load.start" {
			return true
		}
		if s.Step == "chaos.container" {
			if t := kit.Str(s.With, "target"); t != "" && t != "sut" {
				return true
			}
		}
	}
	return false
}
