// Package plot draws simple, self-contained SVG charts: the same charts are
// shown in the app and embedded in the HTML report.
package plot

import (
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"
)

// Series is one set of points.
type Series struct {
	Name   string
	Color  string
	X, Y   []float64
	Lo, Hi []float64 // optional error bars (absolute y values)
	Kind   string    // line (default), points, step, bars
	Dash   bool
	Width  float64
}

// Band is a shaded x range (e.g. a settling period).
type Band struct {
	X0, X1 float64
	Color  string
	Label  string
}

// Chart is a 2-D chart.
type Chart struct {
	Title          string
	XLabel, YLabel string
	LogX, LogY     bool
	TimeX          bool // x is Unix seconds, labelled as UTC clock time
	W, H           int
	Series         []Series
	Bands          []Band
	HLines         []HLine
	Note           string
}

// HLine is a horizontal reference line (a spec limit, zero).
type HLine struct {
	Y     float64
	Color string
	Label string
}

const (
	padL, padR, padT, padB = 70.0, 16.0, 30.0, 46.0
)

func ok(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// SVG renders the chart.
func (c Chart) SVG() string {
	if c.W == 0 {
		c.W = 900
	}
	if c.H == 0 {
		c.H = 320
	}
	tx := func(v float64) float64 {
		if c.LogX {
			return math.Log10(v)
		}
		return v
	}
	ty := func(v float64) float64 {
		if c.LogY {
			return math.Log10(v)
		}
		return v
	}
	// Data range.
	x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	use := func(x, y float64) {
		if (c.LogX && x <= 0) || (c.LogY && y <= 0) || !ok(x) || !ok(y) {
			return
		}
		x, y = tx(x), ty(y)
		x0, x1, y0, y1 = math.Min(x0, x), math.Max(x1, x), math.Min(y0, y), math.Max(y1, y)
	}
	for _, s := range c.Series {
		for i := range s.X {
			if i < len(s.Y) {
				use(s.X[i], s.Y[i])
			}
			if i < len(s.Lo) {
				use(s.X[i], s.Lo[i])
			}
			if i < len(s.Hi) {
				use(s.X[i], s.Hi[i])
			}
		}
	}
	for _, h := range c.HLines {
		if ok(x0) && ok(h.Y) && !(c.LogY && h.Y <= 0) {
			y0, y1 = math.Min(y0, ty(h.Y)), math.Max(y1, ty(h.Y))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" class="chart" font-family="Segoe UI, Arial, sans-serif" font-size="12">`, c.W, c.H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, c.W, c.H)
	if c.Title != "" {
		fmt.Fprintf(&b, `<text x="%g" y="18" font-size="14" font-weight="600" fill="#1f2937">%s</text>`, padL, esc(c.Title))
	}
	if !ok(x0) || !ok(y0) {
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" fill="#6b7280">No data yet</text></svg>`, c.W/2, c.H/2)
		return b.String()
	}
	if x1 == x0 {
		x0, x1 = x0-1, x1+1
	}
	if y1 == y0 {
		d := math.Max(math.Abs(y0)*0.1, 1e-3)
		if c.LogY {
			d = 0.5
		}
		y0, y1 = y0-d, y1+d
	}
	// Pad the y range a little (linear) or to whole decades (log).
	if c.LogY {
		y0, y1 = math.Floor(y0), math.Ceil(y1)
		if y1 == y0 {
			y1++
		}
	} else {
		pad := (y1 - y0) * 0.06
		y0, y1 = y0-pad, y1+pad
	}
	if c.LogX {
		x0, x1 = math.Floor(x0), math.Ceil(x1)
		if x1 == x0 {
			x1++
		}
	}
	pw, ph := float64(c.W)-padL-padR, float64(c.H)-padT-padB
	px := func(v float64) float64 { return padL + (tx(v)-x0)/(x1-x0)*pw }
	py := func(v float64) float64 { return padT + (1-(ty(v)-y0)/(y1-y0))*ph }

	// Bands.
	for _, bd := range c.Bands {
		a, z := px(bd.X0), px(bd.X1)
		a, z = math.Max(a, padL), math.Min(z, padL+pw)
		if z > a {
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" opacity="0.15"><title>%s</title></rect>`,
				a, padT, z-a, ph, bd.Color, esc(bd.Label))
		}
	}
	// Grid and ticks.
	b.WriteString(`<g stroke="#e5e7eb" stroke-width="1">`)
	yt := ticks(y0, y1, c.LogY, 6)
	for _, v := range yt {
		y := padT + (1-(v-y0)/(y1-y0))*ph
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%.1f" y2="%.1f"/>`, padL, y, padL+pw, y)
	}
	xt := ticks(x0, x1, c.LogX, 8)
	if c.TimeX {
		xt = timeTicks(x0, x1, 8)
	}
	for _, v := range xt {
		x := padL + (v-x0)/(x1-x0)*pw
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%g" x2="%.1f" y2="%.1f"/>`, x, padT, x, padT+ph)
	}
	b.WriteString(`</g><g fill="#4b5563">`)
	for _, v := range yt {
		y := padT + (1-(v-y0)/(y1-y0))*ph
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`, padL-6, y, tickLabel(v, c.LogY, yt))
	}
	for _, v := range xt {
		x := padL + (v-x0)/(x1-x0)*pw
		lbl := tickLabel(v, c.LogX, xt)
		if c.TimeX {
			lbl = timeLabel(v, x1-x0)
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, padT+ph+16, lbl)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%d" text-anchor="middle">%s</text>`, padL+pw/2, c.H-6, esc(c.XLabel))
	fmt.Fprintf(&b, `<text transform="translate(14 %.1f) rotate(-90)" text-anchor="middle">%s</text>`, padT+ph/2, esc(c.YLabel))
	b.WriteString(`</g>`)
	fmt.Fprintf(&b, `<rect x="%g" y="%g" width="%.1f" height="%.1f" fill="none" stroke="#9ca3af"/>`, padL, padT, pw, ph)
	// Clip the plot area.
	id := fmt.Sprintf("c%x", uint32(len(c.Title)*7919+c.W*31+c.H+len(c.Series)*131))
	fmt.Fprintf(&b, `<clipPath id="%s"><rect x="%g" y="%g" width="%.1f" height="%.1f"/></clipPath><g clip-path="url(#%s)">`, id, padL, padT, pw, ph, id)
	for _, h := range c.HLines {
		y := py(h.Y)
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="6 4"/>`, padL, y, padL+pw, y, h.Color)
		if h.Label != "" {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" fill="%s" font-size="11">%s</text>`, padL+pw-4, y-4, h.Color, esc(h.Label))
		}
	}
	for _, s := range c.Series {
		drawSeries(&b, s, px, py, c, padT+ph)
	}
	b.WriteString(`</g>`)
	// Legend.
	lx := padL + 8
	names := map[string]bool{}
	for _, s := range c.Series {
		if s.Name == "" || names[s.Name] {
			continue
		}
		names[s.Name] = true
		w := 14 + 7*float64(len(s.Name))
		if lx+w > float64(c.W)-padR {
			break
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%g" width="10" height="10" fill="%s"/><text x="%.1f" y="%g" fill="#374151">%s</text>`,
			lx, padT+6, s.Color, lx+14, padT+15, esc(s.Name))
		lx += w + 12
	}
	if c.Note != "" {
		fmt.Fprintf(&b, `<text x="%.1f" y="18" text-anchor="end" fill="#6b7280" font-size="11">%s</text>`, float64(c.W)-padR, esc(c.Note))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func drawSeries(b *strings.Builder, s Series, px, py func(float64) float64, c Chart, base float64) {
	if s.Width == 0 {
		s.Width = 1.4
	}
	valid := func(i int) bool {
		return i < len(s.Y) && ok(s.X[i]) && ok(s.Y[i]) && !(c.LogX && s.X[i] <= 0) && !(c.LogY && s.Y[i] <= 0)
	}
	switch s.Kind {
	case "points":
		fmt.Fprintf(b, `<g fill="%s">`, s.Color)
		for i := range s.X {
			if valid(i) {
				fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="1.8"/>`, px(s.X[i]), py(s.Y[i]))
			}
		}
		b.WriteString(`</g>`)
	case "bars":
		fmt.Fprintf(b, `<g fill="%s" opacity="0.85">`, s.Color)
		for i := 0; i+1 < len(s.X); i++ {
			if i < len(s.Y) && ok(s.Y[i]) {
				x0, x1 := px(s.X[i]), px(s.X[i+1])
				y := py(s.Y[i])
				fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, x0+0.5, y, math.Max(0.5, x1-x0-1), math.Max(0, base-y))
			}
		}
		b.WriteString(`</g>`)
	default:
		dash := ""
		if s.Dash {
			dash = ` stroke-dasharray="5 3"`
		}
		var d strings.Builder
		pen := false
		for i := range s.X {
			if !valid(i) {
				pen = false
				continue
			}
			cmd := "L"
			if !pen {
				cmd = "M"
			}
			if s.Kind == "step" && pen {
				fmt.Fprintf(&d, "H%.1f", px(s.X[i]))
			}
			fmt.Fprintf(&d, "%s%.1f %.1f", cmd, px(s.X[i]), py(s.Y[i]))
			pen = true
		}
		fmt.Fprintf(b, `<path d="%s" fill="none" stroke="%s" stroke-width="%g"%s stroke-linejoin="round"/>`, d.String(), s.Color, s.Width, dash)
		if c.LogX && c.LogY { // stability plots: mark each tau
			fmt.Fprintf(b, `<g fill="%s">`, s.Color)
			for i := range s.X {
				if valid(i) {
					fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.6"><title>%s τ=%g s: %.3g</title></circle>`, px(s.X[i]), py(s.Y[i]), esc(s.Name), s.X[i], s.Y[i])
				}
			}
			b.WriteString(`</g>`)
		}
	}
	if len(s.Lo) > 0 && len(s.Hi) > 0 {
		fmt.Fprintf(b, `<g stroke="%s" stroke-width="1">`, s.Color)
		for i := range s.X {
			if i < len(s.Lo) && i < len(s.Hi) && ok(s.Lo[i]) && ok(s.Hi[i]) && !(c.LogY && s.Lo[i] <= 0) {
				x := px(s.X[i])
				fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, x, py(s.Lo[i]), x, py(s.Hi[i]))
			}
		}
		b.WriteString(`</g>`)
	}
}

