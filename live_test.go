package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/va3mw/FlexRefLab/internal/flex"
	"github.com/va3mw/FlexRefLab/internal/logx"
	"github.com/va3mw/FlexRefLab/internal/report"
)

// TestLiveSingleRun measures a real radio for FLEXREFLAB_LIVE_SECS seconds
// (default 120). It only runs when FLEXREFLAB_LIVE_RADIO holds the radio's
// IP; it tunes slice A (restored afterwards) and never switches references.
// FLEXREFLAB_LIVE_CARRIER sets the carrier in Hz (default 10 MHz, WWV).
func TestLiveSingleRun(t *testing.T) {
	ip := os.Getenv("FLEXREFLAB_LIVE_RADIO")
	if ip == "" {
		t.Skip("set FLEXREFLAB_LIVE_RADIO to the radio's IP")
	}
	secs, _ := strconv.Atoi(os.Getenv("FLEXREFLAB_LIVE_SECS"))
	if secs == 0 {
		secs = 120
	}
	carrier, _ := strconv.ParseFloat(os.Getenv("FLEXREFLAB_LIVE_CARRIER"), 64)
	if carrier == 0 {
		carrier = 10e6
	}
	home := os.Getenv("FLEXREFLAB_HOME")
	if home == "" {
		home = t.TempDir()
		t.Setenv("FLEXREFLAB_HOME", home)
	}
	lg := logx.New(filepath.Join(home, "logs"))
	defer lg.Close()
	a := NewApp(lg)
	a.radio = flex.NewRadio(lg, nil)
	a.radio.Connect(ip, "", "", "", "")
	defer a.radio.Disconnect()
	for i := 0; i < 60; i++ {
		s := a.radio.Snapshot()
		if s.Connected && s.BoundHandle != 0 && s.Ref.Known && len(s.Meters) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	s := defaultSettings()
	s.Operator = "live test"
	s.Stimulus.Kind, s.Stimulus.Label, s.Stimulus.CarrierHz, s.Stimulus.UncertPPB = "wwv", "WWV off air", carrier, 10
	a.set = s

	// FLEXREFLAB_LIVE_SWEEP=tcxo,gpsdo runs a reference sweep instead, with
	// FLEXREFLAB_LIVE_SETTLE / _DWELL seconds per step.
	sweep := os.Getenv("FLEXREFLAB_LIVE_SWEEP")
	if sweep != "" {
		s.Plan.Refs = strings.Split(sweep, ",")
		s.Plan.Cycles = 1
		s.Plan.SettleSec, _ = strconv.Atoi(os.Getenv("FLEXREFLAB_LIVE_SETTLE"))
		s.Plan.DwellSec, _ = strconv.Atoi(os.Getenv("FLEXREFLAB_LIVE_DWELL"))
		a.set = s
	}

	before := a.radio.Snapshot()
	orig, _ := before.SliceByLetter("A")
	origOsc := before.Ref.Setting
	t.Logf("reference before: setting %s, state %s, locked %v", origOsc, before.Ref.State, before.Ref.Locked)
	if sweep != "" {
		// Whatever happens, leave the radio on the setting it had.
		defer func() {
			if now := a.radio.Snapshot().Ref.Setting; now != origOsc {
				t.Errorf("reference left on %q; putting %q back", now, origOsc)
				a.radio.SetOscillator(origOsc)
			}
		}()
	}
	name := "live " + before.Info["name"]
	if sweep != "" {
		name += " sweep " + sweep
	}
	id, err := a.StartRun(RunOptions{Name: name, Notes: "live test", Sweep: sweep != ""})
	if err != nil {
		t.Fatal(err)
	}
	if sweep == "" {
		time.Sleep(time.Duration(secs) * time.Second)
		a.StopRun()
	} else {
		deadline := time.Now().Add(45 * time.Minute)
		last := ""
		for a.eng.running() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			st, _ := a.eng.Live()
			snap := a.radio.Snapshot()
			line := fmt.Sprintf("step %d/%d %s %s | radio setting %s state %s locked %v", st.Step, st.Steps, st.StepRef, st.Phase, snap.Ref.Setting, snap.Ref.State, snap.Ref.Locked)
			if line != last {
				t.Logf("%s  %s", time.Now().Format("15:04:05"), line)
				last = line
			}
		}
		if a.eng.running() {
			a.StopRun()
			t.Error("sweep did not finish in time")
		}
		time.Sleep(3 * time.Second)
		if r := a.radio.Snapshot().Ref; r.Setting != origOsc {
			t.Errorf("reference setting %q after the sweep, want %q", r.Setting, origOsc)
		} else {
			t.Logf("reference restored: setting %s, state %s, locked %v", r.Setting, r.State, r.Locked)
		}
	}
	after := a.radio.Snapshot()
	if got, _ := after.SliceByLetter("A"); got.FreqHz != orig.FreqHz || got.Mode != orig.Mode || got.DAX != orig.DAX || got.DiguOffset != orig.DiguOffset {
		t.Errorf("slice not restored: was %+v now %+v", orig, got)
	}
	run, res, err := a.analyzed(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("run %s: %d samples in %s", id, len(run.Samples), run.Dir)
	for _, ev := range run.Meta.Events {
		t.Logf("event %s %-6s %s", ev.Time.Format("15:04:05"), ev.Kind, ev.Msg)
	}
	for _, f := range res.Findings {
		t.Logf("[%s] %s", f.Level, f.Msg)
	}
	for _, sg := range res.Segments {
		t.Logf("segment %d %s used %d invalid %d %v mean %.3f σ %.3f SNR %.1f slice err %.4f mHz noise %q",
			sg.Index+1, sg.RefName, sg.Used, sg.Invalid, sg.Why, sg.MeanPPB, sg.StdPPB, sg.SNRMean, sg.SliceErrMH, sg.Noise)
	}
	if len(res.Refs) == 0 || res.Refs[0].Used < secs/2 {
		t.Errorf("too few usable readings: %+v", res.Refs)
	}
	html := report.HTML(run, res, report.Build(run, res, 1000), version)
	os.WriteFile(filepath.Join(run.Dir, "report.html"), []byte(html), 0o644)
}
