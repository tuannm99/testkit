package perf

import "testing"

func TestCompareFlagsStaleBaseline(t *testing.T) {
	base := []float64{320, 330, 300, 340, 310}
	if c := Compare("p95_ms", base, []float64{70, 72, 68}, 0.10); !c.Stale || c.Regression {
		t.Fatalf("much better than baseline not flagged: %+v", c)
	}
	if c := Compare("p95_ms", base, []float64{300, 310, 320}, 0.10); c.Stale {
		t.Fatalf("flagged within noise: %+v", c)
	}
}
