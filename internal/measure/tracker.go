// Package measure turns DAX audio of a steady carrier into frequency
// readings precise to a few millihertz.
//
// The audio is mixed down by a numerically controlled oscillator (NCO) set
// near the tone, averaged over 10 ms blocks (100 Hz complex samples; the
// boxcar nulls the image at twice the tone, which is a multiple of 100 Hz when
// the tone is) and phase-tracked. Each gate (1 s by default) gives:
//
//   - FreqHz: NCO + the phase advance from the end of the previous gate to the
//     end of this one, divided by the gate time. Consecutive gates share their
//     end points, so there is no dead time: this is the average frequency a
//     counter would read, and the right input for Allan deviation.
//   - FitHz: NCO + the least-squares slope of the phase over the gate. It is
//     quieter (it rejects white phase noise) and is what the live display shows.
//
// Frequencies are in the radio's own Hz: the DAX sample clock comes from the
// same reference as the radio's LO, which the caller corrects for exactly.
package measure

import (
	"math"
	"math/cmplx"
	"time"
)

// Gate is one frequency reading.
type Gate struct {
	End     time.Time `json:"end"`      // end of the gate (from the sample count)
	FreqHz  float64   `json:"freq_hz"`  // average audio frequency over the gate
	FitHz   float64   `json:"fit_hz"`   // least-squares frequency over the gate
	SNRdB   float64   `json:"snr_db"`   // carrier to noise in the 100 Hz tracking bandwidth
	LevelDB float64   `json:"level_db"` // carrier level, dBFS
	Valid   bool      `json:"valid"`
	Why     string    `json:"why,omitempty"` // why a gate is not valid
	Lost    int       `json:"lost"`          // DAX packets lost in the gate
}

// Status describes the tracker between gates, for the UI.
type Status struct {
	Acquired bool    `json:"acquired"`
	NCO      float64 `json:"nco"`
	PeakHz   float64 `json:"peak_hz"`  // last acquisition peak
	PeakSNR  float64 `json:"peak_snr"` // dB above the median bin
	Rate     int     `json:"rate"`
}

const (
	decimRate  = 100  // complex samples per second after the boxcar
	acqSeconds = 2    // audio collected for each acquisition FFT
	minAcqSNR  = 15.0 // dB above the median FFT bin to accept a peak
	minGateSNR = 10.0 // dB in 100 Hz: below this, cycle slips are likely
	maxStepRad = 1.5  // a phase step this far from the trend is a slip
)

// Tracker measures one tone. It is not safe for concurrent use.
type Tracker struct {
	Nominal float64 // expected tone frequency, Hz
	Search  float64 // acquisition search half-width, Hz
	GateSec int     // gate length, whole seconds

	rate     int
	m        int // audio samples per decimated sample
	t0       time.Time
	n        int64 // audio samples consumed since t0 (including lost ones)
	nco      float64
	ncoPh    float64
	acc      complex128
	accN     int
	acquired bool
	acqBuf   []float64
	status   Status

	// phase tracking
	havePrev bool
	prevZ    complex128
	phase    float64 // unwrapped phase of the latest decimated sample, rad
	gatePh0  float64 // phase at the end of the previous gate
	gateOK0  bool    // gatePh0 is continuous with this gate
	pts      []complex128
	phs      []float64
	lost     int
	slip     bool
}

// NewTracker makes a tracker for a tone near nominal Hz.
func NewTracker(nominal, search float64, gateSec int) *Tracker {
	if gateSec < 1 {
		gateSec = 1
	}
	if search <= 0 {
		search = 300
	}
	return &Tracker{Nominal: nominal, Search: search, GateSec: gateSec}
}

// Status returns the acquisition state.
func (t *Tracker) Status() Status { return t.status }

// Reacquire drops the lock and searches for the tone again (after a reference
// change, for instance, the tone can move by tens of hertz).
func (t *Tracker) Reacquire() {
	t.acquired = false
	t.acqBuf = t.acqBuf[:0]
	t.status.Acquired = false
	t.breakPhase()
}

func (t *Tracker) breakPhase() {
	t.havePrev = false
	t.gateOK0 = false
	t.pts, t.phs = t.pts[:0], t.phs[:0]
	t.acc, t.accN = 0, 0
}

