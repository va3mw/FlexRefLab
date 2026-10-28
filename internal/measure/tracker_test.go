package measure

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// tone makes packets of a carrier at f Hz (optionally drifting) plus noise.
func feed(t *Tracker, secs float64, f, drift, noise float64, rate int, drop map[int]bool) []Gate {
	rng := rand.New(rand.NewSource(1))
	const per = 128
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var ph float64
	var out []Gate
	total := int(secs * float64(rate) / per)
	for p := 0; p < total; p++ {
		buf := make([]float32, per)
		for i := range buf {
			tt := float64(p*per+i) / float64(rate)
			ph += 2 * math.Pi * (f + drift*tt) / float64(rate)
			buf[i] = float32(0.3*math.Sin(ph) + noise*rng.NormFloat64())
		}
		if drop[p] {
			continue
		}
		lost := 0
		if drop[p-1] {
			lost = 1
		}
		now := start.Add(time.Duration(float64((p+1)*per) / float64(rate) * float64(time.Second)))
		out = append(out, t.Push(buf, rate, lost, now)...)
	}
	return out
}

func TestTrackerAccuracy(t *testing.T) {
	tr := NewTracker(1000, 300, 1)
	const f = 1003.4567
	gates := feed(tr, 30, f, 0, 0.01, 24000, nil)
	valid := 0
	for _, g := range gates {
		if !g.Valid {
			continue
		}
		valid++
		if math.Abs(g.FreqHz-f) > 0.002 || math.Abs(g.FitHz-f) > 0.001 {
			t.Errorf("gate %v: freq %.5f fit %.5f want %.5f", g.End, g.FreqHz, g.FitHz, f)
		}
		if g.SNRdB < 30 {
			t.Errorf("SNR %.1f dB too low for a clean tone", g.SNRdB)
		}
	}
	if valid < 25 {
		t.Fatalf("only %d valid gates of %d", valid, len(gates))
	}
}

func TestTrackerDriftAndLoss(t *testing.T) {
	tr := NewTracker(1500, 300, 1)
	const f, drift = 1490.0, 0.01 // Hz, Hz/s
	gates := feed(tr, 40, f, drift, 0.02, 24000, map[int]bool{2000: true})
	sawLost := false
	for _, g := range gates {
		if g.Lost > 0 {
			sawLost = true
			if g.Valid {
				t.Error("gate with lost packets marked valid")
			}
		}
	}
	if !sawLost {
		t.Error("lost packet not reported")
	}
	// Gate ends stay on whole seconds despite the lost packet.
	last := gates[len(gates)-1]
	if ns := last.End.Nanosecond(); ns > 1e6 && ns < 999e6 {
		t.Errorf("gate end drifted off the second: %v", last.End)
	}
	for _, g := range gates[len(gates)-5:] {
		secs := g.End.Sub(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)).Seconds()
		want := f + drift*(secs-0.5) // mean over the gate
		if g.Valid && math.Abs(g.FreqHz-want) > 0.005 {
			t.Errorf("at %.0f s: %.5f want %.5f", secs, g.FreqHz, want)
		}
	}
}

func TestNoCarrier(t *testing.T) {
	tr := NewTracker(1000, 300, 1)
	rng := rand.New(rand.NewSource(2))
	buf := make([]float32, 24000*3)
	for i := range buf {
		buf[i] = float32(0.1 * rng.NormFloat64())
	}
	gates := tr.Push(buf, 24000, 0, time.Now())
	if tr.Status().Acquired {
		t.Fatal("acquired on noise")
	}
	if len(gates) == 0 || gates[0].Valid {
		t.Fatal("expected a 'no carrier' gate")
	}
}
