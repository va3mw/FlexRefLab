package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/flex"
	"github.com/va3mw/FlexRefLab/internal/fmtdata"
	"github.com/va3mw/FlexRefLab/internal/logx"
	"github.com/va3mw/FlexRefLab/internal/report"
	"github.com/va3mw/FlexRefLab/internal/safejson"
	"github.com/va3mw/FlexRefLab/internal/store"
)

// Settings are remembered between sessions.
type Settings struct {
	Operator  string             `json:"operator"`
	Simulator bool               `json:"simulator"`
	RadioIP   string             `json:"radio_ip"`
	Serial    string             `json:"serial"`
	Station   string             `json:"station"`
	Slice     string             `json:"slice"`
	DAX       int                `json:"dax"`
	ToneHz    float64            `json:"tone_hz"`
	GateSec   int                `json:"gate_sec"`
	GPSDOAs   string             `json:"gpsdo_as"` // "", GPSDO or GNSS: how to label the radio's "gpsdo" state
	Stimulus  store.Stimulus     `json:"stimulus"`
	Spec      map[string]float64 `json:"spec"`
	Plan      store.Plan         `json:"plan"`
	ChartW    int                `json:"chart_w"`
}

func defaultSettings() Settings {
	return Settings{
		Slice: "A", DAX: 1, ToneHz: 1000, GateSec: 1,
		Stimulus: store.Stimulus{Kind: "lab", Label: "Signal generator locked to a GPSDO", CarrierHz: 10e6, UncertPPB: 0.01},
		// Placeholder limits: set them to the engineering specification.
		Spec: map[string]float64{store.RefTCXO: 500, store.RefExternal: 10, store.RefGPSDO: 1, store.RefGNSS: 1},
		Plan: store.Plan{Refs: []string{"tcxo", "gpsdo"}, SettleSec: 120, DwellSec: 900, Cycles: 2},
	}
}

// App is bound to the UI.
type App struct {
	ctx    context.Context
	log    *logx.Logger
	src    *logx.Source
	mu     sync.Mutex
	set    Settings
	radio  *flex.Radio
	disc   *flex.Discovery
	sim    *simRig
	eng    *engine
	notice string

	cacheMu sync.Mutex
	cache   map[string]cachedRun
}

type cachedRun struct {
	mod time.Time
	run *store.Run
	res analysis.Result
}

func NewApp(l *logx.Logger) *App {
	a := &App{log: l, src: l.Src("app"), cache: map[string]cachedRun{}}
	a.eng = newEngine(a)
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.set = loadSettings()
	a.radio = flex.NewRadio(a.log, func() { a.emit("changed") })
	a.disc = flex.NewDiscovery(a.log, func() { a.emit("changed") })
	a.disc.Start()
	if a.set.Simulator {
		a.sim = newSimRig(a.set.Stimulus.CarrierHz)
	} else if a.set.RadioIP != "" {
		a.radio.Connect(a.set.RadioIP, "", a.set.Serial, "", a.set.Station)
	}
	if root, fallback := dataRoot(); fallback {
		a.notice = fmt.Sprintf("Windows Security's Controlled folder access stops Flex Reference Lab writing to Documents, so runs are kept in %s.", root)
	}
	a.src.Info("runs are kept in %s", a.runsDir())
}

func (a *App) shutdown(context.Context) {
	a.eng.Stop("application closed")
	a.radio.Disconnect()
	a.disc.Stop()
	a.log.Close()
}

func (a *App) emit(name string) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, name)
	}
}

func (a *App) rig() rig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sim != nil {
		return a.sim
	}
	return a.radio
}

func (a *App) runsDir() string {
	r, _ := dataRoot()
	return filepath.Join(r, "Runs")
}

// ---------------------------------------------------------------------------
// State

// State is everything the UI shows outside the run views.
type State struct {
	Version  string           `json:"version"`
	Settings Settings         `json:"settings"`
	Radios   []flex.RadioInfo `json:"radios"`
	Radio    flex.Snapshot    `json:"radio"`
	RefLabel string           `json:"ref_label"`
	Live     LiveStatus       `json:"live"`
	Notice   string           `json:"notice"`
	RunsDir  string           `json:"runs_dir"`
}

