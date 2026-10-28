// Package store keeps measurement runs on disk: one folder per run holding
// run.json (what was measured and how) and samples.csv (one row per gate,
// appended and flushed as it is measured, so a crash loses at most a second).
package store

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reference labels. GNSS is a GPSDO setting on a radio whose disciplined
// oscillator is GNSS (multi-constellation); the radio calls both "gpsdo".
const (
	RefTCXO     = "TCXO"
	RefExternal = "EXT10"
	RefGPSDO    = "GPSDO"
	RefGNSS     = "GNSS"
	RefUnknown  = "UNKNOWN"
)

// RefNames are the display names of the reference labels.
var RefNames = map[string]string{
	RefTCXO:     "TCXO",
	RefExternal: "External 10 MHz",
	RefGPSDO:    "GPSDO",
	RefGNSS:     "GNSS",
	RefUnknown:  "Unknown",
}

// Radio identifies the radio under test.
type Radio struct {
	Model    string            `json:"model"`
	Serial   string            `json:"serial"`
	Version  string            `json:"version"`
	Nickname string            `json:"nickname"`
	Callsign string            `json:"callsign"`
	IP       string            `json:"ip"`
	Info     map[string]string `json:"info,omitempty"`
	Options  string            `json:"options"` // references fitted, e.g. "TCXO, GPSDO, EXT"
}

// Stimulus is the carrier the radio is measured against.
type Stimulus struct {
	Kind      string  `json:"kind"`  // lab, wwv, chu, other
	Label     string  `json:"label"` // e.g. "Lab synthesizer locked to house GPSDO"
	CarrierHz float64 `json:"carrier_hz"`
	UncertPPB float64 `json:"uncert_ppb"` // 1-sigma uncertainty of the carrier itself
	Notes     string  `json:"notes"`
}

// Plan is an automatic reference sweep.
type Plan struct {
	Refs      []string `json:"refs"`       // oscillator settings in order: tcxo, gpsdo, external
	SettleSec int      `json:"settle_sec"` // discarded after each switch
	DwellSec  int      `json:"dwell_sec"`  // measured per step
	Cycles    int      `json:"cycles"`
	Restore   string   `json:"restore"` // setting restored at the end
}

// Event is something that happened during a run.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // start, stop, switch, lock, unlock, retune, note, error, gps
	Msg  string    `json:"msg"`
}

// Meta describes a run.
type Meta struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Operator   string             `json:"operator"`
	Notes      string             `json:"notes"`
	Created    time.Time          `json:"created"`
	Ended      time.Time          `json:"ended"`
	AppVersion string             `json:"app_version"`
	Source     string             `json:"source"` // dax, sim, wsjtx
	Radio      Radio              `json:"radio"`
	Slice      string             `json:"slice"`
	DAX        int                `json:"dax"`
	Stimulus   Stimulus           `json:"stimulus"`
	DialHz     int64              `json:"dial_hz"`
	ToneHz     float64            `json:"tone_hz"`
	GateSec    int                `json:"gate_sec"`
	Plan       *Plan              `json:"plan,omitempty"`
	Temps      []string           `json:"temps"` // temperature columns in samples.csv
	FreqErrPPB string             `json:"freq_error_ppb"`
	Spec       map[string]float64 `json:"spec_ppb"` // offset limit per reference label
	Events     []Event            `json:"events"`
}

// Sample is one gate.
type Sample struct {
	T          time.Time          `json:"t"`
	Step       int                `json:"step"` // sweep step (0 for a single run)
	Ref        string             `json:"ref"`  // reference label while measured
	Setting    string             `json:"setting"`
	State      string             `json:"state"`
	Locked     bool               `json:"locked"`
	Phase      string             `json:"phase"` // measure or settle
	Valid      bool               `json:"valid"`
	Why        string             `json:"why"`
	AudioHz    float64            `json:"audio_hz"`
	FitHz      float64            `json:"fit_hz"`
	SliceErrMH float64            `json:"slice_err_mhz"` // slice get_error, mHz
	ApparentHz float64            `json:"apparent_hz"`   // what the radio thinks the carrier is
	ErrHz      float64            `json:"err_hz"`        // apparent - true carrier
	PPB        float64            `json:"ppb"`           // reference error, + = reference fast
	FitPPB     float64            `json:"fit_ppb"`
	SNR        float64            `json:"snr_db"`
	Level      float64            `json:"level_db"`
	Lost       int                `json:"lost"`
	GPSTracked int                `json:"gps_tracked"`
	GPSVisible int                `json:"gps_visible"`
	GPSFix     string             `json:"gps_fix"`
	GPSStatus  string             `json:"gps_status"`
	GPSFreqErr string             `json:"gps_freq_error"`
	Temps      map[string]float64 `json:"temps"`
}

