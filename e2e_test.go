package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/logx"
	"github.com/va3mw/FlexRefLab/internal/report"
)

// TestSimSweep runs a short TCXO → GPSDO sweep on the simulator in real time
// (about 80 s) and checks the analysis recovers the simulated offsets.
func TestSimSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time test")
	}
	home := t.TempDir()
	t.Setenv("FLEXREFLAB_HOME", home)
	lg := logx.New(filepath.Join(home, "logs"))
	defer lg.Close()
	a := NewApp(lg)
	s := defaultSettings()
	s.Simulator = true
	s.Plan.Refs = []string{"tcxo", "gpsdo"}
	s.Plan.SettleSec, s.Plan.DwellSec, s.Plan.Cycles = 30, 25, 1
	a.set = s
	a.sim = newSimRig(s.Stimulus.CarrierHz)
	a.sim.SetOscillator("gpsdo")
	a.sim.switched = time.Now().Add(-time.Hour)
	a.sim.lockAt = time.Now()

	id, err := a.StartRun(RunOptions{Name: "e2e", Sweep: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(200 * time.Second)
	for a.eng.running() && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if a.eng.running() {
		a.StopRun()
		t.Fatal("sweep did not finish")
	}
	run, res, err := a.analyzed(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d samples, %d segments", len(run.Samples), len(res.Segments))
	for _, f := range res.Findings {
		t.Logf("[%s] %s", f.Level, f.Msg)
	}
	got := map[string]analysis.RefSummary{}
	for _, r := range res.Refs {
		got[r.Ref] = r
	}
	tc, gp := got["TCXO"], got["GPSDO"]
	// Simulated TCXO ≈ 180 - 6*(T-38) ppb, GPSDO ≈ 0.02 ± 0.12 wobble.
	if tc.Used < 15 || math.IsNaN(tc.MeanPPB) || tc.MeanPPB < 100 || tc.MeanPPB > 260 {
		t.Errorf("TCXO summary %+v", tc)
	}
	if gp.Used < 15 || math.Abs(gp.MeanPPB) > 0.5 {
		t.Errorf("GPSDO summary mean %.3f used %d", gp.MeanPPB, gp.Used)
	}
	if run.Meta.Plan == nil || run.Meta.Plan.Restore != "gpsdo" {
		t.Errorf("plan %+v", run.Meta.Plan)
	}
	if a.sim.Snapshot().Ref.Setting != "gpsdo" {
		t.Errorf("reference not restored: %s", a.sim.Snapshot().Ref.Setting)
	}
	html := report.HTML(run, res, report.Build(run, res, 1000), version)
	if !strings.Contains(html, "Summary by reference") || !strings.Contains(html, "<svg") {
		t.Error("report incomplete")
	}
	os.WriteFile(filepath.Join(home, "report.html"), []byte(html), 0o644)
	if out := os.Getenv("FLEXREFLAB_E2E_OUT"); out != "" {
		os.WriteFile(out, []byte(html), 0o644)
	}
}
