package orchestrator

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tuannm99/testkit/testkit/core/assert"
	"github.com/tuannm99/testkit/testkit/core/kit"
	"github.com/tuannm99/testkit/testkit/core/perf"
	"github.com/tuannm99/testkit/testkit/core/result"
	"github.com/tuannm99/testkit/testkit/core/scenario"
)

// arrival is one scheduled job of the open-model generator.
type arrival struct {
	id       string
	at       time.Duration // offset from the start
	measured bool          // false during warm-up
}

func schedule(spec *scenario.PerfSpec, prefix string) ([]arrival, time.Duration) {
	var out []arrival
	offset := time.Duration(0)
	n := 0
	add := func(rate float64, d time.Duration, measured bool) {
		count := int(rate*d.Seconds() + 0.5)
		step := time.Duration(float64(time.Second) / rate)
		for i := 0; i < count; i++ {
			n++
			out = append(out, arrival{id: fmt.Sprintf("%s%d", prefix, n), at: offset + time.Duration(i)*step, measured: measured})
		}
		offset += d
	}
	if w := spec.WarmupDuration(); w > 0 {
		add(spec.Stages[0].Rate, w, false)
	}
	measured := time.Duration(0)
	for _, s := range spec.Stages {
		d, _ := time.ParseDuration(s.Duration)
		add(s.Rate, d, true)
		measured += d
	}
	return out, measured
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// perfTrigger runs one repetition with the trigger executor: jobs arrive at
// fixed rates (open model); end-to-end latency of each job = completion time
// recorded by the service in its database − time the generator issued it.
func (e *execution) perfTrigger(ctx context.Context, spec *scenario.PerfSpec, rep int) (map[string]float64, error) {
	if e.trigger == nil {
		return nil, &TestError{fmt.Errorf("perf executor trigger needs a trigger")}
	}
	svc := e.r.Services[e.ex.Service]
	pg, ok := e.byName["postgres"]
	if !ok || svc.Perf.Fixture == "" || svc.Perf.Completion == "" {
		return nil, &TestError{fmt.Errorf("perf executor trigger needs stores.postgres and perf.fixture/perf.completion in the service descriptor")}
	}
	prefix := spec.IDPrefix
	if prefix == "" {
		prefix = "PF"
	}
	prefix = fmt.Sprintf("%s%d-", prefix, rep)
	arrivals, window := schedule(spec, prefix)
	data := map[string]any{"prefix": prefix, "from": 1, "to": len(arrivals), "ns": string(e.env.NS)}
	fixture, err := scenario.RenderString(svc.Perf.Fixture, data)
	if err != nil {
		return nil, &TestError{err}
	}
	if _, err := pg.Apply(ctx, kit.Step{Name: "postgres.exec", With: map[string]any{"sql": fmt.Sprint(fixture)}}); err != nil {
		return nil, fmt.Errorf("perf fixture: %w", err)
	}

	sent := make([]time.Time, len(arrivals))
	var late, enqueueErrors atomic.Int64
	sem := make(chan struct{}, 256)
	var wg sync.WaitGroup
	cpu0, t0 := cpuTime(), time.Now()
	for i, a := range arrivals {
		due := t0.Add(a.at)
		if d := time.Until(due); d > 0 {
			time.Sleep(d) // generator pacing (fixed arrival times), not a wait in a test
		} else if -d > 20*time.Millisecond {
			late.Add(1) // the generator could not keep the schedule
		}
		select {
		case sem <- struct{}{}:
		default:
			late.Add(1) // too many enqueues in flight: the generator is saturated
			sem <- struct{}{}
		}
		sent[i] = time.Now()
		wg.Add(1)
		go func(a arrival) {
			defer func() { <-sem; wg.Done() }()
			if err := e.trigger.Enqueue(ctx, kit.Job{ID: a.id, Fields: map[string]any{"order_id": a.id}}); err != nil {
				enqueueErrors.Add(1)
			}
		}(a)
	}
	wg.Wait()
	genWall, genCPU := time.Since(t0), cpuTime()-cpu0
	if n := enqueueErrors.Load(); n > 0 {
		// The generator failed, not the service: the measurement is invalid.
		return nil, fmt.Errorf("load generator: %d of %d job(s) could not be enqueued", n, len(arrivals))
	}

	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	drainErr := e.trigger.Drain(dctx)
	cancel()

	compl, err := scenario.RenderString(svc.Perf.Completion, data)
	if err != nil {
		return nil, &TestError{err}
	}
	res, err := pg.Apply(ctx, kit.Step{Name: "postgres.query", With: map[string]any{"sql": fmt.Sprint(compl)}})
	if err != nil {
		return nil, fmt.Errorf("perf completion query: %w", err)
	}
	done := map[string]float64{}
	rows, _ := res.Output.([]map[string]any)
	for _, r := range rows {
		id := fmt.Sprint(r["id"])
		f, _ := strconv.ParseFloat(fmt.Sprint(r["done"]), 64)
		done[id] = f
	}

	var lat []float64
	measured, completed := 0, 0
	firstSent, lastDone := math.Inf(1), 0.0
	var csvBuf bytes.Buffer
	cw := csv.NewWriter(&csvBuf)
	_ = cw.Write([]string{"id", "phase", "sent_unix", "done_unix", "latency_ms"})
	for i, a := range arrivals {
		s := float64(sent[i].UnixNano()) / 1e9
		d, ok := done[a.id]
		phase := "warmup"
		if a.measured {
			phase = "measure"
			measured++
			firstSent = math.Min(firstSent, s)
			if ok {
				completed++
				lat = append(lat, (d-s)*1000)
				lastDone = math.Max(lastDone, d)
			}
		}
		latStr := ""
		if ok {
			latStr = strconv.FormatFloat((d-s)*1000, 'f', 2, 64)
		}
		_ = cw.Write([]string{a.id, phase, strconv.FormatFloat(s, 'f', 6, 64), strconv.FormatFloat(d, 'f', 6, 64), latStr})
	}
	cw.Flush()
	_, _ = e.dir.WriteFile(path.Join(e.ex.Dir, "perf", fmt.Sprintf("rep-%d", rep), "jobs.csv"), csvBuf.Bytes())

	m := map[string]float64{
		"p50_ms":            perf.Percentile(lat, 50),
		"p95_ms":            perf.Percentile(lat, 95),
		"p99_ms":            perf.Percentile(lat, 99),
		"max_ms":            perf.Percentile(lat, 100),
		"offered_rate":      float64(measured) / window.Seconds(),
		"late":              float64(late.Load()),
		"generator_cpu_pct": float64(genCPU) / float64(genWall) * 100,
		"jobs":              float64(measured),
	}
	if measured > 0 {
		m["error_rate"] = float64(measured-completed) / float64(measured)
	}
	if completed > 0 && lastDone > firstSent {
		m["throughput"] = float64(completed) / (lastDone - firstSent)
	}
	if drainErr != nil {
		e.event(result.Event{Kind: "perf", Name: fmt.Sprintf("repetition %d drain", rep), Status: "error", Detail: drainErr.Error()})
	}
	return m, nil
}

// runPerf executes the measured repetitions and turns SLO thresholds and
// the baseline comparison into assertion outcomes.
func (e *execution) runPerf(ctx context.Context, c *scenario.Case) []assert.Outcome {
	spec := c.Perf
	reps := spec.Repeat
	if reps <= 0 {
		reps = 1
		if spec.Baseline != nil {
			reps = 3
		}
	}
	if e.r.BaselineRuns > 0 {
		reps = e.r.BaselineRuns
	}
	pr := &result.PerfResult{ID: e.ex.ID, Title: e.ex.Title, Kind: spec.Kind, Executor: spec.Executor, Repeat: reps,
		Metrics: map[string]float64{}, Samples: map[string][]float64{}, Thresholds: spec.Thresholds,
		Dir: path.Join(e.ex.Dir, "perf"), Requirement: e.ex.Requirement}
	e.ex.Perf = pr
	var outs []assert.Outcome
	for rep := 1; rep <= reps; rep++ {
		t0 := time.Now().UTC()
		var m map[string]float64
		var err error
		switch spec.Executor {
		case "k6":
			m, err = e.perfK6(ctx, spec, rep)
		default:
			m, err = e.perfTrigger(ctx, spec, rep)
		}
		e.event(result.Event{At: t0, End: time.Now().UTC(), Kind: "perf", Name: fmt.Sprintf("repetition %d/%d (%s, %s)", rep, reps, spec.Kind, spec.Executor),
			Status: status(err), Detail: firstNonEmpty(errString(err), fmtMetrics(m))})
		e.annotate(t0, time.Now().UTC(), "perf", fmt.Sprintf("perf repetition %d/%d", rep, reps))
		if err != nil {
			return []assert.Outcome{{ID: "PERF", Result: "error", Check: "perf.run", Operator: "exists", Why: "the load test must complete", Message: err.Error(), ObservedAt: time.Now().UTC()}}
		}
		for k, v := range m {
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				pr.Samples[k] = append(pr.Samples[k], v)
			}
		}
	}
	for k, xs := range pr.Samples {
		pr.Metrics[k] = perf.Median(xs)
	}
	now := time.Now().UTC()
	src := fmt.Sprintf("median of %d repetition(s), samples in %s/summary.json", reps, pr.Dir)
	thr := []struct{ key, metric, op, why string }{
		{"p50_ms", "p50_ms", "lte", "SLO: độ trễ trung vị"},
		{"p95_ms", "p95_ms", "lte", "SLO: độ trễ p95"},
		{"p99_ms", "p99_ms", "lte", "SLO: độ trễ p99"},
		{"error_rate", "error_rate", "lte", "SLO: tỉ lệ lỗi"},
		{"min_throughput", "throughput", "gte", "SLO: thông lượng tối thiểu"},
		{"max_dropped", "late", "lte", "Máy tạo tải không bị nghẽn (đo đúng tải đã định)"},
	}
	for _, t := range thr {
		limit, ok := spec.Thresholds[t.key]
		if !ok {
			continue
		}
		actual, has := pr.Metrics[t.metric]
		o := assert.Outcome{ID: "SLO-" + t.key, Check: "perf." + t.metric, Operator: t.op, Expected: limit, Actual: actual,
			Why: t.why, ObservedAt: now, Source: src, Attempts: reps}
		pass, _ := assert.Compare(t.op, actual, limit, 0)
		switch {
		case !has:
			o.Result, o.Message = "error", "metric not measured"
		case pass:
			o.Result = "pass"
		default:
			o.Result = "fail"
			o.Message = fmt.Sprintf("%s = %.4g, SLO %s %.4g", t.metric, actual, t.op, limit)
		}
		outs = append(outs, o)
	}
	outs = append(outs, e.baseline(c, pr, now)...)
	_, _ = e.dir.WriteJSON(path.Join(pr.Dir, "summary.json"), pr)
	e.ex.Artifacts = append(e.ex.Artifacts, kit.Artifact{Kind: "perf", Path: path.Join(pr.Dir, "summary.json"),
		Title: fmt.Sprintf("Performance %s: %d repetition(s), medians and samples", spec.Kind, reps), Source: "perf"})
	return outs
}

