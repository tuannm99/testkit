package perf

import (
	"math"
	"testing"
)

func TestMedianMADPercentile(t *testing.T) {
	xs := []float64{5, 1, 3, 2, 4}
	if Median(xs) != 3 || MAD(xs) != 1 || Percentile(xs, 50) != 3 || Percentile(xs, 100) != 5 {
		t.Fatal(Median(xs), MAD(xs))
	}
	if Median([]float64{1, 2, 3, 4}) != 2.5 {
		t.Fatal("even median")
	}
}

func TestMannWhitneyExact(t *testing.T) {
	// Completely separated samples of 5 vs 5: P(U >= 25) = 1/C(10,5) = 1/252.
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{6, 7, 8, 9, 10}
	u, p := MannWhitneyGreater(a, b)
	if u != 25 || math.Abs(p-1.0/252) > 1e-12 {
		t.Fatalf("u=%v p=%v", u, p)
	}
	// Identical distributions are not significant.
	if _, p := MannWhitneyGreater([]float64{1, 3, 5}, []float64{2, 4, 6}); p < 0.2 {
		t.Fatalf("p=%v", p)
	}
}

func TestCompareDetectsRegressionOnlyWhenSignificant(t *testing.T) {
	base := []float64{100, 102, 98, 101, 99}
	slow := []float64{130, 128, 131}
	c := Compare("p95_ms", base, slow, 0.10)
	if !c.Regression || !c.Significant {
		t.Fatalf("%+v", c)
	}
	noisy := []float64{100, 140, 95}
	if c := Compare("p95_ms", base, noisy, 0.10); c.Regression {
		t.Fatalf("noisy sample must not be a regression: %+v", c)
	}
	// Throughput: lower is worse.
	if c := Compare("throughput", []float64{50, 51, 49, 50, 50}, []float64{40, 41, 39}, 0.10); !c.Regression {
		t.Fatalf("%+v", c)
	}
	if c := Compare("throughput", []float64{50, 51, 49}, []float64{60, 61, 59}, 0.10); c.Regression {
		t.Fatalf("faster is not a regression: %+v", c)
	}
}

func TestFingerprintStable(t *testing.T) {
	a := Fingerprint{CPUs: 4, MemoryGiB: 16, OS: "linux/amd64", DockerVersion: "29", Images: map[string]string{"A": "x", "B": "y"}}
	b := a
	b.Images = map[string]string{"B": "y", "A": "x"}
	if a.ID() != b.ID() {
		t.Fatal("fingerprint depends on map order")
	}
	b.CPUs = 8
	if a.ID() == b.ID() {
		t.Fatal("different machines share a fingerprint")
	}
}
