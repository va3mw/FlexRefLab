package main

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/va3mw/FlexRefLab/internal/flex"
)

// rig is what a measurement needs from a radio. *flex.Radio is the real one;
// simRig stands in for it so the whole chain (audio, tracker, sweep,
// analysis) can be exercised without a radio or a carrier.
type rig interface {
	Snapshot() flex.Snapshot
	TuneDigital(slice int, dialHz int64) error
	SliceError(slice int) (float64, float64, error)
	StartAudio(slice, ch int, fn flex.AudioHandler) error
	StopAudio()
	AudioOpen() bool
	SetOscillator(setting string) error
	RestoreSlice(orig flex.Slice) error
}

// simRig models a radio with a TCXO, a GPSDO and an external 10 MHz input,
// receiving a perfect carrier. Each reference has its own offset and noise,
// the TCXO follows a slowly changing temperature, the GPSDO wobbles a little
// and every switch has a lock delay and a settling transient.
type simRig struct {
	mu       sync.Mutex
	carrier  float64
	dial     int64
	setting  string
	state    string
	lockAt   time.Time
	switched time.Time
	from     float64 // offset (ppb) at the switch, for the transient
	rw       float64 // TCXO random walk, ppb
	stop     chan struct{}
	start    time.Time
	rng      *rand.Rand
	gnss     bool
}

func newSimRig(carrier float64) *simRig {
	now := time.Now()
	return &simRig{carrier: carrier, setting: "auto", state: "gpsdo", lockAt: now, switched: now.Add(-time.Hour),
		start: now, rng: rand.New(rand.NewSource(now.UnixNano()))}
}

func (s *simRig) temp(t time.Time) float64 {
	h := t.Sub(s.start).Hours()
	return 38 + 4*math.Sin(2*math.Pi*h/0.75) + 1.5*(1-math.Exp(-h*3))
}

// ppb returns the reference error now. s.mu must be held.
func (s *simRig) ppbLocked(t time.Time) float64 {
	var base float64
	el := t.Sub(s.start).Seconds()
	switch s.state {
	case "tcxo":
		base = 180 - 6*(s.temp(t)-38) + s.rw
	case "external":
		base = 0.8 + 0.05*math.Sin(el/400)
	default:
		base = 0.02 + 0.12*math.Sin(2*math.Pi*el/17)
	}
	if !s.lockedLocked(t) && s.state != "tcxo" {
		base += 25 * math.Sin(el/9)
	}
	// Settling after a switch: exponential from the old offset.
	if d := t.Sub(s.switched).Seconds(); d >= 0 && d < 300 {
		base += (s.from - base) * math.Exp(-d/6)
	}
	return base
}

func (s *simRig) lockedLocked(t time.Time) bool { return !t.Before(s.lockAt) }

func (s *simRig) Snapshot() flex.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	locked := s.lockedLocked(now) || s.state == "tcxo"
	return flex.Snapshot{
		Connected: true, IP: "simulator", Model: "SIMULATOR", Serial: "SIM-0001", Version: "sim", Nickname: "Simulated radio",
		Stations: []flex.Station{{Handle: 1, ClientID: "sim", Program: "Simulator", Name: "Sim"}}, BoundStation: "Sim", BoundHandle: 1,
		Slices:     []flex.Slice{{Index: 0, Letter: "A", InUse: true, FreqHz: s.dial, Mode: "DIGU", ClientHandle: 1, DAX: 1}},
		Ref:        flex.Reference{Known: true, Setting: s.setting, State: s.state, Locked: locked, TCXO: true, GPSDO: !s.gnss, GNSS: s.gnss, External: true},
		GPS:        flex.GPS{Known: true, Status: "Fine Lock", Fix: "3D", Tracked: 8 + int(now.Unix()/97%3), Visible: 12, FreqError: "0"},
		FreqErrPPB: "0",
		Meters: []flex.Meter{
			{ID: 1, Source: "RAD", Name: "PATEMP", Unit: "degC", Value: s.temp(now) + 3, Fresh: true},
			{ID: 2, Source: "RAD", Name: "RADTEMP", Unit: "degC", Value: s.temp(now), Fresh: true},
		},
	}
}

func (s *simRig) TuneDigital(_ int, dialHz int64) error {
	s.mu.Lock()
	s.dial = dialHz
	s.mu.Unlock()
	return nil
}

func (s *simRig) RestoreSlice(orig flex.Slice) error { return s.TuneDigital(orig.Index, orig.FreqHz) }

func (s *simRig) SliceError(int) (float64, float64, error) { return 0.37, -0.12, nil }

func (s *simRig) SetOscillator(setting string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	state := setting
	if setting == "auto" {
		state = "gpsdo"
	}
	if state != s.state {
		s.from = s.ppbLocked(now)
		s.switched = now
		s.state = state
		s.lockAt = now.Add(8 * time.Second)
	}
	s.setting = setting
	return nil
}

func (s *simRig) AudioOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop != nil
}

// StartAudio generates 24 kHz audio of the carrier as the radio would hear it.
func (s *simRig) StartAudio(_, _ int, fn flex.AudioHandler) error {
	s.StopAudio()
	s.mu.Lock()
	stop := make(chan struct{})
	s.stop = stop
	s.mu.Unlock()
	go func() {
		const rate, per = 24000, 1200
		var ph float64
		dropped := 0
		next := time.Now()
		for {
			next = next.Add(per * time.Second / rate)
			select {
			case <-stop:
				return
			case <-time.After(time.Until(next)):
			}
			now := time.Now()
			s.mu.Lock()
			if s.state == "tcxo" {
				s.rw += 0.02 * s.rng.NormFloat64()
			}
			e := s.ppbLocked(now)*1e-9 + 0.05e-9*s.rng.NormFloat64()
			// The radio reads the carrier at carrier/(1+e); the slice's own
			// tuning error (0.37 mHz) shifts the tone the other way.
			tone := s.carrier/(1+e) - float64(s.dial) - 0.37e-3
			buf := make([]float32, per)
			for i := range buf {
				ph += 2 * math.Pi * tone / rate
				buf[i] = float32(0.2*math.Sin(ph) + 0.002*s.rng.NormFloat64())
			}
			ph = math.Mod(ph, 2*math.Pi)
			drop := s.rng.Intn(4000) == 0 // now and then a packet goes missing
			s.mu.Unlock()
			if drop {
				dropped++
				continue
			}
			fn(buf, rate, dropped)
			dropped = 0
		}
	}()
	return nil
}

func (s *simRig) StopAudio() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}

func (s *simRig) String() string { return fmt.Sprintf("simulator at %.3f MHz", s.carrier/1e6) }