// Push feeds audio. now is the wall time the packet arrived, used only to
// anchor the first sample. lost is the number of packets missed before this
// one; they are counted as silence of the same length so that gate times
// stay true, and the gate they fall in is marked not valid.
func (t *Tracker) Push(samples []float32, rate, lost int, now time.Time) []Gate {
	if rate <= 0 || len(samples) == 0 {
		return nil
	}
	if rate != t.rate {
		t.rate, t.m = rate, rate/decimRate
		t.t0, t.n = now.Add(-time.Duration(float64(len(samples))/float64(rate)*float64(time.Second))), 0
		t.Reacquire()
		t.status.Rate = rate
	}
	if lost > 0 {
		skip := int64(lost * len(samples))
		t.n += skip
		t.ncoPh = math.Mod(t.ncoPh+2*math.Pi*t.nco*float64(skip)/float64(t.rate), 2*math.Pi)
		t.lost += lost
		t.havePrev = false
		t.gateOK0 = false
		t.acc, t.accN = 0, 0
		// Realign the boxcar with the decimated grid.
		t.accN = int(t.n % int64(t.m))
		if t.accN != 0 {
			t.accN = -(t.m - t.accN) // drop samples until the next boundary
		}
	}
	if !t.acquired {
		return t.acquire(samples)
	}
	var out []Gate
	step := 2 * math.Pi * t.nco / float64(t.rate)
	for _, s := range samples {
		t.n++
		lo := complex(math.Cos(t.ncoPh), -math.Sin(t.ncoPh))
		t.ncoPh += step
		if t.ncoPh > 2*math.Pi {
			t.ncoPh -= 2 * math.Pi
		}
		if t.accN < 0 {
			t.accN++
			continue
		}
		t.acc += complex(float64(s), 0) * lo
		t.accN++
		if t.accN < t.m {
			continue
		}
		z := t.acc / complex(float64(t.m), 0)
		t.acc, t.accN = 0, 0
		if g, ok := t.addPoint(z); ok {
			out = append(out, g)
		}
	}
	return out
}

// addPoint adds one 100 Hz sample; at the end of a gate it returns a reading.
func (t *Tracker) addPoint(z complex128) (Gate, bool) {
	if t.havePrev && z != 0 && t.prevZ != 0 {
		t.phase += cmplx.Phase(z * cmplx.Conj(t.prevZ))
	} else if z != 0 {
		t.phase = cmplx.Phase(z)
	}
	if z != 0 {
		t.prevZ, t.havePrev = z, true
	}
	t.pts = append(t.pts, z)
	t.phs = append(t.phs, t.phase)
	per := decimRate * t.GateSec
	// Gates end on whole multiples of the gate length since t0.
	if t.n/int64(t.m)%int64(per) != 0 {
		return Gate{}, false
	}
	g := t.finishGate()
	t.pts, t.phs = t.pts[:0], t.phs[:0]
	t.lost, t.slip = 0, false
	return g, true
}

func (t *Tracker) finishGate() Gate {
	end := t.t0.Add(time.Duration(float64(t.n) / float64(t.rate) * float64(time.Second)))
	g := Gate{End: end.UTC(), Lost: t.lost, FreqHz: math.NaN(), FitHz: math.NaN()}
	per := decimRate * t.GateSec
	n := len(t.phs)
	if n < per/2 {
		g.Why = "partial gate"
		t.gatePh0, t.gateOK0 = t.phase, t.lost == 0 && n > 0
		return g
	}
	// Least-squares line through the phase.
	dt := 1.0 / decimRate
	var sx, sy, sxx, sxy float64
	for i, p := range t.phs {
		x := float64(i) * dt
		sx += x
		sy += p
		sxx += x * x
		sxy += x * p
	}
	fn := float64(n)
	slope := (fn*sxy - sx*sy) / (fn*sxx - sx*sx)
	icpt := (sy - slope*sx) / fn
	g.FitHz = t.nco + slope/(2*math.Pi)

	// Carrier and noise: remove the fitted phase and look at what is left.
	var mean complex128
	for i, z := range t.pts {
		p := icpt + slope*float64(i)*dt
		mean += z * cmplx.Exp(complex(0, -p))
	}
	mean /= complex(fn, 0)
	var noise float64
	worst := 0.0
	for i, z := range t.pts {
		p := icpt + slope*float64(i)*dt
		r := z*cmplx.Exp(complex(0, -p)) - mean
		noise += real(r)*real(r) + imag(r)*imag(r)
		if d := math.Abs(t.phs[i] - p); d > worst {
			worst = d
		}
	}
	noise /= fn
	amp := cmplx.Abs(mean)
	// The tone is real: the complex mix keeps half its amplitude.
	g.LevelDB = 20 * math.Log10(2*amp+1e-30)
	g.SNRdB = 10 * math.Log10(amp*amp/(noise+1e-30))

	if t.gateOK0 && t.lost == 0 {
		g.FreqHz = t.nco + (t.phase-t.gatePh0)/(2*math.Pi*float64(t.GateSec))
	}
	switch {
	case t.lost > 0:
		g.Why = "DAX packets lost"
	case g.SNRdB < minGateSNR:
		g.Why = "low SNR"
	case worst > maxStepRad:
		g.Why = "phase jump"
	case !t.gateOK0:
		g.Why = "first gate after a break"
	default:
		g.Valid = true
	}
	// Keep the tone centred: if it has wandered far from the NCO the boxcar
	// starts to attenuate it, so move the NCO (whole 0.01 Hz) and break phase.
	if g.SNRdB > minGateSNR && math.Abs(g.FitHz-t.nco) > 20 {
		t.setNCO(g.FitHz)
		t.breakPhase()
		t.gatePh0 = 0
		return g
	}
	t.gatePh0, t.gateOK0 = t.phase, true
	return g
}