// ticks returns tick positions in transformed units.
func ticks(lo, hi float64, log bool, n int) []float64 {
	if log {
		var out []float64
		for v := math.Ceil(lo); v <= hi+1e-9; v++ {
			out = append(out, v)
		}
		return out
	}
	step := niceStep((hi - lo) / float64(n))
	var out []float64
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		if math.Abs(v) < step*1e-9 {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

func niceStep(raw float64) float64 {
	if raw <= 0 || !ok(raw) {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*p >= raw {
			return m * p
		}
	}
	return 10 * p
}

func tickLabel(v float64, log bool, all []float64) string {
	if log {
		e := int(math.Round(v))
		if e >= -2 && e <= 4 {
			return trimFloat(math.Pow(10, float64(e)))
		}
		return fmt.Sprintf("1e%d", e)
	}
	step := 1.0
	if len(all) > 1 {
		step = all[1] - all[0]
	}
	dec := 0
	if step < 1 {
		dec = int(math.Ceil(-math.Log10(step) - 1e-9))
	}
	if math.Abs(v) >= 1e5 || (v != 0 && math.Abs(v) < 1e-4) {
		return fmt.Sprintf("%.2g", v)
	}
	return fmt.Sprintf("%.*f", dec, v)
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func timeTicks(lo, hi float64, n int) []float64 {
	span := hi - lo
	steps := []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400}
	step := steps[len(steps)-1]
	for _, s := range steps {
		if span/s <= float64(n) {
			step = s
			break
		}
	}
	var out []float64
	for v := math.Ceil(lo/step) * step; v <= hi; v += step {
		out = append(out, v)
	}
	return out
}

func timeLabel(v, span float64) string {
	t := time.Unix(int64(v), 0).UTC()
	switch {
	case span > 2*86400:
		return t.Format("01-02 15:04")
	case span < 600:
		return t.Format("15:04:05")
	}
	return t.Format("15:04")
}

func esc(s string) string { return html.EscapeString(s) }

// Decimate keeps at most n points, preserving each bucket's min and max so
// spikes stay visible.
func Decimate(x, y []float64, n int) ([]float64, []float64) {
	if len(x) <= n || n < 4 {
		return x, y
	}
	per := float64(len(x)) / float64(n/2)
	var ox, oy []float64
	for b := 0; b < n/2; b++ {
		i0, i1 := int(float64(b)*per), int(float64(b+1)*per)
		if i1 > len(x) {
			i1 = len(x)
		}
		lo, hi := -1, -1
		gap := false
		for i := i0; i < i1; i++ {
			if !ok(y[i]) {
				gap = true
				continue
			}
			if lo < 0 || y[i] < y[lo] {
				lo = i
			}
			if hi < 0 || y[i] > y[hi] {
				hi = i
			}
		}
		if lo < 0 {
			ox, oy = append(ox, x[i0]), append(oy, math.NaN())
			continue
		}
		idx := []int{lo, hi}
		sort.Ints(idx)
		for _, i := range idx {
			ox, oy = append(ox, x[i]), append(oy, y[i])
		}
		if gap { // keep a break in the line
			ox, oy = append(ox, x[i1-1]), append(oy, math.NaN())
		}
	}
	return ox, oy
}
