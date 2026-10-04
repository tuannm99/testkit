package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

// experiment is the state of a chaos experiment / load inside one execution:
// background open-model load, fault hold with abort conditions, recovery time.
type experiment struct {
	mu        sync.Mutex
	loadStop  context.CancelFunc
	loadDone  chan struct{}
	sent      atomic.Int64
	late      atomic.Int64 // arrivals the generator could not issue on time (generator saturated)
	errors    atomic.Int64
	aborted   bool
	abortWhy  string
	recovery  time.Duration
	recovered bool
	faultAt   time.Time
	clearedAt time.Time
}

// clearer is implemented by the chaos connector.
type clearer interface {
	ClearAll(ctx context.Context) error
}

// startLoad enqueues jobs at a fixed arrival rate (open model: arrivals do
// not wait for previous jobs, so slowness cannot hide behind fewer requests).
func (e *execution) startLoad(ctx context.Context, s kit.Step) (kit.Result, error) {
	if e.trigger == nil {
		return kit.Result{}, &TestError{fmt.Errorf("load.start needs a trigger")}
	}
	rate := kit.Int(s.With, "rate", 10)
	from, to := kit.Int(s.With, "from", 1), kit.Int(s.With, "to", 100)
	prefix := kit.Str(s.With, "id_prefix")
	if prefix == "" {
		prefix = "L"
	}
	if rate <= 0 || to < from {
		return kit.Result{}, &TestError{fmt.Errorf("load.start: rate > 0 and to >= from required")}
	}
	x := e.exp()
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	x.mu.Lock()
	x.loadStop, x.loadDone = cancel, make(chan struct{})
	x.mu.Unlock()
	interval := time.Second / time.Duration(rate)
	go func() {
		defer close(x.loadDone)
		start := time.Now()
		for i := from; i <= to; i++ {
			due := start.Add(time.Duration(i-from) * interval)
			if wait := time.Until(due); wait > 0 {
				select {
				case <-lctx.Done():
					return
				case <-time.After(wait):
				}
			} else if -wait > interval {
				x.late.Add(1)
			}
			id := fmt.Sprintf("%s%d", prefix, i)
			go func() {
				if err := e.trigger.Enqueue(lctx, kit.Job{ID: id, Fields: map[string]any{"order_id": id}}); err != nil {
					x.errors.Add(1)
				}
			}()
			x.sent.Add(1)
		}
	}()
	return kit.Result{Note: fmt.Sprintf("open-model load: %d jobs/s, ids %s%d..%s%d", rate, prefix, from, prefix, to)}, nil
}

func (e *execution) exp() *experiment {
	e.tlMu.Lock()
	defer e.tlMu.Unlock()
	if e.x == nil {
		e.x = &experiment{}
	}
	return e.x
}

