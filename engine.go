package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/flex"
	"github.com/va3mw/FlexRefLab/internal/logx"
	"github.com/va3mw/FlexRefLab/internal/measure"
	"github.com/va3mw/FlexRefLab/internal/store"
)

// A measurement: the slice is tuned so the carrier is an audio tone, DAX
// audio feeds the tracker, and each gate becomes a sample with the radio's
// telemetry. With a plan, the engine also steps the radio through its
// references (settle, then measure) and puts the original setting back.

const liveKeep = 3600 // samples kept for the live view

// RunOptions start a measurement.
type RunOptions struct {
	Name  string `json:"name"`
	Notes string `json:"notes"`
	Sweep bool   `json:"sweep"` // step through Settings.Plan
}

// step is one entry of a sweep.
type step struct {
	Setting string // oscillator setting to select
}

// LiveStatus is what the Measure tab shows.
type LiveStatus struct {
	Running  bool           `json:"running"`
	RunID    string         `json:"run_id"`
	Name     string         `json:"name"`
	Started  time.Time      `json:"started"`
	Phase    string         `json:"phase"` // measure, settle, waiting for lock
	Step     int            `json:"step"`
	Steps    int            `json:"steps"`
	StepRef  string         `json:"step_ref"`
	StepLeft float64        `json:"step_left"` // seconds left in this phase
	Ref      string         `json:"ref"`
	Last     *store.Sample  `json:"last"`
	Mean60   float64        `json:"mean60"` // ppb over the last 60 usable gates
	Std60    float64        `json:"std60"`
	MeanAll  float64        `json:"mean_all"` // this reference, this step
	Count    int            `json:"count"`
	Invalid  int            `json:"invalid"`
	Tracker  measure.Status `json:"tracker"`
	SliceErr float64        `json:"slice_err_mhz"`
	DialHz   int64          `json:"dial_hz"`
	Carrier  float64        `json:"carrier_hz"`
	Message  string         `json:"message"`
	Error    string         `json:"error"`
}

type engine struct {
	log *logx.Source
	app *App

	mu        sync.Mutex
	rig       rig
	w         *store.Writer
	tr        *measure.Tracker
	opts      Settings
	sliceIdx  int
	origSlice flex.Slice // as it was before the run, put back at the end
	slice     string
	dial      int64
	sliceErr  float64
	gates     chan measure.Gate
	stop      chan struct{}
	done      chan struct{}
	live      []store.Sample
	status    LiveStatus

	// sweep
	steps    []step
	stepIdx  int
	phase    string
	phaseAt  time.Time
	lockedAt time.Time
	restore  string
	lastRef  string
	lastLock bool
	lastTune time.Time
}

func newEngine(a *App) *engine { return &engine{app: a, log: a.log.Src("measure")} }

func (e *engine) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w != nil
}

// refLabel names the reference the radio is using.
func refLabel(r flex.Reference, gpsdoAs string) string {
	if !r.Known {
		return store.RefUnknown
	}
	switch r.State {
	case "tcxo":
		return store.RefTCXO
	case "external":
		return store.RefExternal
	case "gnss":
		return store.RefGNSS
	case "gpsdo":
		switch gpsdoAs {
		case store.RefGNSS:
			return store.RefGNSS
		case store.RefGPSDO:
			return store.RefGPSDO
		}
		if r.GNSS && !r.GPSDO {
			return store.RefGNSS
		}
		return store.RefGPSDO
	}
	return store.RefUnknown
}

func fitted(r flex.Reference) string {
	var f []string
	if r.TCXO {
		f = append(f, "TCXO")
	}
	if r.GPSDO {
		f = append(f, "GPSDO")
	}
	if r.GNSS {
		f = append(f, "GNSS")
	}
	if r.External {
		f = append(f, "external 10 MHz input")
	}
	return strings.Join(f, ", ")
}