// PPBFrom returns the radio reference's fractional error, in ppb, from the
// frequency the radio reads for a carrier (dial + slice tuning error + audio,
// all in the radio's own Hz) and the carrier's true frequency. A reference
// running fast by e makes the radio read every frequency low by a factor
// 1/(1+e), so e = true/apparent - 1. This is exact: the DAX sample clock is
// scaled by the same factor as the LO.
func PPBFrom(apparentHz, trueHz float64) float64 {
	if apparentHz <= 0 || math.IsNaN(apparentHz) {
		return math.NaN()
	}
	return (trueHz/apparentHz - 1) * 1e9
}

var header = []string{"time_utc", "elapsed_s", "step", "ref", "osc_setting", "osc_state", "locked", "phase", "valid", "why",
	"audio_hz", "fit_hz", "slice_err_mhz", "apparent_hz", "err_hz", "ppb", "fit_ppb", "snr_db", "level_dbfs", "lost_pkts",
	"gps_tracked", "gps_visible", "gps_fix", "gps_status", "gps_freq_error"}

// Root is the folder runs are kept in.
type Root struct{ Dir string }

// Writer appends samples to a run.
type Writer struct {
	mu   sync.Mutex
	dir  string
	meta Meta
	f    *os.File
	w    *bufio.Writer
	cw   *csv.Writer
}

// Create starts a run folder.
func (r Root) Create(m Meta) (*Writer, error) {
	name := m.Created.UTC().Format("20060102-150405")
	if m.Name != "" {
		name += " " + SafeName(m.Name)
	}
	dir := filepath.Join(r.Dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m.ID = filepath.Base(dir)
	f, err := os.Create(filepath.Join(dir, "samples.csv"))
	if err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, meta: m, f: f, w: bufio.NewWriter(f)}
	w.cw = csv.NewWriter(w.w)
	h := append([]string(nil), header...)
	for _, t := range m.Temps {
		h = append(h, "temp_"+t)
	}
	w.cw.Write(h)
	w.flushLocked()
	return w, w.saveMetaLocked()
}

func (w *Writer) Dir() string { return w.dir }
func (w *Writer) ID() string  { return w.meta.ID }

// Meta returns a copy of the run's description.
func (w *Writer) Meta() Meta {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.meta
	m.Events = append([]Event(nil), w.meta.Events...)
	return m
}

// Append writes one sample.
func (w *Writer) Append(s Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return fmt.Errorf("run closed")
	}
	w.cw.Write(row(s, w.meta.Created, w.meta.Temps))
	return w.flushLocked()
}

// Event records an event in run.json.
func (w *Writer) Event(kind, format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.meta.Events = append(w.meta.Events, Event{Time: time.Now().UTC(), Kind: kind, Msg: fmt.Sprintf(format, args...)})
	w.saveMetaLocked()
}

// Update changes the description (e.g. a temperature column appeared late is
// not allowed; names and notes are).
func (w *Writer) Update(f func(*Meta)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(&w.meta)
	w.saveMetaLocked()
}

// Close ends the run.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	w.meta.Ended = time.Now().UTC()
	w.flushLocked()
	err := w.f.Close()
	w.f = nil
	if e := w.saveMetaLocked(); err == nil {
		err = e
	}
	return err
}

func (w *Writer) flushLocked() error {
	w.cw.Flush()
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.cw.Error()
}

func (w *Writer) saveMetaLocked() error {
	return SaveMeta(w.dir, w.meta)
}

// SaveMeta writes run.json atomically.
func SaveMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "run.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "run.json"))
}

