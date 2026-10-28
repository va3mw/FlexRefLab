package analysis

import (
	"fmt"
	"math"
	"math/cmplx"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/va3mw/FlexRefLab/internal/store"
)

// Usable reports whether a sample counts towards the statistics: a valid
// reading, outside a settling period, with a disciplined reference locked.
func Usable(s store.Sample) bool {
	return s.Valid && s.Phase != "settle" && (s.Locked || s.Ref == store.RefTCXO) && finite(s.PPB)
}

// TempStat is one temperature sensor over a segment.
type TempStat struct {
	Name     string  `json:"name"`
	Mean     float64 `json:"mean"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	PPBPerC  float64 `json:"ppb_per_c"` // slope of offset against temperature
	PPBPerCE float64 `json:"ppb_per_c_err"`
	R        float64 `json:"r"`
}

// Periodic is the strongest periodic component in the offset.
type Periodic struct {
	PeriodSec float64   `json:"period_sec"`
	AmpPPB    float64   `json:"amp_ppb"`  // peak amplitude (half the peak-to-peak)
	PromDB    float64   `json:"prom_db"`  // height above the median of the spectrum
	Found     bool      `json:"found"`    // prominent enough to report
	SpanSec   float64   `json:"span_sec"` // length of data analysed
	Freqs     []float64 `json:"freqs"`    // spectrum for plotting, Hz
	Amps      []float64 `json:"amps"`     // ppb
}

// Segment is a stretch on one reference (one sweep step, or the part of a
// run between reference changes).
type Segment struct {
	Index      int            `json:"index"`
	Step       int            `json:"step"`
	Ref        string         `json:"ref"`
	RefName    string         `json:"ref_name"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Gates      int            `json:"gates"` // all samples, settling included
	Used       int            `json:"used"`  // samples in the statistics
	Settling   int            `json:"settling"`
	Invalid    int            `json:"invalid"`
	Unlocked   int            `json:"unlocked"`
	Why        map[string]int `json:"why"` // reasons readings were not valid
	MeanPPB    float64        `json:"mean_ppb"`
	MeanHz     float64        `json:"mean_hz"` // at the carrier
	StdPPB     float64        `json:"std_ppb"`
	MedianPPB  float64        `json:"median_ppb"`
	MinPPB     float64        `json:"min_ppb"`
	MaxPPB     float64        `json:"max_ppb"`
	P2PPPB     float64        `json:"p2p_ppb"`
	UMeanPPB   float64        `json:"u_mean_ppb"` // statistical uncertainty of the mean
	DriftPPBh  float64        `json:"drift_ppb_h"`
	DriftErr   float64        `json:"drift_err"`
	DriftR2    float64        `json:"drift_r2"`
	SettleSec  float64        `json:"settle_sec"` // NaN: did not settle
	SettleTol  float64        `json:"settle_tol"`
	SNRMean    float64        `json:"snr_mean"`
	SNRMin     float64        `json:"snr_min"`
	SliceErrMH float64        `json:"slice_err_mhz"`
	GPSTrkMean float64        `json:"gps_trk_mean"`
	GPSTrkMin  int            `json:"gps_trk_min"`
	GPSErrPPB  float64        `json:"gps_err_ppb"` // the radio's GPS freq_error (fractional, as ppb)
	Taus       []TauPoint     `json:"taus"`
	Noise      string         `json:"noise"` // dominant noise at short tau
	Temps      []TempStat     `json:"temps"`
	Periodic   Periodic       `json:"periodic"`
	Hist       []Bin          `json:"hist"`
}