// Start begins a run.
func (e *engine) Start(rg rig, s Settings, o RunOptions, source string) (string, error) {
	e.mu.Lock()
	if e.w != nil {
		e.mu.Unlock()
		return "", fmt.Errorf("a measurement is already running")
	}
	e.mu.Unlock()

	snap := rg.Snapshot()
	if !snap.Connected {
		return "", fmt.Errorf("no radio connected")
	}
	sl, ok := snap.SliceByLetter(s.Slice)
	if !ok {
		return "", fmt.Errorf("slice %s isn't open on the bound station; open it in SmartSDR or choose another", s.Slice)
	}
	c := s.Stimulus.CarrierHz
	if c < 100e3 || c > 1e9 {
		return "", fmt.Errorf("set the stimulus carrier frequency first")
	}
	if s.ToneHz < 300 || s.ToneHz > 2700 {
		return "", fmt.Errorf("the audio tone must be between 300 and 2700 Hz")
	}
	if s.DAX < 1 || s.DAX > 8 {
		return "", fmt.Errorf("choose DAX channel 1 to 8")
	}
	var steps []step
	if o.Sweep {
		for cyc := 0; cyc < max(1, s.Plan.Cycles); cyc++ {
			for _, r := range s.Plan.Refs {
				steps = append(steps, step{Setting: r})
			}
		}
		if len(steps) == 0 {
			return "", fmt.Errorf("choose at least one reference for the sweep")
		}
		if !snap.Ref.Known {
			return "", fmt.Errorf("the radio hasn't reported its reference oscillator yet")
		}
		for _, r := range s.Plan.Refs {
			if !refPresent(snap.Ref, r) {
				return "", fmt.Errorf("the radio reports no %s reference", oscName(r))
			}
		}
	}
	// Whole-hertz dial, so the tone sits near the chosen audio frequency.
	dial := int64(math.Round(c - s.ToneHz))
	if err := rg.TuneDigital(sl.Index, dial); err != nil {
		return "", fmt.Errorf("tuning slice %s: %w", s.Slice, err)
	}
	rx, _, err := rg.SliceError(sl.Index)
	if err != nil {
		e.log.Warn("slice get_error: %v (assuming 0)", err)
		rx = 0
	}
	var temps []string
	for _, m := range snap.Meters {
		if m.IsTemp() {
			temps = append(temps, m.Name)
		}
	}
	plan := (*store.Plan)(nil)
	if o.Sweep {
		p := s.Plan
		p.Restore = snap.Ref.Setting
		plan = &p
	}
	meta := store.Meta{
		Name: o.Name, Operator: s.Operator, Notes: o.Notes, Created: time.Now().UTC(), AppVersion: version, Source: source,
		Radio: store.Radio{Model: snap.Model, Serial: snap.Serial, Version: snap.Version, Nickname: snap.Nickname, Callsign: snap.Callsign,
			IP: snap.IP, Info: snap.Info, Options: fitted(snap.Ref)},
		Slice: s.Slice, DAX: s.DAX, Stimulus: s.Stimulus, DialHz: dial, ToneHz: c - float64(dial), GateSec: s.GateSec,
		Plan: plan, Temps: temps, FreqErrPPB: snap.FreqErrPPB, Spec: s.Spec,
	}
	w, err := store.Root{Dir: e.app.runsDir()}.Create(meta)
	if err != nil {
		return "", fmt.Errorf("creating the run folder: %w", err)
	}
	w.Event("start", "%s; slice %s at %.6f MHz DIGU, tone %.1f Hz, DAX %d; reference %s (setting %s, %s); slice tuning error %+.4f mHz",
		map[bool]string{true: "reference sweep", false: "single run"}[o.Sweep], s.Slice, float64(dial)/1e6, c-float64(dial), s.DAX,
		snap.Ref.State, snap.Ref.Setting, map[bool]string{true: "locked", false: "NOT locked"}[snap.Ref.Locked], rx)

	tr := measure.NewTracker(c-float64(dial), 300, s.GateSec)
	gates := make(chan measure.Gate, 256)
	var tmu sync.Mutex
	handler := func(samples []float32, rate, lost int) {
		tmu.Lock()
		out := tr.Push(samples, rate, lost, time.Now())
		tmu.Unlock()
		for _, g := range out {
			select {
			case gates <- g:
			default: // the writer is stuck; drop rather than block the socket
			}
		}
	}
	if err := rg.StartAudio(sl.Index, s.DAX, handler); err != nil {
		w.Event("error", "DAX audio: %v", err)
		w.Close()
		return "", fmt.Errorf("starting DAX audio: %w", err)
	}

	e.mu.Lock()
	e.rig, e.w, e.tr, e.opts = rg, w, tr, s
	e.sliceIdx, e.slice, e.dial, e.sliceErr = sl.Index, s.Slice, dial, rx
	e.origSlice = sl
	e.gates, e.stop, e.done = gates, make(chan struct{}), make(chan struct{})
	e.live = nil
	e.steps, e.stepIdx, e.restore = steps, -1, snap.Ref.Setting
	e.phase, e.phaseAt = "measure", time.Now()
	e.lastRef, e.lastLock = refLabel(snap.Ref, s.GPSDOAs), snap.Ref.Locked
	e.lastTune = time.Now()
	e.status = LiveStatus{Running: true, RunID: w.ID(), Name: o.Name, Started: meta.Created, Steps: len(steps),
		DialHz: dial, Carrier: c, SliceErr: rx, Phase: "measure"}
	if len(steps) > 0 {
		e.nextStepLocked()
	}
	stop, done := e.stop, e.done
	e.mu.Unlock()
	e.log.Info("run %s started", w.ID())
	go e.loop(stop, done, &tmu)
	return w.ID(), nil
}