func (e *execution) stopLoad(wait bool, timeout time.Duration) kit.Result {
	x := e.exp()
	x.mu.Lock()
	stop, done := x.loadStop, x.loadDone
	x.mu.Unlock()
	if done == nil {
		return kit.Result{Note: "no load running"}
	}
	if wait {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
	stop()
	<-done
	return kit.Result{Output: map[string]any{"sent": x.sent.Load(), "late": x.late.Load(), "errors": x.errors.Load()},
		Note: fmt.Sprintf("load stopped: %d sent, %d late (generator saturated), %d enqueue errors", x.sent.Load(), x.late.Load(), x.errors.Load())}
}

// hold keeps the injected fault for `for`, checking abort conditions all the
// time; when one holds, every fault is removed at once and the experiment
// is marked aborted (blast radius control).
func (e *execution) hold(ctx context.Context, s kit.Step) (kit.Result, error) {
	x := e.exp()
	x.faultAt = time.Now()
	d := kit.Dur(s.With, "for", 10*time.Second)
	var aborts []assert.Expectation
	if _, ok := s.With["abort_if"]; ok {
		list, err := inlineExpectations(kit.Step{Name: s.Name, With: map[string]any{"expect": s.With["abort_if"]}})
		if err != nil {
			return kit.Result{}, &TestError{err}
		}
		aborts = list
	}
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		for _, a := range aborts {
			o := assert.Evaluate(ctx, a, e.resolve)
			if o.Result == "pass" {
				x.aborted = true
				x.abortWhy = fmt.Sprintf("%s %s %v (actual %s)", a.Check, a.Op, assert.Show(a.Expected), assert.Show(o.Actual))
				if cl, ok := e.byName["chaos"].(clearer); ok {
					_ = cl.ClearAll(ctx)
				}
				x.clearedAt = time.Now()
				return kit.Result{Output: o}, fmt.Errorf("experiment aborted after %s: %s — every fault was removed", time.Since(x.faultAt).Round(time.Millisecond), x.abortWhy)
			}
		}
		select {
		case <-ctx.Done():
			return kit.Result{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return kit.Result{Note: fmt.Sprintf("fault held for %s, no abort condition met", d)}, nil
}

// recover polls the steady-state expectations from now until they hold and
// records the recovery time (measured from the end of the fault).
func (e *execution) recoverStep(ctx context.Context, s kit.Step, within time.Duration) (kit.Result, error) {
	x := e.exp()
	if x.clearedAt.IsZero() {
		x.clearedAt = time.Now()
	}
	exps, err := inlineExpectations(s)
	if err != nil {
		return kit.Result{}, &TestError{err}
	}
	outs := assert.Eventually(ctx, exps, e.resolve, assert.Poll{Within: kit.Dur(s.With, "within", within), Interval: 200 * time.Millisecond, MaxInt: 500 * time.Millisecond}, nil)
	for _, o := range outs {
		if o.Result != "pass" {
			return kit.Result{Output: outs}, fmt.Errorf("steady state not regained within %s: %s %s %v (actual %s)",
				kit.Dur(s.With, "within", within), o.Check, o.Operator, assert.Show(o.Expected), assert.Show(o.Actual))
		}
	}
	last := time.Time{}
	for _, o := range outs {
		if o.FirstPass.After(last) {
			last = o.FirstPass
		}
	}
	x.recovered = true
	x.recovery = last.Sub(x.clearedAt)
	if x.recovery < 0 {
		x.recovery = 0
	}
	return kit.Result{Output: outs, Note: fmt.Sprintf("steady state regained %s after the fault was removed", x.recovery.Round(time.Millisecond))}, nil
}

// experimentChecker answers experiment.* checks.
type experimentChecker struct{ e *execution }

func (c experimentChecker) CheckPrefixes() []string { return []string{"experiment"} }

// Check resolves:
//
//	experiment.recovery_seconds   time from fault removal to steady state (-1 if not recovered)
//	experiment.aborted            true when an abort condition stopped the fault
//	experiment.load.sent | late | errors
func (c experimentChecker) Check(_ context.Context, ref kit.CheckRef) (kit.Observation, error) {
	x := c.e.exp()
	now := time.Now().UTC()
	switch ref.Segments[1].Name {
	case "recovery_seconds":
		v := -1.0
		if x.recovered {
			v = x.recovery.Seconds()
		}
		return kit.Observation{Value: v, Source: "time from fault removal to steady state (chaos.recover)", At: now}, nil
	case "aborted":
		return kit.Observation{Value: x.aborted, Source: "abort conditions of chaos.hold: " + x.abortWhy, At: now}, nil
	case "load":
		if len(ref.Segments) == 3 {
			switch ref.Segments[2].Name {
			case "sent":
				return kit.Observation{Value: x.sent.Load(), Source: "open-model generator", At: now}, nil
			case "late":
				return kit.Observation{Value: x.late.Load(), Source: "arrivals issued late (generator saturated)", At: now}, nil
			case "errors":
				return kit.Observation{Value: x.errors.Load(), Source: "enqueue errors", At: now}, nil
			}
		}
	}
	return kit.Observation{At: now}, fmt.Errorf("unknown experiment check %q", ref.Raw)
}
