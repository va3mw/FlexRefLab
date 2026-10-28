// Package report builds the charts for a run and the self-contained HTML
// report that goes to engineering.
package report

import (
	"fmt"
	"math"
	"sort"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/plot"
	"github.com/va3mw/FlexRefLab/internal/store"
)

// RefColor is each reference's colour in every chart.
var RefColor = map[string]string{
	store.RefTCXO:     "#d97706",
	store.RefExternal: "#7c3aed",
	store.RefGPSDO:    "#2563eb",
	store.RefGNSS:     "#0d9488",
	store.RefUnknown:  "#6b7280",
}

var extra = []string{"#dc2626", "#16a34a", "#db2777", "#0891b2", "#65a30d", "#9333ea"}

func color(ref string) string {
	if c, ok := RefColor[ref]; ok {
		return c
	}
	return RefColor[store.RefUnknown]
}

// Charts are the SVGs for a run, keyed by name.
type Charts map[string]string

// ChartOrder is the order charts appear in.
var ChartOrder = []string{"offset", "offset_dev", "stability", "tdev", "hist", "spectrum", "temps", "tempco", "snr", "gps"}

// ChartTitles describe each chart.
var ChartTitles = map[string]string{
	"offset":     "Reference error over time",
	"offset_dev": "Deviation from each reference's mean",
	"stability":  "Frequency stability (ADEV, MDEV, HDEV)",
	"tdev":       "Time deviation",
	"hist":       "Distribution of readings",
	"spectrum":   "Spectrum of the offset (periodic components)",
	"temps":      "Radio temperatures",
	"tempco":     "Offset against temperature",
	"snr":        "Carrier SNR and level",
	"gps":        "GPS satellites tracked",
}