func refPresent(r flex.Reference, setting string) bool {
	switch setting {
	case "tcxo":
		return r.TCXO
	case "gpsdo":
		return r.GPSDO || r.GNSS
	case "external":
		return r.External
	}
	return false
}

func oscName(setting string) string {
	return map[string]string{"tcxo": "TCXO", "gpsdo": "GPSDO/GNSS", "external": "external 10 MHz", "auto": "auto"}[setting]
}

// Stop ends the run, restoring the reference setting after a sweep.
func (e *engine) Stop(why string) {
	e.mu.Lock()
	if e.w == nil {
		e.mu.Unlock()
		return
	}
	stop, done := e.stop, e.done
	e.stop = nil
	e.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return
	}
	e.rig.StopAudio()
	o := e.origSlice
	if err := e.rig.RestoreSlice(o); err != nil {
		e.w.Event("error", "restoring slice %s: %v", e.slice, err)
	} else {
		e.w.Event("retune", "slice %s restored to %.6f MHz %s, DAX %d", e.slice, float64(o.FreqHz)/1e6, o.Mode, o.DAX)
	}
	if len(e.steps) > 0 && e.restore != "" {
		if err := e.rig.SetOscillator(e.restore); err != nil {
			e.w.Event("error", "restoring the reference setting %s: %v", e.restore, err)
		} else {
			e.w.Event("switch", "reference setting restored to %s", e.restore)
		}
	}
	e.w.Event("stop", "%s", why)
	if err := e.w.Close(); err != nil {
		e.log.Error("closing run: %v", err)
	}
	e.log.Info("run %s stopped: %s", e.w.ID(), why)
	e.w = nil
	e.status.Running = false
	e.status.Message = why
}

// nextStepLocked moves the sweep on, or ends it.
func (e *engine) nextStepLocked() bool {
	e.stepIdx++
	if e.stepIdx >= len(e.steps) {
		return false
	}
	st := e.steps[e.stepIdx]
	e.phase, e.phaseAt, e.lockedAt = "switching", time.Now(), time.Time{}
	if err := e.rig.SetOscillator(st.Setting); err != nil {
		// The radio refused: waiting for a lock that can't come would stall
		// the sweep, so skip the step.
		e.w.Event("error", "step %d: selecting %s: %v; step skipped", e.stepIdx+1, oscName(st.Setting), err)
		return e.nextStepLocked()
	} else {
		e.w.Event("switch", "step %d of %d: reference set to %s", e.stepIdx+1, len(e.steps), oscName(st.Setting))
	}
	e.status.Step, e.status.StepRef = e.stepIdx+1, oscName(st.Setting)
	return true
}

