package analysis

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/va3mw/FlexRefLab/internal/store"
)

func TestADEVWhiteFM(t *testing.T) {
	// White FM: ADEV(tau) = sigma / sqrt(tau).
	rng := rand.New(rand.NewSource(3))
	y := make([]float64, 20000)
	for i := range y {
		y[i] = 1e-9 * rng.NormFloat64()
	}
	y[500] = math.NaN() // a gap must not bias anything
	for _, p := range Deviations(y, 1) {
		want := 1e-9 / math.Sqrt(p.Tau)
		if p.Tau <= 100 && math.Abs(p.ADEV/want-1) > 0.1 {
			t.Errorf("tau %g: ADEV %.3g want %.3g", p.Tau, p.ADEV, want)
		}
		if p.Tau == 1 && math.Abs(p.MDEV/p.ADEV-1) > 0.05 {
			t.Errorf("MDEV(1) %.3g should equal ADEV(1) %.3g", p.MDEV, p.ADEV)
		}
	}
}

func TestDriftAndHadamard(t *testing.T) {
	// Linear drift raises ADEV ∝ tau but leaves HDEV at zero.
	y := make([]float64, 2000)
	for i := range y {
		y[i] = 1e-12 * float64(i)
	}
	ps := Deviations(y, 1)
	last := ps[len(ps)-1]
	if last.HDEV > 1e-15 {
		t.Errorf("HDEV %.3g should ignore drift", last.HDEV)
	}
	want := 1e-12 * last.Tau / math.Sqrt2
	if math.Abs(last.ADEV/want-1) > 0.01 {
		t.Errorf("ADEV %.3g want %.3g", last.ADEV, want)
	}
}

func TestAnalyzeSweep(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	t0 := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	const carrier = 10e6
	var ss []store.Sample
	add := func(step int, ref string, secs int, ppb, noise float64, settle int) {
		for i := 0; i < secs; i++ {
			v := ppb + noise*rng.NormFloat64()
			if i < settle {
				v += 50 * math.Exp(-float64(i)/5)
			}
			ph := "measure"
			if i < settle {
				ph = "settle"
			}
			ss = append(ss, store.Sample{T: t0.Add(time.Duration(len(ss)) * time.Second), Step: step, Ref: ref, Locked: true,
				Phase: ph, Valid: true, PPB: v, FitPPB: v, SNR: 40, Temps: map[string]float64{}})
		}
	}
	add(0, store.RefTCXO, 600, 120, 2, 30)
	add(1, store.RefGPSDO, 600, 0.3, 0.2, 30)
	add(2, store.RefTCXO, 600, 121, 2, 30)
	add(3, store.RefGPSDO, 600, 0.3, 0.2, 30)
	run := &store.Run{Meta: store.Meta{GateSec: 1, Stimulus: store.Stimulus{CarrierHz: carrier, UncertPPB: 0.01},
		Spec: map[string]float64{store.RefTCXO: 500, store.RefGPSDO: 1}}, Samples: ss}
	r := Analyze(run)
	if len(r.Segments) != 4 || len(r.Refs) != 2 {
		t.Fatalf("segments %d refs %d", len(r.Segments), len(r.Refs))
	}
	g, tc := r.Refs[0], r.Refs[1]
	if g.Ref != store.RefGPSDO || !g.Baseline || g.Pass != "PASS" {
		t.Errorf("GPSDO summary %+v", g)
	}
	if math.Abs(tc.MeanPPB-120.5) > 0.5 || math.Abs(tc.DeltaPPB-120.2) > 0.6 || tc.Segments != 2 {
		t.Errorf("TCXO summary mean %.2f delta %.2f segs %d", tc.MeanPPB, tc.DeltaPPB, tc.Segments)
	}
	if s := r.Segments[0].SettleSec; !(s > 5 && s < 40) {
		t.Errorf("settling %.1f s", s)
	}
}

func TestPeriodic(t *testing.T) {
	y := make([]float64, 4096)
	rng := rand.New(rand.NewSource(5))
	for i := range y {
		y[i] = 1e-9 * (0.4*math.Sin(2*math.Pi*float64(i)/17) + 0.05*rng.NormFloat64())
	}
	p := periodic(y, 1)
	if !p.Found || math.Abs(p.PeriodSec-17) > 0.3 || math.Abs(p.AmpPPB-0.4) > 0.1 {
		t.Errorf("periodic %+v", p)
	}
}
