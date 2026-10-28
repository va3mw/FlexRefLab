package report

import (
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/store"
)

const css = `
body{font-family:"Segoe UI",Arial,sans-serif;color:#111827;background:#f3f4f6;margin:0}
main{max-width:1000px;margin:0 auto;padding:24px 16px 48px;background:#fff}
h1{font-size:24px;margin:0 0 4px}h2{font-size:18px;margin:28px 0 8px;border-bottom:2px solid #e5e7eb;padding-bottom:4px}
h3{font-size:15px;margin:18px 0 6px}.sub{color:#6b7280;margin:0 0 16px}
table{border-collapse:collapse;width:100%;font-size:13px;margin:6px 0 12px}
th,td{border:1px solid #e5e7eb;padding:4px 6px;text-align:left;vertical-align:top}
th{background:#f9fafb;font-weight:600}td.n{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.kv td:first-child{width:28%;color:#4b5563}
.f{padding:6px 10px;border-left:4px solid #9ca3af;margin:4px 0;background:#f9fafb;font-size:14px}
.f.good{border-color:#16a34a}.f.warn{border-color:#d97706;background:#fffbeb}.f.bad{border-color:#dc2626;background:#fef2f2}
.PASS{color:#15803d;font-weight:600}.FAIL{color:#b91c1c;font-weight:600}.MARGINAL{color:#b45309;font-weight:600}
svg.chart{width:100%;height:auto;border:1px solid #e5e7eb;margin:6px 0}
.small{font-size:12px;color:#6b7280}code{background:#f3f4f6;padding:0 3px}
@media print{body{background:#fff}main{padding:0}h2{break-after:avoid}svg.chart{break-inside:avoid}}
`

func e(s string) string { return html.EscapeString(s) }

func num(v float64, prec int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	return fmt.Sprintf("%.*f", prec, v)
}

func sgn(v float64, prec int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	return fmt.Sprintf("%+.*f", prec, v)
}

func sci(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2e", v)
}