const lockTimeout = 20 * time.Minute

// advanceLocked runs the sweep state machine once a second.
func (e *engine) advanceLocked(snap flex.Snapshot) (finished bool) {
	if len(e.steps) == 0 || e.stepIdx >= len(e.steps) {
		return false
	}
	st := e.steps[e.stepIdx]
	now := time.Now()
	p := e.opts.Plan
	onIt := snap.Ref.Setting == st.Setting && snap.Ref.State == st.Setting || (st.Setting == "gpsdo" && snap.Ref.State == "gnss")
	locked := onIt && (snap.Ref.Locked || st.Setting == "tcxo")
	switch e.phase {
	case "switching":
		if locked {
			took := now.Sub(e.phaseAt).Seconds()
			e.phase, e.phaseAt, e.lockedAt = "settle", now, now
			e.w.Event("lock", "step %d: %s in use and locked after %.0f s", e.stepIdx+1, oscName(st.Setting), took)
		} else if now.Sub(e.phaseAt) > lockTimeout {
			e.w.Event("error", "step %d: %s did not lock within %v; skipped", e.stepIdx+1, oscName(st.Setting), lockTimeout)
			return !e.nextStepLocked()
		}
		e.status.StepLeft = math.NaN()
	case "settle":
		if !locked {
			e.phase, e.phaseAt = "switching", now
			break
		}
		left := float64(p.SettleSec) - now.Sub(e.phaseAt).Seconds()
		e.status.StepLeft = left
		if left <= 0 {
			e.phase, e.phaseAt = "measure", now
			e.w.Event("note", "step %d: settled; measuring %s for %s", e.stepIdx+1, oscName(st.Setting), analysis.Dur(float64(p.DwellSec)))
		}
	case "measure":
		left := float64(p.DwellSec) - now.Sub(e.phaseAt).Seconds()
		e.status.StepLeft = left
		if left <= 0 {
			return !e.nextStepLocked()
		}
	}
	return false
}

func (e *engine) loop(stop, done chan struct{}, tmu *sync.Mutex) {
	defer close(done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case g := <-e.gates:
			e.onGate(g, tmu)
		case <-tick.C:
			if e.onTick() {
				go e.Stop("reference sweep finished")
				// Keep draining until Stop closes stop.
			}
		}
	}
}

// onTick watches the radio between gates. It returns true when a sweep ends.
func (e *engine) onTick() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return false
	}
	snap := e.rig.Snapshot()
	if !snap.Connected {
		e.status.Error = "radio connection lost"
		return false
	}
	ref := refLabel(snap.Ref, e.opts.GPSDOAs)
	if ref != e.lastRef {
		e.w.Event("switch", "radio now on %s (setting %s)", RefNameOr(ref), snap.Ref.Setting)
		e.lastRef = ref
	}
	if snap.Ref.Locked != e.lastLock {
		k := map[bool]string{true: "lock", false: "unlock"}[snap.Ref.Locked]
		e.w.Event(k, "%s %s", RefNameOr(ref), map[bool]string{true: "locked", false: "lost lock"}[snap.Ref.Locked])
		e.lastLock = snap.Ref.Locked
	}
	if sl, ok := snap.SliceByLetter(e.slice); !ok || sl.FreqHz != e.dial || sl.Mode != "DIGU" || sl.RITOn || sl.DiguOffset != 0 {
		if time.Since(e.lastTune) > 5*time.Second {
			e.w.Event("retune", "slice %s was changed (%d Hz %s); tuning it back", e.slice, sl.FreqHz, sl.Mode)
			e.lastTune = time.Now()
			go e.retune()
		}
	}
	if !e.rig.AudioOpen() {
		e.status.Error = "DAX stream closed by the radio"
	} else if e.status.Error == "DAX stream closed by the radio" || e.status.Error == "radio connection lost" {
		e.status.Error = ""
	}
	e.status.Ref = ref
	if e.tr != nil {
		e.status.Tracker = e.tr.Status()
	}
	if len(e.steps) > 0 {
		if e.advanceLocked(snap) {
			return true
		}
		e.status.Phase = e.phase
	}
	return false
}