func ff(v float64, prec int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

func row(s Sample, t0 time.Time, temps []string) []string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	r := []string{
		s.T.UTC().Format("2006-01-02T15:04:05.000Z"), ff(s.T.Sub(t0).Seconds(), 3), strconv.Itoa(s.Step), s.Ref, s.Setting, s.State,
		b(s.Locked), s.Phase, b(s.Valid), s.Why,
		ff(s.AudioHz, 6), ff(s.FitHz, 6), ff(s.SliceErrMH, 6), ff(s.ApparentHz, 6), ff(s.ErrHz, 6), ff(s.PPB, 4), ff(s.FitPPB, 4),
		ff(s.SNR, 1), ff(s.Level, 1), strconv.Itoa(s.Lost),
		strconv.Itoa(s.GPSTracked), strconv.Itoa(s.GPSVisible), s.GPSFix, s.GPSStatus, s.GPSFreqErr,
	}
	for _, t := range temps {
		v, ok := s.Temps[t]
		if !ok {
			v = math.NaN()
		}
		r = append(r, ff(v, 2))
	}
	return r
}

// Run is a run loaded from disk.
type Run struct {
	Dir     string   `json:"dir"`
	Meta    Meta     `json:"meta"`
	Samples []Sample `json:"-"`
}

// List returns the runs' descriptions, newest first.
func (r Root) List() []Meta {
	ents, _ := os.ReadDir(r.Dir)
	var out []Meta
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		m, err := LoadMeta(filepath.Join(r.Dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// LoadMeta reads run.json.
func LoadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	m.ID = filepath.Base(dir)
	return m, err
}

// Load reads a run by ID.
func (r Root) Load(id string) (*Run, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, fmt.Errorf("bad run id %q", id)
	}
	dir := filepath.Join(r.Dir, id)
	m, err := LoadMeta(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "samples.csv"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ss, err := ReadSamples(f, m.Temps)
	return &Run{Dir: dir, Meta: m, Samples: ss}, err
}

// ReadSamples parses samples.csv. A torn last line (from a crash) is skipped.
func ReadSamples(rd io.Reader, temps []string) ([]Sample, error) {
	cr := csv.NewReader(rd)
	cr.FieldsPerRecord = -1
	hdr, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range hdr {
		col[h] = i
	}
	var out []Sample
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < len(header) {
			continue
		}
		get := func(k string) string {
			if i, ok := col[k]; ok && i < len(rec) {
				return rec[i]
			}
			return ""
		}
		num := func(k string) float64 {
			v, err := strconv.ParseFloat(get(k), 64)
			if err != nil {
				return math.NaN()
			}
			return v
		}
		atoi := func(k string) int { v, _ := strconv.Atoi(get(k)); return v }
		t, err := time.Parse("2006-01-02T15:04:05.000Z", get("time_utc"))
		if err != nil {
			continue
		}
		s := Sample{T: t, Step: atoi("step"), Ref: get("ref"), Setting: get("osc_setting"), State: get("osc_state"),
			Locked: get("locked") == "1", Phase: get("phase"), Valid: get("valid") == "1", Why: get("why"),
			AudioHz: num("audio_hz"), FitHz: num("fit_hz"), SliceErrMH: num("slice_err_mhz"), ApparentHz: num("apparent_hz"),
			ErrHz: num("err_hz"), PPB: num("ppb"), FitPPB: num("fit_ppb"), SNR: num("snr_db"), Level: num("level_dbfs"),
			Lost: atoi("lost_pkts"), GPSTracked: atoi("gps_tracked"), GPSVisible: atoi("gps_visible"),
			GPSFix: get("gps_fix"), GPSStatus: get("gps_status"), GPSFreqErr: get("gps_freq_error"),
			Temps: map[string]float64{}}
		for _, tn := range temps {
			if v := num("temp_" + tn); !math.IsNaN(v) {
				s.Temps[tn] = v
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Delete removes a run folder.
func (r Root) Delete(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("bad run id %q", id)
	}
	dir := filepath.Join(r.Dir, id)
	if _, err := os.Stat(filepath.Join(dir, "run.json")); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// SafeName strips characters Windows does not allow in folder names.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