// Build makes all charts for a run. w is the chart width in pixels.
func Build(run *store.Run, res analysis.Result, w int) Charts {
	c := Charts{}
	ss := run.Samples
	if len(ss) == 0 {
		return c
	}
	const maxPts = 3000
	ts := func(s store.Sample) float64 { return float64(s.T.UnixNano()) / 1e9 }

	// Offset over time: one series per segment, so colour follows the
	// reference and lines break between segments.
	off := plot.Chart{Title: ChartTitles["offset"], YLabel: "ppb (+ = reference fast)", XLabel: "UTC", TimeX: true, W: w}
	dev := plot.Chart{Title: ChartTitles["offset_dev"], YLabel: "ppb from mean", XLabel: "UTC", TimeX: true, W: w}
	snr := plot.Chart{Title: ChartTitles["snr"], YLabel: "dB", XLabel: "UTC", TimeX: true, W: w, H: 240}
	gps := plot.Chart{Title: ChartTitles["gps"], YLabel: "satellites", XLabel: "UTC", TimeX: true, W: w, H: 220}
	start := 0
	for si, seg := range res.Segments {
		end := start + seg.Gates
		part := ss[start:min(end, len(ss))]
		start = end
		var x, y, yd, xs, ys []float64
		for _, s := range part {
			x = append(x, ts(s))
			v := math.NaN()
			if analysis.Usable(s) {
				v = s.FitPPB
				if math.IsNaN(v) {
					v = s.PPB
				}
			}
			y = append(y, v)
			yd = append(yd, v-seg.MeanPPB)
			if s.Phase == "settle" && s.Valid {
				xs, ys = append(xs, ts(s)), append(ys, s.PPB)
			}
		}
		name := seg.RefName
		dx, dy := plot.Decimate(x, y, maxPts/max(1, len(res.Segments)))
		off.Series = append(off.Series, plot.Series{Name: name, Color: color(seg.Ref), X: dx, Y: dy, Width: 1.2})
		if len(xs) > 0 {
			off.Series = append(off.Series, plot.Series{Color: color(seg.Ref), X: xs, Y: ys, Kind: "points"})
			off.Bands = append(off.Bands, plot.Band{X0: xs[0], X1: xs[len(xs)-1], Color: "#9ca3af", Label: "settling (excluded)"})
		}
		dx, dy = plot.Decimate(x, yd, maxPts/max(1, len(res.Segments)))
		dev.Series = append(dev.Series, plot.Series{Name: name, Color: color(seg.Ref), X: dx, Y: dy, Width: 1.1})
		_ = si
	}
	off.HLines = []plot.HLine{{Y: 0, Color: "#6b7280"}}
	c["offset"] = off.SVG()
	c["offset_dev"] = dev.SVG()

	// SNR, level, GPS for the whole run.
	var x, sn, lv, trk, vis []float64
	for _, s := range ss {
		x = append(x, ts(s))
		sn = append(sn, s.SNR)
		lv = append(lv, s.Level)
		t, v := math.NaN(), math.NaN()
		if s.GPSTracked > 0 || s.GPSVisible > 0 {
			t, v = float64(s.GPSTracked), float64(s.GPSVisible)
		}
		trk, vis = append(trk, t), append(vis, v)
	}
	a, b := plot.Decimate(x, sn, maxPts)
	snr.Series = append(snr.Series, plot.Series{Name: "SNR (100 Hz)", Color: "#2563eb", X: a, Y: b})
	a, b = plot.Decimate(x, lv, maxPts)
	snr.Series = append(snr.Series, plot.Series{Name: "level dBFS", Color: "#9ca3af", X: a, Y: b})
	c["snr"] = snr.SVG()
	if anyFinite(trk) {
		a, b = plot.Decimate(x, trk, maxPts)
		gps.Series = append(gps.Series, plot.Series{Name: "tracked", Color: "#0d9488", X: a, Y: b, Kind: "step"})
		a, b = plot.Decimate(x, vis, maxPts)
		gps.Series = append(gps.Series, plot.Series{Name: "visible", Color: "#9ca3af", X: a, Y: b, Kind: "step", Dash: true})
		c["gps"] = gps.SVG()
	}

	// Temperatures.
	if len(run.Meta.Temps) > 0 {
		tc := plot.Chart{Title: ChartTitles["temps"], YLabel: "°C", XLabel: "UTC", TimeX: true, W: w, H: 240}
		for i, n := range run.Meta.Temps {
			var ty []float64
			for _, s := range ss {
				v, ok := s.Temps[n]
				if !ok {
					v = math.NaN()
				}
				ty = append(ty, v)
			}
			a, b := plot.Decimate(x, ty, maxPts)
			tc.Series = append(tc.Series, plot.Series{Name: n, Color: extra[i%len(extra)], X: a, Y: b})
		}
		c["temps"] = tc.SVG()
		// Scatter against the sensor that best explains the offset.
		best, bestR := "", 0.0
		for _, seg := range res.Segments {
			for _, t := range seg.Temps {
				if math.Abs(t.R) > bestR {
					best, bestR = t.Name, math.Abs(t.R)
				}
			}
		}
		if best == "" {
			best = run.Meta.Temps[0]
		}
		sc := plot.Chart{Title: ChartTitles["tempco"] + " (" + best + ")", YLabel: "ppb", XLabel: best + " °C", W: w, H: 300}
		byRef := map[string][2][]float64{}
		for _, s := range ss {
			v, ok := s.Temps[best]
			if !ok || !analysis.Usable(s) {
				continue
			}
			p := byRef[s.Ref]
			p[0], p[1] = append(p[0], v), append(p[1], s.PPB)
			byRef[s.Ref] = p
		}
		for _, ref := range sortedKeys(byRef) {
			p := byRef[ref]
			px, py := thin(p[0], p[1], 2000)
			sc.Series = append(sc.Series, plot.Series{Name: store.RefNames[ref], Color: color(ref), X: px, Y: py, Kind: "points"})
			if f := analysis.LinFit(p[0], p[1]); !math.IsNaN(f.B) {
				lo, hi := analysis.MinMax(p[0])
				sc.Series = append(sc.Series, plot.Series{Color: color(ref), X: []float64{lo, hi}, Y: []float64{f.A + f.B*lo, f.A + f.B*hi}, Dash: true})
			}
		}
		c["tempco"] = sc.SVG()
	}

	// Stability: the longest segment of each reference.
	st := plot.Chart{Title: ChartTitles["stability"], XLabel: "τ (s)", YLabel: "σy(τ)", LogX: true, LogY: true, W: w, H: 380,
		Note: "solid ADEV ±1σ · dashed MDEV · thin HDEV"}
	td := plot.Chart{Title: ChartTitles["tdev"], XLabel: "τ (s)", YLabel: "TDEV (ns)", LogX: true, LogY: true, W: w, H: 300}
	sp := plot.Chart{Title: ChartTitles["spectrum"], XLabel: "frequency (cycles/min)", YLabel: "ppb peak", LogX: true, W: w, H: 280}
	hc := plot.Chart{Title: ChartTitles["hist"], XLabel: "ppb from the reference's mean", YLabel: "readings", W: w, H: 260}
	for _, r := range res.Refs {
		seg := res.Segments[r.Longest]
		col := color(r.Ref)
		var tx, ad, lo, hi, md, hd, tdv []float64
		for _, p := range seg.Taus {
			tx = append(tx, p.Tau)
			ad = append(ad, p.ADEV)
			lo = append(lo, p.ADEV-p.AErr)
			hi = append(hi, p.ADEV+p.AErr)
			md = append(md, p.MDEV)
			hd = append(hd, p.HDEV)
			tdv = append(tdv, p.TDEV*1e9)
		}
		st.Series = append(st.Series,
			plot.Series{Name: r.Name, Color: col, X: tx, Y: ad, Lo: lo, Hi: hi, Width: 2},
			plot.Series{Color: col, X: tx, Y: md, Dash: true},
			plot.Series{Color: col, X: tx, Y: hd, Width: 0.8})
		td.Series = append(td.Series, plot.Series{Name: r.Name, Color: col, X: tx, Y: tdv, Width: 2})
		if p := seg.Periodic; len(p.Freqs) > 0 {
			fx := make([]float64, len(p.Freqs))
			for i, f := range p.Freqs {
				fx[i] = f * 60
			}
			sp.Series = append(sp.Series, plot.Series{Name: r.Name, Color: col, X: fx, Y: p.Amps, Width: 1})
		}
		// Histogram of deviations from the mean, every segment of the reference.
		var d []float64
		start := 0
		for _, s := range res.Segments {
			end := start + s.Gates
			if s.Ref == r.Ref {
				for _, v := range ss[start:min(end, len(ss))] {
					if analysis.Usable(v) {
						d = append(d, v.PPB-s.MeanPPB)
					}
				}
			}
			start = end
		}
		if bins := analysis.Histogram(d, 40); len(bins) > 0 {
			var bx, by []float64
			for _, bn := range bins {
				bx, by = append(bx, bn.Lo), append(by, float64(bn.N))
			}
			bx = append(bx, bins[len(bins)-1].Hi)
			hc.Series = append(hc.Series, plot.Series{Name: fmt.Sprintf("%s (σ %.3g ppb)", r.Name, r.StdPPB), Color: col, X: bx, Y: by, Kind: "bars"})
		}
	}
	c["stability"] = st.SVG()
	c["tdev"] = td.SVG()
	c["hist"] = hc.SVG()
	if len(sp.Series) > 0 {
		c["spectrum"] = sp.SVG()
	}
	return c
}