func (e *engine) retune() {
	e.mu.Lock()
	rg, idx, dial, w := e.rig, e.sliceIdx, e.dial, e.w
	e.mu.Unlock()
	if rg == nil || w == nil {
		return
	}
	if err := rg.TuneDigital(idx, dial); err != nil {
		w.Event("error", "retune failed: %v", err)
		return
	}
	if rx, _, err := rg.SliceError(idx); err == nil {
		e.mu.Lock()
		e.sliceErr = rx
		e.status.SliceErr = rx
		e.mu.Unlock()
	}
}

func (e *engine) onGate(g measure.Gate, tmu *sync.Mutex) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return
	}
	snap := e.rig.Snapshot()
	c := e.opts.Stimulus.CarrierHz
	s := store.Sample{T: g.End, Step: max(0, e.stepIdx), Ref: refLabel(snap.Ref, e.opts.GPSDOAs), Setting: snap.Ref.Setting,
		State: snap.Ref.State, Locked: snap.Ref.Locked, Phase: "measure", Valid: g.Valid, Why: g.Why,
		AudioHz: g.FreqHz, FitHz: g.FitHz, SliceErrMH: e.sliceErr, SNR: g.SNRdB, Level: g.LevelDB, Lost: g.Lost,
		GPSTracked: snap.GPS.Tracked, GPSVisible: snap.GPS.Visible, GPSFix: snap.GPS.Fix, GPSStatus: snap.GPS.Status,
		GPSFreqErr: snap.GPS.FreqError, Temps: snap.Temps()}
	if len(e.steps) > 0 && e.phase != "measure" {
		s.Phase = "settle"
	}
	if sl, ok := snap.SliceByLetter(e.slice); ok && (sl.FreqHz != e.dial || sl.Mode != "DIGU") {
		s.Valid, s.Why = false, "slice retuned"
	}
	base := float64(e.dial) + e.sliceErr/1000
	s.ApparentHz = base + g.FreqHz
	s.ErrHz = s.ApparentHz - c
	s.PPB = store.PPBFrom(s.ApparentHz, c)
	s.FitPPB = store.PPBFrom(base+g.FitHz, c)
	if err := e.w.Append(s); err != nil {
		e.status.Error = "writing samples: " + err.Error()
	}
	e.live = append(e.live, s)
	if len(e.live) > liveKeep {
		e.live = e.live[len(e.live)-liveKeep:]
	}
	e.status.Count++
	if !s.Valid {
		e.status.Invalid++
	}
	last := s
	e.status.Last = &last
	// Rolling numbers for the display.
	var r60, all []float64
	for i := len(e.live) - 1; i >= 0; i-- {
		x := e.live[i]
		if x.Step != s.Step || x.Ref != s.Ref {
			break
		}
		if analysis.Usable(x) {
			all = append(all, x.FitPPB)
			if len(r60) < 60 {
				r60 = append(r60, x.FitPPB)
			}
		}
	}
	e.status.Mean60, e.status.Std60, e.status.MeanAll = analysis.Mean(r60), analysis.Std(r60), analysis.Mean(all)
	tmu.Lock()
	e.status.Tracker = e.tr.Status()
	tmu.Unlock()
	e.app.emit("sample")
}

// Live returns the status and recent samples.
func (e *engine) Live() (LiveStatus, []store.Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, append([]store.Sample(nil), e.live...)
}

// Note adds an operator note to the running run.
func (e *engine) Note(text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return fmt.Errorf("no run in progress")
	}
	e.w.Event("note", "%s", text)
	return nil
}

// RefNameOr is the display name of a reference label.
func RefNameOr(ref string) string {
	if n, ok := store.RefNames[ref]; ok {
		return n
	}
	return ref
}