// HTML renders the engineering report for a run.
func HTML(run *store.Run, res analysis.Result, charts Charts, appVersion string) string {
	m := run.Meta
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	title := m.Name
	if title == "" {
		title = m.ID
	}
	p(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s — Reference Report</title><style>%s</style></head><body><main>`, e(title), css)
	p(`<h1>Frequency reference report: %s</h1>`, e(title))
	radio := strings.TrimSpace(m.Radio.Model + " " + m.Radio.Nickname)
	p(`<p class="sub">%s · serial %s · %s UTC · %s · Flex Reference Lab %s</p>`, e(radio), e(orDash(m.Radio.Serial)),
		m.Created.UTC().Format("2006-01-02 15:04"), analysis.Dur(res.Duration), e(appVersion))

	p(`<h2>Findings</h2>`)
	for _, f := range res.Findings {
		p(`<div class="f %s">%s</div>`, f.Level, e(f.Msg))
	}

	p(`<h2>Summary by reference</h2>`)
	p(`<table><tr><th>Reference</th><th>Visits</th><th>Time</th><th>Mean offset ppb</th><th>Hz at carrier</th><th>U (1σ) ppb</th><th>Δ vs baseline ppb</th><th>Scatter σ ppb</th><th>Repeatability ppb</th><th>Drift ppb/h</th><th>ADEV 1 s</th><th>ADEV 10 s</th><th>ADEV 100 s</th><th>ADEV 1000 s</th><th>Settle s</th><th>Limit ±ppb</th><th>Result</th></tr>`)
	for _, r := range res.Refs {
		base := ""
		if r.Baseline {
			base = " (baseline)"
		}
		delta := sgn(r.DeltaPPB, 3)
		if !r.Baseline && !math.IsNaN(r.DeltaUPPB) {
			delta += " ± " + num(r.DeltaUPPB, 3)
		}
		p(`<tr><td><b>%s</b>%s</td><td class="n">%d</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="%s">%s</td></tr>`,
			e(r.Name), base, r.Segments, analysis.Dur(r.Hours*3600), sgn(r.MeanPPB, 3), sgn(r.MeanHz, 4), num(r.UTotalPPB, 3), delta,
			num(r.StdPPB, 3), num(r.RepeatPPB, 3), sgn(r.DriftPPBh, 3), sci(r.ADEV1), sci(r.ADEV10), sci(r.ADEV100), sci(r.ADEV1000),
			num(r.SettleSec, 0), num(r.SpecPPB, 1), r.Pass, orDash(r.Pass))
	}
	p(`</table><p class="small">Offset is the radio reference's fractional frequency error: positive means the reference runs fast, so the radio reads every frequency low by that fraction. U combines the statistical uncertainty (the larger of the Allan deviation at the longest averaging time and the scatter between visits) with the stimulus' stated uncertainty. Differences between references use the same carrier, so the stimulus error cancels in Δ.</p>`)

	p(`<h2>Test setup</h2><table class="kv">`)
	kv := func(k, v string) { p(`<tr><td>%s</td><td>%s</td></tr>`, e(k), e(v)) }
	kv("Radio", fmt.Sprintf("%s “%s”, serial %s, firmware %s, %s", m.Radio.Model, m.Radio.Nickname, orDash(m.Radio.Serial), orDash(m.Radio.Version), m.Radio.IP))
	if m.Radio.Options != "" {
		kv("References fitted", m.Radio.Options)
	}
	kv("Radio frequency correction (freq_error_ppb)", orDash(m.FreqErrPPB))
	kv("Stimulus", fmt.Sprintf("%s — %s, %s", strings.ToUpper(m.Stimulus.Kind), m.Stimulus.Label, analysis.MHz(m.Stimulus.CarrierHz)))
	kv("Stimulus uncertainty", fmt.Sprintf("%g ppb (1σ)", m.Stimulus.UncertPPB))
	if m.Stimulus.Notes != "" {
		kv("Stimulus notes", m.Stimulus.Notes)
	}
	kv("Measurement", fmt.Sprintf("slice %s in DIGU at %s (tone %.1f Hz), DAX channel %d, %d s gates, source %s",
		m.Slice, analysis.MHz(float64(m.DialHz)), m.ToneHz, m.DAX, m.GateSec, m.Source))
	if m.Plan != nil {
		kv("Reference sweep", fmt.Sprintf("%s; settle %d s, measure %d s per step, %d cycle(s); restore %s",
			strings.Join(m.Plan.Refs, " → "), m.Plan.SettleSec, m.Plan.DwellSec, m.Plan.Cycles, orDash(m.Plan.Restore)))
	}
	kv("Started / ended (UTC)", m.Created.UTC().Format(time.DateTime)+" / "+fmtTime(m.Ended))
	kv("Operator", orDash(m.Operator))
	if m.Notes != "" {
		kv("Notes", m.Notes)
	}
	kv("Readings", fmt.Sprintf("%d gates", res.Samples))
	p(`</table>`)

	p(`<h2>Charts</h2>`)
	for _, k := range ChartOrder {
		if svg, ok := charts[k]; ok {
			b.WriteString(svg)
		}
	}

	p(`<h2>Segments</h2><table><tr><th>#</th><th>Reference</th><th>Start UTC</th><th>Length</th><th>Used</th><th>Settling</th><th>Invalid</th><th>Unlocked</th><th>Mean ppb</th><th>U ppb</th><th>σ ppb</th><th>Min / max ppb</th><th>Drift ppb/h (R²)</th><th>Settle s</th><th>SNR dB (min)</th><th>Slice err mHz</th><th>GPS trk (min)</th><th>GPS freq_error ppb</th><th>Noise at short τ</th></tr>`)
	for _, s := range res.Segments {
		p(`<tr><td>%d</td><td>%s</td><td>%s</td><td class="n">%s</td><td class="n">%d</td><td class="n">%d</td><td class="n">%d</td><td class="n">%d</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s / %s</td><td class="n">%s (%s)</td><td class="n">%s</td><td class="n">%s (%s)</td><td class="n">%s</td><td class="n">%s (%d)</td><td class="n">%s</td><td>%s</td></tr>`,
			s.Index+1, e(s.RefName), s.Start.UTC().Format("15:04:05"), analysis.Dur(s.End.Sub(s.Start).Seconds()), s.Used, s.Settling, s.Invalid, s.Unlocked,
			sgn(s.MeanPPB, 3), num(s.UMeanPPB, 3), num(s.StdPPB, 3), sgn(s.MinPPB, 2), sgn(s.MaxPPB, 2), sgn(s.DriftPPBh, 3), num(s.DriftR2, 2),
			num(s.SettleSec, 0), num(s.SNRMean, 1), num(s.SNRMin, 1), sgn(s.SliceErrMH, 3), num(s.GPSTrkMean, 1), s.GPSTrkMin, sgn(s.GPSErrPPB, 5), e(s.Noise))
	}
	p(`</table>`)

	p(`<h2>Stability tables</h2>`)
	for _, r := range res.Refs {
		seg := res.Segments[r.Longest]
		p(`<h3>%s — segment %d (%s)</h3><table><tr><th>τ s</th><th>ADEV</th><th>±1σ</th><th>MDEV</th><th>HDEV</th><th>TDEV ns</th><th>ADEV ppb</th><th>terms</th></tr>`,
			e(r.Name), seg.Index+1, analysis.Dur(seg.End.Sub(seg.Start).Seconds()))
		for _, t := range seg.Taus {
			p(`<tr><td class="n">%g</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%d</td></tr>`,
				t.Tau, sci(t.ADEV), sci(t.AErr), sci(t.MDEV), sci(t.HDEV), num(t.TDEV*1e9, 3), num(t.ADEV*1e9, 4), t.N)
		}
		p(`</table>`)
	}

	var trows []string
	for _, s := range res.Segments {
		for _, t := range s.Temps {
			trows = append(trows, fmt.Sprintf(`<tr><td>%d %s</td><td>%s</td><td class="n">%s</td><td class="n">%s – %s</td><td class="n">%s ± %s</td><td class="n">%s</td></tr>`,
				s.Index+1, e(s.RefName), e(t.Name), num(t.Mean, 2), num(t.Min, 2), num(t.Max, 2), sgn(t.PPBPerC, 3), num(t.PPBPerCE, 3), num(t.R, 2)))
		}
	}
	if len(trows) > 0 {
		p(`<h2>Temperature</h2><table><tr><th>Segment</th><th>Sensor</th><th>Mean °C</th><th>Range °C</th><th>Offset slope ppb/°C</th><th>r</th></tr>%s</table>`, strings.Join(trows, ""))
		p(`<p class="small">A slope with |r| near 1 over a useful temperature range is a temperature coefficient; with a small range or low |r| it is not meaningful.</p>`)
	}

	var prow []string
	for _, s := range res.Segments {
		if pp := s.Periodic; pp.SpanSec > 0 {
			prow = append(prow, fmt.Sprintf(`<tr><td>%d %s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td>%s</td></tr>`,
				s.Index+1, e(s.RefName), analysis.Dur(pp.SpanSec), num(pp.PeriodSec, 1), num(60/pp.PeriodSec, 2), num(pp.AmpPPB, 4), num(pp.PromDB, 1), map[bool]string{true: "yes", false: "no"}[pp.Found]))
		}
	}
	if len(prow) > 0 {
		p(`<h2>Periodic components</h2><table><tr><th>Segment</th><th>Data</th><th>Strongest period s</th><th>cycles/min</th><th>Amplitude ppb</th><th>Above median dB</th><th>Significant</th></tr>%s</table>`, strings.Join(prow, ""))
	}

	if len(m.Events) > 0 {
		p(`<h2>Event log</h2><table><tr><th>UTC</th><th>Event</th><th>Detail</th></tr>`)
		ev := append([]store.Event(nil), m.Events...)
		sort.SliceStable(ev, func(i, j int) bool { return ev[i].Time.Before(ev[j].Time) })
		for _, x := range ev {
			p(`<tr><td>%s</td><td>%s</td><td>%s</td></tr>`, x.Time.UTC().Format("2006-01-02 15:04:05"), e(x.Kind), e(x.Msg))
		}
		p(`</table>`)
	}

	p(`<h2>Method</h2><div class="small">
<p>The radio is tuned in DIGU (no DIGU offset, RIT off) so that a carrier of known frequency <i>f</i><sub>c</sub> appears as an audio tone of about %.0f Hz. The tone is taken from a DAX audio stream, mixed to baseband, averaged over 10 ms and phase-tracked. Each %d s gate gives the average tone frequency <i>f</i><sub>a</sub> from the phase advance between consecutive gate ends (no dead time; used for the Allan family) and a least-squares frequency (lower noise; used for the live display and the offset chart).</p>
<p>The radio reads the carrier at <i>f</i><sub>r</sub> = dial + slice tuning error + <i>f</i><sub>a</sub>, where the slice tuning error is the radio's own report of its digital tuning offset (<code>slice get_error</code>). Every frequency the radio produces, including the DAX sample clock, scales with its reference, so the reference's fractional error is exactly <i>y</i> = <i>f</i><sub>c</sub>/<i>f</i><sub>r</sub> − 1, given in parts per billion.</p>
<p>Readings are excluded from the statistics while settling after a reference change, while a disciplined reference reports it is not locked, when DAX packets were lost, when the carrier-to-noise ratio in the 100 Hz tracking bandwidth is below 10 dB, or when the phase jumps. Stability is computed on an even time grid where a term is used only if every reading it spans is present: overlapping Allan (ADEV), modified Allan (MDEV, separates white from flicker phase noise), overlapping Hadamard (HDEV, insensitive to linear drift) and time deviation (TDEV). ADEV error bars are approximate (σ/√(N/m)). Drift is a least-squares slope; settling is when a 10-gate running mean last left a band (3σ of the final quarter, at least 0.5 ppb) around the final value. The spectrum is a Hann-windowed FFT of the longest gap-free stretch, detrended.</p>
<p>Measurement floor: at a carrier SNR of 30 dB the 1 s reading noise is a few millihertz, i.e. a few tenths of a ppb at 10 MHz; where MDEV falls as τ<sup>−1.5</sup> the stability shown is the measurement's, not the radio's.</p>
<p>Raw data: <code>samples.csv</code> (one row per gate, every field above plus GPS and temperatures) and <code>run.json</code> in the run folder.</p></div>`, m.ToneHz, m.GateSec)
	p(`</main></body></html>`)
	return b.String()
}

// CompareHTML renders a comparison of several runs.
func CompareHTML(rows []analysis.CompareRow, stability, offsets string, appVersion string) string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	p(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Reference Comparison</title><style>%s</style></head><body><main>`, css)
	p(`<h1>Frequency reference comparison</h1><p class="sub">%d rows · generated %s UTC · Flex Reference Lab %s</p>`, len(rows), time.Now().UTC().Format("2006-01-02 15:04"), e(appVersion))
	p(`<table><tr><th>#</th><th>Run</th><th>Radio</th><th>Date</th><th>Reference</th><th>Time</th><th>Mean ppb</th><th>U ppb</th><th>σ ppb</th><th>Drift ppb/h</th><th>ADEV 1 s</th><th>ADEV 10 s</th><th>ADEV 100 s</th><th>ADEV 1000 s</th><th>Settle s</th><th>Result</th></tr>`)
	for i, r := range rows {
		x := r.Ref
		p(`<tr><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td><b>%s</b></td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="%s">%s</td></tr>`,
			i+1, e(orDash(r.RunName)), e(r.Radio), r.Created.UTC().Format("2006-01-02"), e(x.Name), analysis.Dur(x.Hours*3600), sgn(x.MeanPPB, 3), num(x.UTotalPPB, 3),
			num(x.StdPPB, 3), sgn(x.DriftPPBh, 3), sci(x.ADEV1), sci(x.ADEV10), sci(x.ADEV100), sci(x.ADEV1000), num(x.SettleSec, 0), x.Pass, orDash(x.Pass))
	}
	p(`</table>%s%s</main></body></html>`, offsets, stability)
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "running"
	}
	return t.UTC().Format(time.DateTime)
}