// RefSummary pools a reference's segments.
type RefSummary struct {
	Ref       string  `json:"ref"`
	Name      string  `json:"name"`
	Segments  int     `json:"segments"`
	Hours     float64 `json:"hours"`
	Used      int     `json:"used"`
	MeanPPB   float64 `json:"mean_ppb"`
	MeanHz    float64 `json:"mean_hz"`
	StdPPB    float64 `json:"std_ppb"`
	RepeatPPB float64 `json:"repeat_ppb"` // scatter of segment means (NaN with one segment)
	UStatPPB  float64 `json:"u_stat_ppb"`
	UTotalPPB float64 `json:"u_total_ppb"` // with the stimulus' uncertainty
	DriftPPBh float64 `json:"drift_ppb_h"`
	DriftErr  float64 `json:"drift_err"` // of the longest segment
	ADEV1     float64 `json:"adev1"`
	ADEV10    float64 `json:"adev10"`
	ADEV100   float64 `json:"adev100"`
	ADEV1000  float64 `json:"adev1000"`
	SettleSec float64 `json:"settle_sec"`
	SpecPPB   float64 `json:"spec_ppb"`
	Pass      string  `json:"pass"` // PASS, FAIL or "" (no limit)
	Baseline  bool    `json:"baseline"`
	DeltaPPB  float64 `json:"delta_ppb"` // minus the baseline reference
	DeltaUPPB float64 `json:"delta_u_ppb"`
	Longest   int     `json:"longest"` // segment index the stability numbers come from
}

// Result is the analysis of one run.
type Result struct {
	Meta      store.Meta   `json:"meta"`
	Samples   int          `json:"samples"`
	Duration  float64      `json:"duration_sec"`
	Segments  []Segment    `json:"segments"`
	Refs      []RefSummary `json:"refs"`
	Findings  []Finding    `json:"findings"`
	CarrierHz float64      `json:"carrier_hz"`
}

// Finding is a plain-language observation for the report.
type Finding struct {
	Level string `json:"level"` // info, good, warn, bad
	Msg   string `json:"msg"`
}

// refOrder ranks references for display and for choosing the baseline.
var refOrder = map[string]int{store.RefGNSS: 0, store.RefGPSDO: 1, store.RefExternal: 2, store.RefTCXO: 3, store.RefUnknown: 4}

// Analyze computes everything for a run.
func Analyze(run *store.Run) Result {
	m := run.Meta
	res := Result{Meta: m, Samples: len(run.Samples), CarrierHz: m.Stimulus.CarrierHz}
	ss := run.Samples
	if len(ss) > 1 {
		res.Duration = ss[len(ss)-1].T.Sub(ss[0].T).Seconds()
	}
	gate := float64(m.GateSec)
	if gate <= 0 {
		gate = 1
	}
	// Split into segments.
	start := 0
	for i := 1; i <= len(ss); i++ {
		if i < len(ss) && ss[i].Step == ss[start].Step && ss[i].Ref == ss[start].Ref {
			continue
		}
		seg := analyzeSegment(ss[start:i], gate, m)
		seg.Index = len(res.Segments)
		res.Segments = append(res.Segments, seg)
		start = i
	}
	res.Refs = summarize(res.Segments, m)
	res.Findings = findings(res)
	return res
}

