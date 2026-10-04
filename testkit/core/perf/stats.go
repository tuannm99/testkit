// Package perf holds the statistics and the baseline store of performance
// tests. A regression is decided from several runs on each side (a
// distribution against a distribution), never one number against another.
package perf

import (
	"math"
	"sort"
)

// Median of xs (NaN for no data).
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// MAD is the median absolute deviation (robust spread).
func MAD(xs []float64) float64 {
	m := Median(xs)
	d := make([]float64, len(xs))
	for i, x := range xs {
		d[i] = math.Abs(x - m)
	}
	return Median(d)
}

// Percentile with linear interpolation (p in [0, 100]).
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	rank := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	return s[lo] + (s[hi]-s[lo])*(rank-float64(lo))
}

// MannWhitneyGreater tests H1: values of b tend to be greater than values of
// a (one-sided). It returns the U statistic of b and the p-value; exact for
// small samples (n1*n2 <= 400), normal approximation otherwise.
func MannWhitneyGreater(a, b []float64) (u float64, p float64) {
	n1, n2 := len(a), len(b)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}
	// U_b = number of pairs (a_i, b_j) with b_j > a_i, ties count 1/2.
	for _, x := range a {
		for _, y := range b {
			switch {
			case y > x:
				u++
			case y == x:
				u += 0.5
			}
		}
	}
	if n1*n2 <= 400 {
		return u, exactUpper(n1, n2, u)
	}
	mean := float64(n1*n2) / 2
	sd := math.Sqrt(float64(n1*n2*(n1+n2+1)) / 12)
	z := (u - 0.5 - mean) / sd
	return u, 0.5 * math.Erfc(z/math.Sqrt2)
}

// exactUpper returns P(U >= u) under H0 (no ties) by counting the
// permutations with a DP over (i, j, U).
func exactUpper(n1, n2 int, u float64) float64 {
	maxU := n1 * n2
	// f[i][j][k]: number of arrangements of i a's and j b's with U = k.
	f := make([][][]float64, n1+1)
	for i := range f {
		f[i] = make([][]float64, n2+1)
		for j := range f[i] {
			f[i][j] = make([]float64, maxU+1)
		}
	}
	for i := 0; i <= n1; i++ {
		for j := 0; j <= n2; j++ {
			if i == 0 || j == 0 {
				f[i][j][0] = 1
				continue
			}
			for k := 0; k <= i*j; k++ {
				// last element is a b (counts the i a's below it) or an a.
				if k-i >= 0 {
					f[i][j][k] += f[i][j-1][k-i]
				}
				f[i][j][k] += f[i-1][j][k]
			}
		}
	}
	total, tail := 0.0, 0.0
	thr := int(math.Ceil(u - 1e-9))
	for k := 0; k <= maxU; k++ {
		total += f[n1][n2][k]
		if k >= thr {
			tail += f[n1][n2][k]
		}
	}
	return tail / total
}