func (t *Tracker) setNCO(f float64) {
	t.nco = math.Round(f*100) / 100
	t.status.NCO = t.nco
}

// acquire collects audio and finds the tone with an FFT.
func (t *Tracker) acquire(samples []float32) []Gate {
	for _, s := range samples {
		t.acqBuf = append(t.acqBuf, float64(s))
	}
	t.n += int64(len(samples))
	need := t.rate * acqSeconds
	if len(t.acqBuf) < need {
		return nil
	}
	f, snr := findPeak(t.acqBuf[:need], t.rate, t.Nominal, t.Search)
	t.acqBuf = t.acqBuf[:0]
	t.status.PeakHz, t.status.PeakSNR = f, snr
	if snr < minAcqSNR {
		return []Gate{{End: t.t0.Add(time.Duration(float64(t.n) / float64(t.rate) * float64(time.Second))).UTC(),
			FreqHz: math.NaN(), FitHz: math.NaN(), Why: "no carrier found"}}
	}
	t.setNCO(f)
	t.ncoPh = math.Mod(2*math.Pi*t.nco*float64(t.n)/float64(t.rate), 2*math.Pi)
	t.acquired, t.status.Acquired = true, true
	t.breakPhase()
	// Line up the boxcar with the 100 Hz grid.
	if r := int(t.n % int64(t.m)); r != 0 {
		t.accN = -(t.m - r)
	}
	return nil
}

// findPeak returns the strongest tone within nominal±search Hz and its height
// above the median bin in that range, dB.
func findPeak(x []float64, rate int, nominal, search float64) (float64, float64) {
	n := 1
	for n < len(x) {
		n <<= 1
	}
	buf := make([]complex128, n)
	// Hann window.
	for i, v := range x {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(len(x)-1))
		buf[i] = complex(v*w, 0)
	}
	fft(buf)
	bin := float64(rate) / float64(n)
	lo := int(math.Max(1, (nominal-search)/bin))
	hi := int(math.Min(float64(n/2-2), (nominal+search)/bin))
	if hi <= lo {
		return nominal, 0
	}
	pw := make([]float64, hi-lo+1)
	best := lo
	for k := lo; k <= hi; k++ {
		pw[k-lo] = real(buf[k])*real(buf[k]) + imag(buf[k])*imag(buf[k])
		if pw[k-lo] > pw[best-lo] {
			best = k
		}
	}
	med := median(pw)
	// Parabolic interpolation on log power.
	a := math.Log(pw[max(best-1, lo)-lo] + 1e-30)
	b := math.Log(pw[best-lo] + 1e-30)
	c := math.Log(pw[min(best+1, hi)-lo] + 1e-30)
	d := 0.0
	if den := a - 2*b + c; den != 0 {
		d = 0.5 * (a - c) / den
	}
	return (float64(best) + d) * bin, 10 * math.Log10(pw[best-lo]/(med+1e-30))
}

func median(v []float64) float64 {
	c := append([]float64(nil), v...)
	// Partial selection is plenty for a few thousand bins.
	k := len(c) / 2
	lo, hi := 0, len(c)-1
	for lo < hi {
		p := c[(lo+hi)/2]
		i, j := lo, hi
		for i <= j {
			for c[i] < p {
				i++
			}
			for c[j] > p {
				j--
			}
			if i <= j {
				c[i], c[j] = c[j], c[i]
				i++
				j--
			}
		}
		if k <= j {
			hi = j
		} else if k >= i {
			lo = i
		} else {
			break
		}
	}
	return c[k]
}

// fft is an in-place radix-2 FFT; len(a) must be a power of two.
func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u := a[start+k]
				v := a[start+k+size/2] * wk
				a[start+k] = u + v
				a[start+k+size/2] = u - v
				wk *= w
			}
		}
	}
}