func (a *App) GetState() any {
	a.mu.Lock()
	s := a.set
	a.mu.Unlock()
	snap := a.rig().Snapshot()
	st, _ := a.eng.Live()
	return safejson.Tree(State{Version: version, Settings: s, Radios: a.disc.List(), Radio: snap,
		RefLabel: refLabel(snap.Ref, s.GPSDOAs), Live: st, Notice: a.notice, RunsDir: a.runsDir()})
}

// SaveSettings stores the settings (not while a run uses them).
func (a *App) SaveSettings(s Settings) error {
	if a.eng.running() {
		return fmt.Errorf("stop the measurement before changing settings")
	}
	if s.Spec == nil {
		s.Spec = map[string]float64{}
	}
	if s.GateSec < 1 {
		s.GateSec = 1
	}
	a.mu.Lock()
	simChanged := s.Simulator != a.set.Simulator
	a.set = s
	saveSettings(s)
	if simChanged {
		if s.Simulator {
			a.sim = newSimRig(s.Stimulus.CarrierHz)
		} else {
			a.sim = nil
		}
	} else if a.sim != nil {
		a.sim.mu.Lock()
		a.sim.carrier = s.Stimulus.CarrierHz
		a.sim.mu.Unlock()
	}
	a.mu.Unlock()
	if simChanged && s.Simulator {
		a.radio.Disconnect()
	} else if simChanged && s.RadioIP != "" {
		a.radio.Connect(s.RadioIP, "", s.Serial, "", s.Station)
	}
	a.emit("changed")
	return nil
}

// Connect connects to a discovered radio (by serial) or an IP address.
func (a *App) Connect(serialOrIP string) error {
	if a.eng.running() {
		return fmt.Errorf("stop the measurement first")
	}
	ip, serial, model, ver := strings.TrimSpace(serialOrIP), "", "", ""
	if info, ok := a.disc.Find(serialOrIP); ok {
		ip, serial, model, ver = info.IP, info.Serial, info.Model, info.Version
	}
	if ip == "" {
		return fmt.Errorf("choose a radio or type its IP address")
	}
	a.mu.Lock()
	a.set.RadioIP, a.set.Serial, a.set.Simulator = ip, serial, false
	a.sim = nil
	station := a.set.Station
	saveSettings(a.set)
	a.mu.Unlock()
	a.radio.Connect(ip, model, serial, ver, station)
	return nil
}

func (a *App) Disconnect() {
	a.eng.Stop("radio disconnected")
	a.radio.Disconnect()
	a.mu.Lock()
	a.set.RadioIP = ""
	saveSettings(a.set)
	a.mu.Unlock()
	a.emit("changed")
}

func (a *App) SetStation(name string) {
	a.mu.Lock()
	a.set.Station = name
	saveSettings(a.set)
	a.mu.Unlock()
	a.radio.SetStation(name)
}

// SetOscillator selects the radio's reference by hand (Setup tab).
func (a *App) SetOscillator(setting string) error {
	if a.eng.running() {
		return fmt.Errorf("stop the measurement first (a sweep switches references itself)")
	}
	return a.rig().SetOscillator(setting)
}

// ---------------------------------------------------------------------------
// Measuring

func (a *App) StartRun(o RunOptions) (string, error) {
	a.mu.Lock()
	s := a.set
	src := "dax"
	if a.sim != nil {
		src = "sim"
	}
	a.mu.Unlock()
	id, err := a.eng.Start(a.rig(), s, o, src)
	a.emit("changed")
	return id, err
}

func (a *App) StopRun() {
	a.eng.Stop("stopped by the operator")
	a.emit("changed")
}

func (a *App) AddNote(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return a.eng.Note(strings.TrimSpace(text))
}

// LiveView is the Measure tab's data.
type LiveView struct {
	Status LiveStatus     `json:"status"`
	Chart  string         `json:"chart"`
	Recent []store.Sample `json:"recent"`
}