func analyzeSegment(ss []store.Sample, gate float64, m store.Meta) Segment {
	g := Segment{Step: ss[0].Step, Ref: ss[0].Ref, RefName: store.RefNames[ss[0].Ref], Start: ss[0].T, End: ss[len(ss)-1].T,
		Gates: len(ss), Why: map[string]int{}, SettleSec: math.NaN()}
	if g.RefName == "" {
		g.RefName = g.Ref
	}
	var t0 time.Time
	var ppb, fit, tsec, snr, slerr, gpsErr []float64
	trkMin, trkN, trkSum := math.MaxInt, 0, 0
	for _, s := range ss {
		switch {
		case s.Phase == "settle":
			g.Settling++
		case !s.Valid:
			g.Invalid++
			g.Why[s.Why]++
		case !(s.Locked || s.Ref == store.RefTCXO):
			g.Unlocked++
		}
		if !Usable(s) {
			continue
		}
		if t0.IsZero() {
			t0 = s.T
		}
		g.Used++
		ppb = append(ppb, s.PPB)
		fit = append(fit, s.FitPPB)
		tsec = append(tsec, s.T.Sub(t0).Seconds())
		snr = append(snr, s.SNR)
		slerr = append(slerr, s.SliceErrMH)
		if v, err := strconv.ParseFloat(strings.TrimSpace(s.GPSFreqErr), 64); err == nil && (s.GPSTracked > 0 || v != 0) {
			gpsErr = append(gpsErr, v*1e9)
		}
		if s.GPSVisible > 0 || s.GPSTracked > 0 {
			trkN++
			trkSum += s.GPSTracked
			trkMin = min(trkMin, s.GPSTracked)
		}
	}
	nan := math.NaN()
	g.MeanPPB, g.StdPPB, g.MedianPPB = Mean(ppb), Std(ppb), Quantile(ppb, 0.5)
	g.MinPPB, g.MaxPPB = MinMax(ppb)
	g.P2PPPB = g.MaxPPB - g.MinPPB
	g.MeanHz = g.MeanPPB * m.Stimulus.CarrierHz / 1e9
	g.SNRMean = Mean(snr)
	g.SNRMin, _ = MinMax(snr)
	g.SliceErrMH = Mean(slerr)
	g.GPSErrPPB = Mean(gpsErr)
	g.GPSTrkMean, g.GPSTrkMin = nan, 0
	if trkN > 0 {
		g.GPSTrkMean, g.GPSTrkMin = float64(trkSum)/float64(trkN), trkMin
	}
	f := LinFit(tsec, ppb)
	g.DriftPPBh, g.DriftErr, g.DriftR2 = f.B*3600, f.BErr*3600, f.R2
	g.Hist = Histogram(ppb, 30)

	// Allan family on an even time grid from the first to the last usable gate.
	if g.Used >= 4 {
		n := int(math.Round(tsec[len(tsec)-1]/gate)) + 1
		y := make([]float64, n)
		for i := range y {
			y[i] = nan
		}
		for i, ts := range tsec {
			if k := int(math.Round(ts / gate)); k >= 0 && k < n {
				y[k] = ppb[i] * 1e-9
			}
		}
		g.Taus = Deviations(y, gate)
		if len(g.Taus) >= 3 {
			g.Noise = NoiseType(g.Taus[0], g.Taus[2])
		}
		// Uncertainty of the mean: the Allan deviation at the longest tau
		// that fits, which (unlike std/sqrt(N)) holds for correlated noise.
		g.UMeanPPB = g.StdPPB / math.Sqrt(float64(g.Used))
		if len(g.Taus) > 0 {
			if a := g.Taus[len(g.Taus)-1].ADEV * 1e9; finite(a) && a > g.UMeanPPB {
				g.UMeanPPB = a
			}
		}
		g.Periodic = periodic(y, gate)
	} else {
		g.UMeanPPB = nan
	}
	g.SettleSec, g.SettleTol = settling(ss, gate)
	g.Temps = temps(ss, m.Temps)
	return g
}

// settling finds how long after the step began a 10-gate running mean last
// came within tolerance of the final value (the median of the last quarter).
func settling(ss []store.Sample, gate float64) (float64, float64) {
	var t []float64
	var v []float64
	for _, s := range ss {
		if s.Valid && finite(s.PPB) {
			t = append(t, s.T.Sub(ss[0].T).Seconds())
			v = append(v, s.PPB)
		}
	}
	const w = 10
	if len(v) < 4*w {
		return math.NaN(), math.NaN()
	}
	avg := make([]float64, len(v)-w+1)
	for i := range avg {
		avg[i] = Mean(v[i : i+w])
	}
	tail := avg[len(avg)*3/4:]
	final := Quantile(tail, 0.5)
	tol := math.Max(0.5, 3*Std(tail))
	last := -1
	for i, a := range avg {
		if math.Abs(a-final) > tol {
			last = i
		}
	}
	if last >= len(avg)*3/4 {
		return math.NaN(), tol
	}
	if last < 0 {
		return 0, tol
	}
	return t[last+w] + gate, tol
}

func temps(ss []store.Sample, names []string) []TempStat {
	var out []TempStat
	for _, n := range names {
		var x, y []float64
		for _, s := range ss {
			v, ok := s.Temps[n]
			if !ok || !Usable(s) {
				continue
			}
			x = append(x, v)
			y = append(y, s.PPB)
		}
		if len(x) < 3 {
			continue
		}
		lo, hi := MinMax(x)
		f := LinFit(x, y)
		out = append(out, TempStat{Name: n, Mean: Mean(x), Min: lo, Max: hi, PPBPerC: f.B, PPBPerCE: f.BErr, R: f.R})
	}
	return out
}