// Live draws the live chart from recent samples.
func Live(ss []store.Sample, w int) string {
	ch := plot.Chart{Title: "Live reference error (least-squares, per gate)", YLabel: "ppb", XLabel: "UTC", TimeX: true, W: w, H: 300}
	var cur string
	var x, y []float64
	flush := func() {
		if len(x) > 0 {
			ch.Series = append(ch.Series, plot.Series{Name: store.RefNames[cur], Color: color(cur), X: x, Y: y})
		}
		x, y = nil, nil
	}
	for _, s := range ss {
		if s.Ref != cur {
			flush()
			cur = s.Ref
		}
		v := math.NaN()
		if s.Valid {
			v = s.FitPPB
		}
		x, y = append(x, float64(s.T.UnixNano())/1e9), append(y, v)
	}
	flush()
	return ch.SVG()
}

// Compare draws the stability of several runs' references together.
func Compare(rows []analysis.CompareRow, w int) (stability, offsets string) {
	st := plot.Chart{Title: "Stability across runs (ADEV)", XLabel: "τ (s)", YLabel: "σy(τ)", LogX: true, LogY: true, W: w, H: 400}
	of := plot.Chart{Title: "Mean offset by run and reference (±U)", XLabel: "row", YLabel: "ppb", W: w, H: 300}
	var ox, oy, olo, ohi []float64
	for i, r := range rows {
		var tx, ad []float64
		for _, p := range r.Taus {
			tx, ad = append(tx, p.Tau), append(ad, p.ADEV)
		}
		col := color(r.Ref.Ref)
		if i >= len(RefColor) {
			col = extra[i%len(extra)]
		}
		st.Series = append(st.Series, plot.Series{Name: fmt.Sprintf("%d %s", i+1, r.Ref.Name), Color: col, X: tx, Y: ad, Width: 1.6})
		ox, oy = append(ox, float64(i+1)), append(oy, r.Ref.MeanPPB)
		olo, ohi = append(olo, r.Ref.MeanPPB-r.Ref.UTotalPPB), append(ohi, r.Ref.MeanPPB+r.Ref.UTotalPPB)
	}
	of.Series = []plot.Series{{Color: "#1f2937", X: ox, Y: oy, Lo: olo, Hi: ohi, Kind: "points"}}
	of.HLines = []plot.HLine{{Y: 0, Color: "#6b7280"}}
	return st.SVG(), of.SVG()
}

func anyFinite(v []float64) bool {
	for _, x := range v {
		if !math.IsNaN(x) {
			return true
		}
	}
	return false
}

func thin(x, y []float64, n int) ([]float64, []float64) {
	if len(x) <= n {
		return x, y
	}
	step := float64(len(x)) / float64(n)
	var ox, oy []float64
	for f := 0.0; int(f) < len(x); f += step {
		ox, oy = append(ox, x[int(f)]), append(oy, y[int(f)])
	}
	return ox, oy
}

func sortedKeys[V any](m map[string]V) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}
