package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PerfSpec turns a case into a performance test. Load is generated with an
// open model (fixed arrival rate, independent of response times) so slow
// responses cannot hide latency (no coordinated omission).
type PerfSpec struct {
	Kind       string             `yaml:"kind"`       // smoke | load | stress | spike | soak
	Executor   string             `yaml:"executor"`   // trigger (jobs at a fixed rate) | k6 (HTTP script)
	Script     string             `yaml:"script"`     // k6: script relative to the case file
	Target     string             `yaml:"target"`     // k6: base URL (template, e.g. {{ .sut.http }})
	Warmup     string             `yaml:"warmup"`     // excluded from the measurement
	Stages     []PerfStage        `yaml:"stages"`     // fixed-rate segments, in order
	Thresholds map[string]float64 `yaml:"thresholds"` // SLO (hard): p50_ms p95_ms p99_ms error_rate min_throughput max_dropped
	Baseline   *BaselineSpec      `yaml:"baseline"`
	Repeat     int                `yaml:"repeat"` // measured repetitions (statistics need several)
	Env        map[string]string  `yaml:"env"`    // k6 env (templated)
	IDPrefix   string             `yaml:"id_prefix"`
}

// PerfStage is a constant arrival rate for a duration.
type PerfStage struct {
	Rate     float64 `yaml:"rate"`     // arrivals per second
	Duration string  `yaml:"duration"` // e.g. 30s
}

// BaselineSpec compares the measurement with a stored baseline of the same environment.
type BaselineSpec struct {
	Key           string   `yaml:"key"`
	MaxRegression string   `yaml:"max_regression"` // e.g. 10%
	Metrics       []string `yaml:"metrics"`        // default: p95_ms, p99_ms, throughput
}

// WarmupDuration parses the warm-up (0 when absent).
func (p *PerfSpec) WarmupDuration() time.Duration {
	d, _ := time.ParseDuration(p.Warmup)
	return d
}

// TotalJobs is the number of arrivals of the measured stages + warm-up.
func (p *PerfSpec) TotalJobs() int {
	n := 0.0
	if w := p.WarmupDuration(); w > 0 && len(p.Stages) > 0 {
		n += p.Stages[0].Rate * w.Seconds()
	}
	for _, s := range p.Stages {
		d, _ := time.ParseDuration(s.Duration)
		n += s.Rate * d.Seconds()
	}
	return int(n + 0.5)
}

// MaxRegressionRatio parses "10%" into 0.10 (default 10%).
func (b *BaselineSpec) MaxRegressionRatio() float64 {
	if b == nil || b.MaxRegression == "" {
		return 0.10
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(b.MaxRegression, "%"), 64)
	if err != nil {
		return 0.10
	}
	return v / 100
}

// Validate checks a perf spec (used by lint).
func (p *PerfSpec) Validate() []string {
	var errs []string
	switch p.Kind {
	case "smoke", "load", "stress", "spike", "soak":
	default:
		errs = append(errs, fmt.Sprintf("perf.kind %q must be smoke|load|stress|spike|soak", p.Kind))
	}
	switch p.Executor {
	case "trigger":
	case "k6":
		if p.Script == "" || p.Target == "" {
			errs = append(errs, "perf.executor k6 needs script and target")
		}
	default:
		errs = append(errs, fmt.Sprintf("perf.executor %q must be trigger|k6", p.Executor))
	}
	if len(p.Stages) == 0 {
		errs = append(errs, "perf.stages: at least one {rate, duration}")
	}
	for i, s := range p.Stages {
		if s.Rate <= 0 {
			errs = append(errs, fmt.Sprintf("perf.stages[%d].rate must be > 0", i))
		}
		if _, err := time.ParseDuration(s.Duration); err != nil {
			errs = append(errs, fmt.Sprintf("perf.stages[%d].duration: %v", i, err))
		}
	}
	if p.Warmup != "" {
		if _, err := time.ParseDuration(p.Warmup); err != nil {
			errs = append(errs, "perf.warmup: "+err.Error())
		}
	}
	known := map[string]bool{"p50_ms": true, "p95_ms": true, "p99_ms": true, "error_rate": true, "min_throughput": true, "max_dropped": true}
	for k := range p.Thresholds {
		if !known[k] {
			errs = append(errs, fmt.Sprintf("perf.thresholds: unknown key %q (p50_ms p95_ms p99_ms error_rate min_throughput max_dropped)", k))
		}
	}
	if p.Baseline != nil && p.Baseline.Key == "" {
		errs = append(errs, "perf.baseline.key is required")
	}
	return errs
}