// periodic looks for a periodic wobble in the longest gap-free stretch.
func periodic(y []float64, gate float64) Periodic {
	bs, bl, s := 0, 0, -1
	for i := 0; i <= len(y); i++ {
		if i < len(y) && finite(y[i]) {
			if s < 0 {
				s = i
			}
			continue
		}
		if s >= 0 && i-s > bl {
			bs, bl = s, i-s
		}
		s = -1
	}
	p := Periodic{}
	if bl < 128 {
		return p
	}
	n := 1
	for n*2 <= bl && n < 16384 {
		n *= 2
	}
	seg := y[bs+bl-n : bs+bl]
	// Detrend, window and transform.
	ts := make([]float64, n)
	for i := range ts {
		ts[i] = float64(i)
	}
	f := LinFit(ts, seg)
	buf := make([]complex128, n)
	wsum := 0.0
	for i, v := range seg {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		wsum += w
		buf[i] = complex((v-(f.A+f.B*float64(i)))*1e9*w, 0)
	}
	fft(buf)
	half := n / 2
	amps := make([]float64, half)
	for k := range amps {
		amps[k] = 2 * cmplx.Abs(buf[k]) / wsum
	}
	p.SpanSec = float64(n) * gate
	best := 3
	for k := 3; k < half; k++ {
		if amps[k] > amps[best] {
			best = k
		}
	}
	med := Quantile(amps[3:], 0.5)
	p.AmpPPB = amps[best]
	p.PeriodSec = float64(n) * gate / float64(best)
	if med > 0 {
		p.PromDB = 20 * math.Log10(amps[best]/med)
	}
	// Need a clear peak with at least three cycles in the data.
	p.Found = p.PromDB > 14 && best >= 3
	step := max(1, half/400)
	for k := 1; k < half; k += step {
		p.Freqs = append(p.Freqs, float64(k)/(float64(n)*gate))
		p.Amps = append(p.Amps, amps[k])
	}
	return p
}

func summarize(segs []Segment, m store.Meta) []RefSummary {
	by := map[string][]Segment{}
	for _, s := range segs {
		if s.Used > 0 {
			by[s.Ref] = append(by[s.Ref], s)
		}
	}
	var out []RefSummary
	for ref, list := range by {
		r := RefSummary{Ref: ref, Name: store.RefNames[ref], Segments: len(list), SettleSec: math.NaN(), SpecPPB: math.NaN(),
			RepeatPPB: math.NaN(), DeltaPPB: math.NaN(), DeltaUPPB: math.NaN()}
		var means, drifts, dw, settles []float64
		var wsum, wmean float64
		longest := list[0]
		for _, s := range list {
			r.Used += s.Used
			r.Hours += s.End.Sub(s.Start).Hours()
			means = append(means, s.MeanPPB)
			wsum += float64(s.Used)
			wmean += float64(s.Used) * s.MeanPPB
			if finite(s.DriftPPBh) && s.Used > 60 {
				drifts = append(drifts, s.DriftPPBh)
				dw = append(dw, float64(s.Used))
			}
			if finite(s.SettleSec) {
				settles = append(settles, s.SettleSec)
			}
			if s.Used > longest.Used {
				longest = s
			}
		}
		r.Longest = longest.Index
		r.MeanPPB = wmean / wsum
		r.MeanHz = r.MeanPPB * m.Stimulus.CarrierHz / 1e9
		// Pooled scatter: mean of the segments' variances.
		var vs float64
		for _, s := range list {
			if finite(s.StdPPB) {
				vs += s.StdPPB * s.StdPPB * float64(s.Used)
			}
		}
		r.StdPPB = math.Sqrt(vs / wsum)
		r.UStatPPB = longest.UMeanPPB / math.Sqrt(float64(len(list)))
		if len(list) >= 2 {
			r.RepeatPPB = Std(means)
			r.UStatPPB = math.Max(r.UStatPPB, r.RepeatPPB/math.Sqrt(float64(len(list))))
		}
		r.UTotalPPB = math.Hypot(r.UStatPPB, m.Stimulus.UncertPPB)
		if len(drifts) > 0 {
			var s, w float64
			for i := range drifts {
				s += drifts[i] * dw[i]
				w += dw[i]
			}
			r.DriftPPBh = s / w
			r.DriftErr = longest.DriftErr
		} else {
			r.DriftPPBh = math.NaN()
		}
		r.SettleSec = Quantile(settles, 0.5)
		r.ADEV1, r.ADEV10, r.ADEV100, r.ADEV1000 = adevAt(longest.Taus, 1), adevAt(longest.Taus, 10), adevAt(longest.Taus, 100), adevAt(longest.Taus, 1000)
		if lim, ok := m.Spec[ref]; ok && lim > 0 {
			r.SpecPPB = lim
			r.Pass = "PASS"
			if math.Abs(r.MeanPPB)+r.UTotalPPB > lim {
				r.Pass = "FAIL"
				if math.Abs(r.MeanPPB)-r.UTotalPPB <= lim {
					r.Pass = "MARGINAL"
				}
			}
		}
		if r.Name == "" {
			r.Name = ref
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return refOrder[out[i].Ref] < refOrder[out[j].Ref] })
	// Baseline: the best disciplined reference measured.
	for i := range out {
		if out[i].Ref == store.RefGNSS || out[i].Ref == store.RefGPSDO || out[i].Ref == store.RefExternal {
			out[i].Baseline = true
			b := out[i]
			for j := range out {
				// The stimulus' own error is common to both, so it cancels.
				out[j].DeltaPPB = out[j].MeanPPB - b.MeanPPB
				out[j].DeltaUPPB = math.Hypot(out[j].UStatPPB, b.UStatPPB)
				if j == i {
					out[j].DeltaUPPB = 0
				}
			}
			break
		}
	}
	return out
}