func fmtMetrics(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%.4g ", k, m[k])
	}
	return b.String()
}

// baseline records (testkit baseline record) or compares with the stored
// baseline of the same environment.
func (e *execution) baseline(c *scenario.Case, pr *result.PerfResult, now time.Time) []assert.Outcome {
	b := c.Perf.Baseline
	if b == nil || e.r.Baselines == nil {
		return nil
	}
	pr.BaselineKey, pr.BaselineEnv = b.Key, e.r.Fingerprint.ID()
	metrics := b.Metrics
	if len(metrics) == 0 {
		metrics = []string{"p95_ms", "p99_ms", "throughput"}
	}
	if e.r.RecordBaseline {
		samples := map[string][]float64{}
		for _, m := range metrics {
			samples[m] = pr.Samples[m]
		}
		bl := perf.New(b.Key, c.ID, e.env.RunID, e.r.Commit, e.r.Fingerprint, samples)
		p, err := e.r.Baselines.Save(bl)
		pr.Note = fmt.Sprintf("baseline %s recorded (%d samples per metric) in %s", b.Key, len(pr.Samples[metrics[0]]), p)
		if err != nil {
			pr.Note = "baseline not saved: " + err.Error()
		}
		return nil
	}
	bl, p, err := e.r.Baselines.Load(e.r.Fingerprint, b.Key)
	if err != nil || bl == nil {
		why := fmt.Sprintf("no baseline %q for environment %s (record one with `testkit baseline record`)", b.Key, e.r.Fingerprint.ID())
		if err != nil {
			why = err.Error()
		}
		pr.Note = why
		return []assert.Outcome{{ID: "BASE", Result: "skipped", Check: "baseline." + b.Key, Operator: "exists", Why: "So sánh với baseline cùng môi trường", Message: why, ObservedAt: now, Source: p}}
	}
	pr.BaselineAt = bl.RecordedAt.Format(time.RFC3339)
	var outs []assert.Outcome
	for _, m := range metrics {
		cmp := perf.Compare(m, bl.Samples[m], pr.Samples[m], b.MaxRegressionRatio())
		pr.Comparisons = append(pr.Comparisons, result.PerfComparison{Metric: m, BaseMedian: cmp.BaseMedian, CurMedian: cmp.CurMedian,
			ChangePct: cmp.ChangePct, PValue: cmp.PValue, Allowed: cmp.Allowed, Regression: cmp.Regression, Stale: cmp.Stale, Verdict: cmp.Verdict,
			Baseline: cmp.Baseline, Current: cmp.Current})
		o := assert.Outcome{ID: "BASE-" + m, Check: "baseline." + b.Key + "." + m, Operator: "lte",
			Expected: fmt.Sprintf("tệ đi không quá %.0f%% so với baseline (median %.6g)", cmp.Allowed, cmp.BaseMedian), Actual: fmt.Sprintf("%.6g (tệ đi %+.1f%%, p=%.3f)", cmp.CurMedian, cmp.ChangePct, cmp.PValue),
			Why: "Không chậm hơn baseline cùng môi trường quá ngưỡng (kiểm định Mann-Whitney U, nhiều lần chạy)", ObservedAt: now, Source: p, Attempts: len(cmp.Current), Result: "pass"}
		if cmp.Regression {
			o.Result, o.Message = "fail", cmp.Verdict
		} else {
			o.Message = cmp.Verdict
		}
		outs = append(outs, o)
	}
	return outs
}

// perfK6 runs one repetition through the k6 connector.
func (e *execution) perfK6(ctx context.Context, spec *scenario.PerfSpec, rep int) (map[string]float64, error) {
	k6, ok := e.byName["k6"]
	if !ok {
		return nil, &TestError{fmt.Errorf("perf executor k6: connector k6 not provisioned")}
	}
	target, err := scenario.RenderString(spec.Target, e.data)
	if err != nil {
		return nil, &TestError{err}
	}
	env := map[string]string{}
	for k, v := range spec.Env {
		r, err := scenario.RenderString(v, e.data)
		if err != nil {
			return nil, &TestError{err}
		}
		env[k] = fmt.Sprint(r)
	}
	res, err := k6.Apply(ctx, kit.Step{Name: "k6.run", With: map[string]any{"spec": spec, "rep": rep, "case_file": e.caseFile,
		"target": fmt.Sprint(target), "env": env}})
	if err != nil {
		return nil, err
	}
	m, _ := res.Output.(map[string]float64)
	e.ex.Artifacts = append(e.ex.Artifacts, res.Artifacts...)
	return m, nil
}