// GetLive returns the live status, the last minutes' chart and recent rows.
func (a *App) GetLive(minutes int, width int) any {
	st, ss := a.eng.Live()
	if minutes > 0 && len(ss) > 0 {
		cut := ss[len(ss)-1].T.Add(-time.Duration(minutes) * time.Minute)
		i := sort.Search(len(ss), func(i int) bool { return !ss[i].T.Before(cut) })
		ss = ss[i:]
	}
	v := LiveView{Status: st, Chart: report.Live(ss, max(600, width))}
	if n := len(ss); n > 0 {
		v.Recent = ss[max(0, n-12):]
	}
	return safejson.Tree(v)
}

// ---------------------------------------------------------------------------
// Runs and analysis

func (a *App) root() store.Root { return store.Root{Dir: a.runsDir()} }

// RunItem is a row in the run list.
type RunItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Ended    time.Time `json:"ended"`
	Radio    string    `json:"radio"`
	Carrier  float64   `json:"carrier"`
	Source   string    `json:"source"`
	Sweep    bool      `json:"sweep"`
	Running  bool      `json:"running"`
	Operator string    `json:"operator"`
}

func (a *App) ListRuns() any {
	st, _ := a.eng.Live()
	var out []RunItem
	for _, m := range a.root().List() {
		out = append(out, RunItem{ID: m.ID, Name: m.Name, Created: m.Created, Ended: m.Ended,
			Radio: strings.TrimSpace(m.Radio.Model + " " + m.Radio.Nickname), Carrier: m.Stimulus.CarrierHz,
			Source: m.Source, Sweep: m.Plan != nil, Running: st.Running && st.RunID == m.ID, Operator: m.Operator})
	}
	return safejson.Tree(out)
}

// analyzed loads and analyses a run, cached until its files change.
func (a *App) analyzed(id string) (*store.Run, analysis.Result, error) {
	dir := filepath.Join(a.runsDir(), id)
	var mod time.Time
	for _, f := range []string{"samples.csv", "run.json"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil && fi.ModTime().After(mod) {
			mod = fi.ModTime()
		}
	}
	a.cacheMu.Lock()
	c, ok := a.cache[id]
	a.cacheMu.Unlock()
	if ok && c.mod.Equal(mod) {
		return c.run, c.res, nil
	}
	run, err := a.root().Load(id)
	if err != nil {
		return nil, analysis.Result{}, err
	}
	res := analysis.Analyze(run)
	a.cacheMu.Lock()
	a.cache[id] = cachedRun{mod: mod, run: run, res: res}
	a.cacheMu.Unlock()
	return run, res, nil
}

// RunView is an analysed run with its charts.
type RunView struct {
	Result analysis.Result   `json:"result"`
	Charts map[string]string `json:"charts"`
	Order  []string          `json:"order"`
	Titles map[string]string `json:"titles"`
	Dir    string            `json:"dir"`
}

func (a *App) GetRun(id string, width int) (any, error) {
	run, res, err := a.analyzed(id)
	if err != nil {
		return nil, err
	}
	ch := report.Build(run, res, max(600, width))
	return safejson.Tree(RunView{Result: res, Charts: ch, Order: report.ChartOrder, Titles: report.ChartTitles, Dir: run.Dir}), nil
}

// EditRun renames a run or changes its notes.
func (a *App) EditRun(id, name, notes string) error {
	run, err := a.root().Load(id)
	if err != nil {
		return err
	}
	st, _ := a.eng.Live()
	if st.Running && st.RunID == id {
		return fmt.Errorf("edit the run after it stops")
	}
	run.Meta.Name, run.Meta.Notes = name, notes
	return store.SaveMeta(run.Dir, run.Meta)
}

// DeleteRun removes a run folder (the UI asks first).
func (a *App) DeleteRun(id string) error {
	st, _ := a.eng.Live()
	if st.Running && st.RunID == id {
		return fmt.Errorf("stop the run before deleting it")
	}
	a.cacheMu.Lock()
	delete(a.cache, id)
	a.cacheMu.Unlock()
	return a.root().Delete(id)
}