func adevAt(taus []TauPoint, tau float64) float64 {
	for _, t := range taus {
		if math.Abs(t.Tau-tau) < 1e-6 {
			return t.ADEV
		}
	}
	return math.NaN()
}

func findings(r Result) []Finding {
	var out []Finding
	add := func(level, f string, a ...any) { out = append(out, Finding{level, fmt.Sprintf(f, a...)}) }
	c := r.CarrierHz
	if c <= 0 {
		add("bad", "No carrier frequency is set, so no offset could be computed.")
		return out
	}
	for _, s := range r.Refs {
		hz := s.MeanPPB * c / 1e9
		msg := fmt.Sprintf("%s: offset %+.3f ppb (%+.4f Hz at %s) ± %.3f ppb over %s",
			s.Name, s.MeanPPB, hz, MHz(c), s.UTotalPPB, Dur(s.Hours*3600))
		switch s.Pass {
		case "PASS":
			add("good", "%s, within the ±%g ppb limit.", msg, s.SpecPPB)
		case "FAIL":
			add("bad", "%s, OUTSIDE the ±%g ppb limit.", msg, s.SpecPPB)
		case "MARGINAL":
			add("warn", "%s: the ±%g ppb limit lies within the uncertainty; measure longer or with a better stimulus.", msg, s.SpecPPB)
		default:
			add("info", "%s.", msg)
		}
		if !s.Baseline && finite(s.DeltaPPB) {
			add("info", "%s differs from the baseline reference by %+.3f ± %.3f ppb (stimulus error cancels).", s.Name, s.DeltaPPB, s.DeltaUPPB)
		}
		if finite(s.RepeatPPB) && s.Segments >= 2 {
			add("info", "%s returns to the same offset within %.3f ppb (1σ) across %d visits.", s.Name, s.RepeatPPB, s.Segments)
		}
		// Drift over a short stretch is mostly noise: need 15 min and 3σ.
		if finite(s.DriftPPBh) && math.Abs(s.DriftPPBh) > 0.5 && s.Hours >= 0.25 && math.Abs(s.DriftPPBh) > 3*s.DriftErr {
			add("warn", "%s drifts %+.2f ppb/hour during measurement.", s.Name, s.DriftPPBh)
		}
		if finite(s.SettleSec) && s.SettleSec > 0 {
			add("info", "%s settles within %.0f s after switching.", s.Name, s.SettleSec)
		}
	}
	for _, g := range r.Segments {
		if g.Used == 0 {
			continue
		}
		if g.Unlocked > 0 {
			add("warn", "Segment %d (%s): %d readings taken while the reference was not locked were excluded.", g.Index+1, g.RefName, g.Unlocked)
		}
		if bad := g.Invalid; bad > g.Gates/10 {
			var why []string
			for k, v := range g.Why {
				why = append(why, fmt.Sprintf("%s ×%d", k, v))
			}
			sort.Strings(why)
			add("warn", "Segment %d (%s): %d of %d readings not valid (%s).", g.Index+1, g.RefName, bad, g.Gates, strings.Join(why, ", "))
		}
		if g.Periodic.Found {
			add("warn", "Segment %d (%s): periodic wobble, period %.1f s (%.2f cycles/min), %.3f ppb peak (%.4f Hz at the carrier).",
				g.Index+1, g.RefName, g.Periodic.PeriodSec, 60/g.Periodic.PeriodSec, g.Periodic.AmpPPB, g.Periodic.AmpPPB*c/1e9)
		}
		for _, t := range g.Temps {
			if math.Abs(t.R) > 0.7 && t.Max-t.Min > 0.5 {
				add("info", "Segment %d (%s): offset follows %s, %+.2f ± %.2f ppb/°C (r = %.2f, %.1f–%.1f °C).",
					g.Index+1, g.RefName, t.Name, t.PPBPerC, t.PPBPerCE, t.R, t.Min, t.Max)
			}
		}
		if g.Noise == "white PM (measurement noise)" && g.Ref != store.RefTCXO {
			add("info", "Segment %d (%s): short-term stability is limited by measurement noise (SNR %.0f dB); the radio is better than ADEV(1 s) shows. Use MDEV/TDEV, or raise the carrier level.",
				g.Index+1, g.RefName, g.SNRMean)
		}
		if finite(g.SNRMean) && g.SNRMean < 20 {
			add("warn", "Segment %d (%s): weak carrier (mean SNR %.0f dB in 100 Hz).", g.Index+1, g.RefName, g.SNRMean)
		}
		if finite(g.GPSTrkMean) && g.GPSTrkMin < 4 && (g.Ref == store.RefGPSDO || g.Ref == store.RefGNSS) {
			add("warn", "Segment %d (%s): GPS tracked as few as %d satellites.", g.Index+1, g.RefName, g.GPSTrkMin)
		}
	}
	// "chu" only appears in old runs; CHU no longer transmits.
	if k := r.Meta.Stimulus.Kind; k == "wwv" || k == "chu" || k == "other" {
		add("info", "The stimulus is received off the air (%s): ionospheric Doppler adds roughly ±0.01–0.1 ppm at HF, so compare references with a lab carrier for ppb-level results.", r.Meta.Stimulus.Label)
	}
	return out
}

