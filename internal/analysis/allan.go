package analysis

import "math"

// TauPoint is the stability at one averaging time. Deviations are fractional
// (dimensionless); multiply by 1e9 for ppb.
type TauPoint struct {
	Tau  float64 `json:"tau"`  // seconds
	ADEV float64 `json:"adev"` // overlapping Allan deviation
	AErr float64 `json:"aerr"` // rough 1-sigma error of ADEV
	MDEV float64 `json:"mdev"` // modified Allan deviation
	HDEV float64 `json:"hdev"` // overlapping Hadamard deviation (blind to linear drift)
	TDEV float64 `json:"tdev"` // time deviation, seconds
	N    int     `json:"n"`    // ADEV terms used
}

// Deviations computes the Allan family from fractional frequency y (one
// value per tau0 seconds; NaN marks a gate with no reading). A term is only
// used if every reading it spans is present, so gaps cost data but never
// bias the result.
func Deviations(y []float64, tau0 float64) []TauPoint {
	n := len(y)
	if n < 4 {
		return nil
	}
	// Phase x (seconds) and a prefix count of missing readings.
	x := make([]float64, n+1)
	bad := make([]int, n+1)
	for i, v := range y {
		b := 0
		if !finite(v) {
			v, b = 0, 1
		}
		x[i+1] = x[i] + v*tau0
		bad[i+1] = bad[i] + b
	}
	ok := func(from, to int) bool { return bad[to]-bad[from] == 0 } // y[from:to] complete
	var out []TauPoint
	for _, m := range tauFactors(n) {
		tau := float64(m) * tau0
		p := TauPoint{Tau: tau, ADEV: math.NaN(), AErr: math.NaN(), MDEV: math.NaN(), HDEV: math.NaN(), TDEV: math.NaN()}
		// ADEV and the second differences d_i used by MDEV.
		nd := n + 1 - 2*m
		if nd < 2 {
			break
		}
		d := make([]float64, nd)
		dok := make([]bool, nd)
		var s float64
		cnt := 0
		for i := 0; i < nd; i++ {
			if !ok(i, i+2*m) {
				continue
			}
			d[i] = x[i+2*m] - 2*x[i+m] + x[i]
			dok[i] = true
			s += d[i] * d[i]
			cnt++
		}
		if cnt > 0 {
			p.ADEV = math.Sqrt(s / (2 * tau * tau * float64(cnt)))
			p.N = cnt
			p.AErr = p.ADEV / math.Sqrt(math.Max(1, float64(cnt)/float64(m)))
		}
		// MDEV: sums of m consecutive d_i, via prefix sums.
		if nj := nd - m + 1; nj >= 1 {
			pre := make([]float64, nd+1)
			preBad := make([]int, nd+1)
			for i := 0; i < nd; i++ {
				pre[i+1] = pre[i] + d[i]
				b := 0
				if !dok[i] {
					b = 1
				}
				preBad[i+1] = preBad[i] + b
			}
			var ms float64
			mc := 0
			for j := 0; j < nj; j++ {
				if preBad[j+m]-preBad[j] != 0 {
					continue
				}
				v := pre[j+m] - pre[j]
				ms += v * v
				mc++
			}
			if mc > 0 {
				p.MDEV = math.Sqrt(ms / (2 * float64(m*m) * tau * tau * float64(mc)))
				p.TDEV = tau / math.Sqrt(3) * p.MDEV
			}
		}
		// Overlapping Hadamard.
		if nh := n + 1 - 3*m; nh >= 2 {
			var hs float64
			hc := 0
			for i := 0; i < nh; i++ {
				if !ok(i, i+3*m) {
					continue
				}
				v := x[i+3*m] - 3*x[i+2*m] + 3*x[i+m] - x[i]
				hs += v * v
				hc++
			}
			if hc > 0 {
				p.HDEV = math.Sqrt(hs / (6 * tau * tau * float64(hc)))
			}
		}
		out = append(out, p)
	}
	return out
}

// tauFactors returns 1, 2, 5, 10, 20, 50 ... up to n/4.
func tauFactors(n int) []int {
	var out []int
	for dec := 1; dec <= n; dec *= 10 {
		for _, k := range []int{1, 2, 5} {
			if m := k * dec; m <= n/4 {
				out = append(out, m)
			}
		}
	}
	return out
}

// NoiseType names the dominant noise from the ADEV slope (log-log) between
// two taus, refined by MDEV to tell white from flicker phase noise.
func NoiseType(a, b TauPoint) string {
	if !finite(a.ADEV) || !finite(b.ADEV) || a.ADEV <= 0 || b.ADEV <= 0 {
		return ""
	}
	mu := math.Log(b.ADEV/a.ADEV) / math.Log(b.Tau/a.Tau)
	switch {
	case mu < -0.75:
		if finite(a.MDEV) && finite(b.MDEV) && a.MDEV > 0 && b.MDEV > 0 {
			if math.Log(b.MDEV/a.MDEV)/math.Log(b.Tau/a.Tau) < -1.25 {
				return "white PM (measurement noise)"
			}
		}
		return "flicker PM"
	case mu < -0.25:
		return "white FM"
	case mu < 0.25:
		return "flicker FM (floor)"
	case mu < 0.75:
		return "random-walk FM"
	default:
		return "drift"
	}
}