func (a *App) OpenRunFolder(id string) {
	dir := a.runsDir()
	if id != "" {
		dir = filepath.Join(dir, id)
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = exec.Command("explorer", dir).Start()
}

// ExportReport writes report.html into the run folder and opens it.
func (a *App) ExportReport(id string) (string, error) {
	run, res, err := a.analyzed(id)
	if err != nil {
		return "", err
	}
	html := report.HTML(run, res, report.Build(run, res, 1000), version)
	p := filepath.Join(run.Dir, "report.html")
	if err := os.WriteFile(p, []byte(html), 0o644); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(safejson.Tree(res), "", " ")
	_ = os.WriteFile(filepath.Join(run.Dir, "analysis.json"), b, 0o644)
	a.src.Info("report written to %s", p)
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", p).Start()
	return p, nil
}

// CompareView compares several runs.
type CompareView struct {
	Rows      []analysis.CompareRow `json:"rows"`
	Stability string                `json:"stability"`
	Offsets   string                `json:"offsets"`
}

func (a *App) compare(ids []string, width int) (CompareView, error) {
	var rs []analysis.Result
	for _, id := range ids {
		_, res, err := a.analyzed(id)
		if err != nil {
			return CompareView{}, fmt.Errorf("%s: %w", id, err)
		}
		rs = append(rs, res)
	}
	rows := analysis.CompareRuns(rs)
	st, of := report.Compare(rows, max(600, width))
	return CompareView{Rows: rows, Stability: st, Offsets: of}, nil
}

func (a *App) GetCompare(ids []string, width int) (any, error) {
	v, err := a.compare(ids, width)
	return safejson.Tree(v), err
}

// ExportCompare saves a comparison report and opens it.
func (a *App) ExportCompare(ids []string) (string, error) {
	v, err := a.compare(ids, 1000)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(a.runsDir()), "Comparisons")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "compare-"+time.Now().UTC().Format("20060102-150405")+".html")
	if err := os.WriteFile(p, []byte(report.CompareHTML(v.Rows, v.Stability, v.Offsets, version)), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", p).Start()
	return p, nil
}

// ---------------------------------------------------------------------------
// WSJT-X FreqCal import

// ImportRequest describes an fmt.all file to bring in as a run.
type ImportRequest struct {
	Path      string  `json:"path"`
	Name      string  `json:"name"`
	DialHz    float64 `json:"dial_hz"`    // WSJT-X dial frequency (USB)
	CarrierHz float64 `json:"carrier_hz"` // true frequency of the carrier measured
	Ref       string  `json:"ref"`        // reference label the radio was on
	KHz       int     `json:"khz"`        // only lines of this station (0: all)
}

func (a *App) BrowseFmtAll() (string, error) {
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{Title: "Choose a WSJT-X fmt.all file",
		Filters: []wruntime.FileFilter{{DisplayName: "FreqCal files (fmt*.all)", Pattern: "*.all"}, {DisplayName: "All files", Pattern: "*.*"}}})
}