// MHz formats a frequency in Hz as MHz.
func MHz(hz float64) string { return fmt.Sprintf("%.6f MHz", hz/1e6) }

// Dur formats seconds briefly.
func Dur(sec float64) string {
	switch {
	case sec < 120:
		return fmt.Sprintf("%.0f s", sec)
	case sec < 7200:
		return fmt.Sprintf("%.1f min", sec/60)
	}
	return fmt.Sprintf("%.2f h", sec/3600)
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

// Compare lines up several runs' reference summaries.
type CompareRow struct {
	RunID   string     `json:"run_id"`
	RunName string     `json:"run_name"`
	Radio   string     `json:"radio"`
	Created time.Time  `json:"created"`
	Ref     RefSummary `json:"ref"`
	Taus    []TauPoint `json:"taus"`
}

// CompareRuns flattens analyses into rows, one per run and reference.
func CompareRuns(rs []Result) []CompareRow {
	var out []CompareRow
	for _, r := range rs {
		radio := strings.TrimSpace(r.Meta.Radio.Model + " " + r.Meta.Radio.Serial)
		for _, ref := range r.Refs {
			row := CompareRow{RunID: r.Meta.ID, RunName: r.Meta.Name, Radio: radio, Created: r.Meta.Created, Ref: ref}
			if ref.Longest < len(r.Segments) {
				row.Taus = r.Segments[ref.Longest].Taus
			}
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return refOrder[out[i].Ref.Ref] < refOrder[out[j].Ref.Ref] })
	return out
}
