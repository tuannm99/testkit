package perf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fingerprint identifies an environment: results are only comparable with
// a baseline measured on the same kind of machine and stack versions.
type Fingerprint struct {
	CPUs          int               `json:"cpus"`
	MemoryGiB     int               `json:"memory_gib"`
	OS            string            `json:"os"`
	DockerVersion string            `json:"docker_version"`
	Images        map[string]string `json:"images"` // pinned images of the stack
}

// ID is a short stable directory name.
func (f Fingerprint) ID() string {
	keys := make([]string, 0, len(f.Images))
	for k := range f.Images {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%d|%s|%s", f.CPUs, f.MemoryGiB, f.OS, f.DockerVersion)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, f.Images[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%dcpu-%dg-%s", f.CPUs, f.MemoryGiB, hex.EncodeToString(sum[:])[:10])
}

// Baseline is the stored distribution of each metric for one perf case.
type Baseline struct {
	Key         string               `json:"key"`
	CaseID      string               `json:"case_id"`
	Environment Fingerprint          `json:"environment"`
	RecordedAt  time.Time            `json:"recorded_at"`
	Commit      string               `json:"commit"`
	RunID       string               `json:"run_id"`
	Samples     map[string][]float64 `json:"samples"`
	Median      map[string]float64   `json:"median"`
	MAD         map[string]float64   `json:"mad"`
}

// New computes medians/MADs from samples.
func New(key, caseID, runID, commit string, env Fingerprint, samples map[string][]float64) *Baseline {
	b := &Baseline{Key: key, CaseID: caseID, RunID: runID, Commit: commit, Environment: env, RecordedAt: time.Now().UTC(),
		Samples: samples, Median: map[string]float64{}, MAD: map[string]float64{}}
	for m, xs := range samples {
		b.Median[m], b.MAD[m] = Median(xs), MAD(xs)
	}
	return b
}

// Store keeps baselines under <dir>/<fingerprint id>/<key>.json.
type Store struct{ Dir string }

func (s Store) path(env Fingerprint, key string) string {
	return filepath.Join(s.Dir, env.ID(), key+".json")
}

func (s Store) Save(b *Baseline) (string, error) {
	p := s.path(b.Environment, b.Key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	raw, _ := json.MarshalIndent(b, "", "  ")
	return p, os.WriteFile(p, append(raw, '\n'), 0o644)
}

// Load returns the baseline of key for this environment (nil, nil if none).
func (s Store) Load(env Fingerprint, key string) (*Baseline, string, error) {
	p := s.path(env, key)
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, p, nil
	}
	if err != nil {
		return nil, p, err
	}
	var b Baseline
	return &b, p, json.Unmarshal(raw, &b)
}

// HigherIsWorse tells the direction of a metric.
func HigherIsWorse(metric string) bool {
	switch metric {
	case "throughput":
		return false
	}
	return true
}

// Comparison of one metric against the baseline.
type Comparison struct {
	Metric       string    `json:"metric"`
	Baseline     []float64 `json:"baseline_samples"`
	Current      []float64 `json:"current_samples"`
	BaseMedian   float64   `json:"baseline_median"`
	CurMedian    float64   `json:"current_median"`
	ChangePct    float64   `json:"change_pct"`   // positive = worse
	PValue       float64   `json:"p_value"`      // one-sided Mann-Whitney U (current worse than baseline)
	Allowed      float64   `json:"allowed_pct"`  // max regression
	Regression   bool      `json:"regression"`   // worse by more than allowed AND significant
	Significant  bool      `json:"significant"`  // p < Alpha
	Insufficient bool      `json:"insufficient"` // fewer than 3 samples on a side: median rule only
	// Stale: significantly better than the baseline by more than twice the
	// allowed ratio. The baseline no longer describes the system, and a real
	// regression back towards it would go unnoticed: record a new one.
	Stale   bool   `json:"stale_baseline"`
	Verdict string `json:"verdict"`
}

// Alpha is the significance level of the regression test.
const Alpha = 0.05

// Compare decides regression for one metric: the current median is worse
// than the baseline median by more than allowed (ratio, e.g. 0.10) and the
// difference is statistically significant (Mann-Whitney U, one-sided).
func Compare(metric string, base, cur []float64, allowed float64) Comparison {
	c := Comparison{Metric: metric, Baseline: base, Current: cur, BaseMedian: Median(base), CurMedian: Median(cur), Allowed: allowed * 100}
	worse := c.CurMedian - c.BaseMedian
	if !HigherIsWorse(metric) {
		worse = -worse
	}
	if c.BaseMedian != 0 && !math.IsNaN(c.BaseMedian) {
		c.ChangePct = worse / math.Abs(c.BaseMedian) * 100
	}
	if HigherIsWorse(metric) {
		_, c.PValue = MannWhitneyGreater(base, cur)
	} else {
		_, c.PValue = MannWhitneyGreater(cur, base)
	}
	c.Insufficient = len(base) < 3 || len(cur) < 3
	c.Significant = c.PValue < Alpha
	beyond := c.ChangePct > allowed*100
	switch {
	case beyond && (c.Significant || c.Insufficient):
		c.Regression = true
		c.Verdict = fmt.Sprintf("REGRESSION: %s worse by %.1f%% (allowed %.0f%%), p=%.3f", metric, c.ChangePct, allowed*100, c.PValue)
	case beyond:
		c.Verdict = fmt.Sprintf("worse by %.1f%% but not significant (p=%.3f ≥ %.2f): not a regression", c.ChangePct, c.PValue, Alpha)
	default:
		c.Verdict = fmt.Sprintf("within %.0f%% of the baseline (%+.1f%% in the worse direction, p=%.3f)", allowed*100, c.ChangePct, c.PValue)
	}
	var pBetter float64
	if HigherIsWorse(metric) {
		_, pBetter = MannWhitneyGreater(cur, base)
	} else {
		_, pBetter = MannWhitneyGreater(base, cur)
	}
	if !c.Insufficient && -c.ChangePct > 2*allowed*100 && pBetter < Alpha {
		c.Stale = true
		c.Verdict += fmt.Sprintf(" — better by %.1f%% (p=%.3f): the baseline looks stale, record a new one", -c.ChangePct, pBetter)
	}
	if c.Insufficient {
		c.Verdict += " — fewer than 3 samples on a side: decided on medians only"
	}
	return c
}