// ImportFmtAll turns WSJT-X FreqCal readings into a run (no telemetry).
// WSJT-X's own frequency calibration must have been zero.
func (a *App) ImportFmtAll(r ImportRequest) (string, error) {
	f, err := fmtdata.ParseFile(r.Path)
	if err != nil {
		return "", err
	}
	if r.CarrierHz <= 0 || r.DialHz <= 0 {
		return "", fmt.Errorf("enter the dial and carrier frequencies")
	}
	var lines []fmtdata.Line
	for _, l := range f.Lines {
		if r.KHz == 0 || l.KHz == r.KHz {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return "", fmt.Errorf("no readings for that station in %s", filepath.Base(r.Path))
	}
	// Times have no date: take the date the file was last written, a day
	// earlier if the first reading would then fall after that, and count
	// forward across midnight.
	fi, err := os.Stat(r.Path)
	if err != nil {
		return "", err
	}
	day := fi.ModTime().UTC().Truncate(24 * time.Hour)
	first := day.Add(time.Duration(lines[0].Sec) * time.Second)
	if first.After(fi.ModTime().Add(time.Minute)) {
		first = first.Add(-24 * time.Hour)
	}
	var gaps []float64
	for i := 1; i < len(lines); i++ {
		gaps = append(gaps, float64(fmtdata.SecDiff(lines[i].Sec, lines[i-1].Sec)))
	}
	gate := int(math.Max(1, math.Round(analysis.Quantile(gaps, 0.5))))
	ref := r.Ref
	if _, ok := store.RefNames[ref]; !ok {
		ref = store.RefUnknown
	}
	meta := store.Meta{Name: r.Name, Created: first, AppVersion: version, Source: "wsjtx",
		Radio:    store.Radio{Model: "(from WSJT-X FreqCal)"},
		Stimulus: store.Stimulus{Kind: "other", Label: fmt.Sprintf("Imported from %s", filepath.Base(r.Path)), CarrierHz: r.CarrierHz},
		DialHz:   int64(math.Round(r.DialHz)), ToneHz: r.CarrierHz - r.DialHz, GateSec: gate, Spec: a.set.Spec}
	a.mu.Lock()
	meta.Operator, meta.Spec = a.set.Operator, a.set.Spec
	a.mu.Unlock()
	w, err := a.root().Create(meta)
	if err != nil {
		return "", err
	}
	t := meta.Created
	prev := lines[0].Sec
	for _, l := range lines {
		t = t.Add(time.Duration(fmtdata.SecDiff(l.Sec, prev)) * time.Second)
		prev = l.Sec
		app := r.DialHz + l.FMeas
		ppb := store.PPBFrom(app, r.CarrierHz)
		w.Append(store.Sample{T: t, Ref: ref, Locked: true, Phase: "measure", Valid: l.SNR >= 10, Why: map[bool]string{true: "", false: "low SNR"}[l.SNR >= 10],
			AudioHz: l.FMeas, FitHz: l.FMeas, ApparentHz: app, ErrHz: app - r.CarrierHz, PPB: ppb, FitPPB: ppb, SNR: l.SNR, Level: l.Level})
	}
	w.Event("note", "imported %d readings from %s (WSJT-X FreqCal); median spacing %d s", len(lines), r.Path, gate)
	return w.ID(), w.Close()
}

// ---------------------------------------------------------------------------
// Log

func (a *App) GetLog(since uint64) []logx.Entry { return a.log.Since(since) }

func (a *App) SetTraffic(on bool) { a.log.SetTraffic(on) }

// ---------------------------------------------------------------------------
// Files

// Home is FLEXREFLAB_HOME: when set (development and testing) settings, logs
// and runs live under it instead of %APPDATA% and Documents.
func Home() string { return os.Getenv("FLEXREFLAB_HOME") }

func appDataDir() string {
	if h := Home(); h != "" {
		return filepath.Join(h, "AppData")
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "FlexRefLab")
}

// dataRoot is Documents\Flex Reference Lab, or %USERPROFILE%\Flex Reference
// Lab when Controlled folder access blocks Documents.
func dataRoot() (string, bool) {
	if h := Home(); h != "" {
		return filepath.Join(h, "Data"), false
	}
	home, _ := os.UserHomeDir()
	docs := filepath.Join(home, "Documents", "Flex Reference Lab")
	if writable(docs) {
		return docs, false
	}
	return filepath.Join(home, "Flex Reference Lab"), true
}

func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	n := f.Name()
	f.Close()
	os.Remove(n)
	return true
}

func settingsPath() string { return filepath.Join(appDataDir(), "settings.json") }

func loadSettings() Settings {
	s := defaultSettings()
	if b, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Spec == nil {
		s.Spec = defaultSettings().Spec
	}
	return s
}

func saveSettings(s Settings) {
	_ = os.MkdirAll(appDataDir(), 0o755)
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(settingsPath(), b, 0o644)
}
