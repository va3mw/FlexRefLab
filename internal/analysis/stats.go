// Package analysis turns runs into the numbers engineering asks for:
// offset, scatter, drift, stability (Allan family), settling, temperature
// coefficient, periodic components, and comparisons between references.
package analysis

import (
	"math"
	"sort"
)

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func clean(v []float64) []float64 {
	out := make([]float64, 0, len(v))
	for _, x := range v {
		if finite(x) {
			out = append(out, x)
		}
	}
	return out
}

// Mean of the finite values.
func Mean(v []float64) float64 {
	s, n := 0.0, 0
	for _, x := range v {
		if finite(x) {
			s += x
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return s / float64(n)
}

// Std is the sample standard deviation of the finite values.
func Std(v []float64) float64 {
	m := Mean(v)
	s, n := 0.0, 0
	for _, x := range v {
		if finite(x) {
			s += (x - m) * (x - m)
			n++
		}
	}
	if n < 2 {
		return math.NaN()
	}
	return math.Sqrt(s / float64(n-1))
}

// Quantile of the finite values (q in 0..1, linear interpolation).
func Quantile(v []float64, q float64) float64 {
	c := clean(v)
	if len(c) == 0 {
		return math.NaN()
	}
	sort.Float64s(c)
	pos := q * float64(len(c)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return c[lo] + (c[hi]-c[lo])*(pos-float64(lo))
}

// MinMax of the finite values.
func MinMax(v []float64) (float64, float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, x := range v {
		if finite(x) {
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
	}
	if lo > hi {
		return math.NaN(), math.NaN()
	}
	return lo, hi
}

// Fit is a least-squares straight line y = A + B x.
type Fit struct {
	A, B  float64
	BErr  float64 // standard error of B
	R2    float64
	R     float64 // Pearson correlation
	N     int
	Resid float64 // RMS residual
}

// LinFit fits pairs where both values are finite.
func LinFit(x, y []float64) Fit {
	var sx, sy, sxx, sxy, syy float64
	n := 0
	for i := range x {
		if i >= len(y) || !finite(x[i]) || !finite(y[i]) {
			continue
		}
		sx += x[i]
		sy += y[i]
		n++
	}
	f := Fit{A: math.NaN(), B: math.NaN(), BErr: math.NaN(), R2: math.NaN(), R: math.NaN(), N: n, Resid: math.NaN()}
	if n < 3 {
		return f
	}
	mx, my := sx/float64(n), sy/float64(n)
	for i := range x {
		if i >= len(y) || !finite(x[i]) || !finite(y[i]) {
			continue
		}
		dx, dy := x[i]-mx, y[i]-my
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 {
		return f
	}
	f.B = sxy / sxx
	f.A = my - f.B*mx
	ssr := syy - f.B*sxy
	if ssr < 0 {
		ssr = 0
	}
	f.Resid = math.Sqrt(ssr / float64(n))
	f.BErr = math.Sqrt(ssr / float64(n-2) / sxx)
	if syy > 0 {
		f.R2 = 1 - ssr/syy
		f.R = sxy / math.Sqrt(sxx*syy)
	}
	return f
}

// Bin is one histogram bar.
type Bin struct {
	Lo, Hi float64
	N      int
}

// Histogram of the finite values in about n bins.
func Histogram(v []float64, n int) []Bin {
	lo, hi := MinMax(v)
	if !finite(lo) {
		return nil
	}
	if hi == lo {
		hi = lo + 1
	}
	w := (hi - lo) / float64(n)
	bins := make([]Bin, n)
	for i := range bins {
		bins[i].Lo = lo + float64(i)*w
		bins[i].Hi = bins[i].Lo + w
	}
	for _, x := range v {
		if !finite(x) {
			continue
		}
		k := int((x - lo) / w)
		if k >= n {
			k = n - 1
		}
		bins[k].N++
	}
	return bins
}
