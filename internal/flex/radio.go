package flex

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FlexRefLab/internal/logx"
)

// Slice mirrors the radio's slice status.
type Slice struct {
	Index        int    `json:"index"`
	Letter       string `json:"letter"`
	InUse        bool   `json:"in_use"`
	FreqHz       int64  `json:"freq_hz"`
	Mode         string `json:"mode"`
	ClientHandle uint32 `json:"client_handle"`
	RITOn        bool   `json:"rit_on"`
	RITFreq      int    `json:"rit_freq"`
	DiguOffset   int    `json:"digu_offset"`
	DAX          int    `json:"dax"`
}

// Station is a GUI client (SmartSDR, Maestro, ...) connected to the radio.
type Station struct {
	Handle   uint32 `json:"handle"`
	ClientID string `json:"client_id"`
	Program  string `json:"program"`
	Name     string `json:"name"`
}

// Reference is the radio's 10 MHz frequency reference, from the "radio
// oscillator" and "gps" status.
type Reference struct {
	Known    bool   `json:"known"`   // an oscillator status has been received
	Setting  string `json:"setting"` // what the user chose: auto, tcxo, gpsdo, external
	State    string `json:"state"`   // the reference in use (auto falls back by itself)
	Locked   bool   `json:"locked"`
	TCXO     bool   `json:"tcxo_present"`
	GPSDO    bool   `json:"gpsdo_present"`
	External bool   `json:"ext_present"`
	GNSS     bool   `json:"gnss_present"`
}

// GPS is the radio's GPS receiver status, as reported. FreqError is kept as
// the radio's text: its units and sign are not documented.
type GPS struct {
	Known     bool      `json:"known"`
	Status    string    `json:"status"`
	Fix       string    `json:"fix"`
	Tracked   int       `json:"tracked"`
	Visible   int       `json:"visible"`
	FreqError string    `json:"freq_error"`
	Time      string    `json:"time"`
	TimeAt    time.Time `json:"time_at"`
	Lat       string    `json:"lat"`
	Lon       string    `json:"lon"`
	Grid      string    `json:"grid"`
	Altitude  string    `json:"altitude"`
}

// Meter is one of the radio's meters (only the radio-level ones are kept).
type Meter struct {
	ID     int     `json:"id"`
	Source string  `json:"source"`
	Name   string  `json:"name"`
	Desc   string  `json:"desc"`
	Unit   string  `json:"unit"`
	Value  float64 `json:"value"`
	Fresh  bool    `json:"fresh"` // a value has arrived
}

// IsTemp reports whether the meter measures a temperature.
func (m Meter) IsTemp() bool {
	u := strings.ToLower(m.Unit)
	return u == "degc" || u == "degf"
}

// Snapshot is a consistent copy of the radio state.
type Snapshot struct {
	Connected    bool              `json:"connected"`
	IP           string            `json:"ip"`
	Model        string            `json:"model"`
	Serial       string            `json:"serial"`
	Version      string            `json:"version"`
	Nickname     string            `json:"nickname"`
	Callsign     string            `json:"callsign"`
	Info         map[string]string `json:"info"`
	Stations     []Station         `json:"stations"`
	BoundStation string            `json:"bound_station"`
	BoundHandle  uint32            `json:"bound_handle"`
	Slices       []Slice           `json:"slices"`
	Ref          Reference         `json:"ref"`
	GPS          GPS               `json:"gps"`
	FreqErrPPB   string            `json:"freq_error_ppb"` // the radio's own ppb correction, as reported
	CalFreq      string            `json:"cal_freq"`       // MHz the radio's auto-calibration listens on
	Meters       []Meter           `json:"meters"`
}

// SliceByLetter finds an in-use slice owned by the bound station.
func (s *Snapshot) SliceByLetter(letter string) (Slice, bool) {
	if s.BoundHandle == 0 {
		return Slice{}, false
	}
	for _, sl := range s.Slices {
		if sl.InUse && sl.ClientHandle == s.BoundHandle && strings.EqualFold(sl.Letter, letter) {
			return sl, true
		}
	}
	return Slice{}, false
}

// Temps returns the temperature meters, in degrees C.
func (s *Snapshot) Temps() map[string]float64 {
	out := map[string]float64{}
	for _, m := range s.Meters {
		if !m.IsTemp() || !m.Fresh {
			continue
		}
		v := m.Value
		if strings.EqualFold(m.Unit, "degF") {
			v = (v - 32) * 5 / 9
		}
		out[m.Name] = v
	}
	return out
}

type replyFn func(code uint32, msg string)

// AudioHandler receives the left channel of a DAX audio stream. lost is the
// number of packets missed before this one (from the VITA packet count).
type AudioHandler func(samples []float32, rate int, lost int)

// Radio is a non-GUI client of the SmartSDR TCP API (port 4992). It reads
// slice, reference, GPS and meter state, tunes slices, receives DAX audio and,
// when asked, selects the radio's reference oscillator. It never transmits.
type Radio struct {
	log      *logx.Source
	onUpdate func()
	notifyCh chan struct{}

	// OnReference (optional) is told when the reference in use, its setting
	// or its lock state changes.
	OnReference func(Reference)

	mu          sync.Mutex
	conn        net.Conn
	connected   bool
	ip, model   string
	serial      string
	version     string
	nickname    string
	callsign    string
	info        map[string]string
	seq         uint32
	pending     map[uint32]replyFn
	myHandle    uint32
	slices      map[int]*Slice
	stations    map[uint32]*Station
	wantStation string
	boundHandle uint32
	boundName   string
	ref         Reference
	gps         GPS
	freqErrPPB  string
	calFreq     string
	meters      map[int]*Meter
	lastRx      time.Time
	stop        chan struct{}

	udp       *net.UDPConn
	udpPort   int
	audioID   uint32 // our dax_rx stream
	audioFn   AudioHandler
	audioLast int // last VITA packet count, -1 before the first
}

func NewRadio(l *logx.Logger, onUpdate func()) *Radio {
	r := &Radio{log: l.Src("radio"), onUpdate: onUpdate, notifyCh: make(chan struct{}, 1)}
	go r.notifyLoop()
	return r
}

func (r *Radio) notifyLoop() {
	for range r.notifyCh {
		if r.onUpdate != nil {
			r.onUpdate()
		}
	}
}

// Connect starts a connection loop that reconnects until Disconnect is called.
// serial and version come from discovery and may be empty.
func (r *Radio) Connect(ip, model, serial, version, station string) {
	r.Disconnect()
	r.mu.Lock()
	r.ip, r.model, r.serial, r.version, r.wantStation = ip, model, serial, version, station
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()
	go r.loop(ip, stop)
}

func (r *Radio) Disconnect() {
	r.mu.Lock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.conn != nil {
		r.conn.Close()
	}
	r.mu.Unlock()
}

// SetStation selects which GUI station's slices we use, and binds to it.
func (r *Radio) SetStation(name string) {
	r.mu.Lock()
	r.wantStation = name
	r.boundHandle, r.boundName = 0, ""
	r.mu.Unlock()
	r.tryBind()
	r.notify()
}

func (r *Radio) loop(ip string, stop chan struct{}) {
	for {
		err := r.session(ip, stop)
		select {
		case <-stop:
			r.log.Info("disconnected from radio")
			return
		default:
		}
		r.log.Warn("radio connection lost: %v; retrying in 3 s", err)
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// openUDP binds the local VITA socket. Like FlexLib it starts at 4991 and
// walks up if the port is taken (SmartSDR on the same PC already uses 4991).
func openUDP() (*net.UDPConn, int, error) {
	var lastErr error
	for port := 4991; port <= 4991+40; port++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err == nil {
			c.SetReadBuffer(1 << 21)
			return c, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no free UDP port for radio streams: %w", lastErr)
}

func (r *Radio) session(ip string, stop chan struct{}) error {
	r.log.Info("connecting to radio at %s:4992", ip)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "4992"), 5*time.Second)
	if err != nil {
		return err
	}
	udp, udpPort, uerr := openUDP()
	if uerr != nil {
		r.log.Warn("%v: DAX audio and meters will not work", uerr)
	} else {
		go r.udpLoop(udp, net.ParseIP(ip))
	}
	r.mu.Lock()
	r.conn = conn
	r.udp, r.udpPort = udp, udpPort
	r.pending = map[uint32]replyFn{}
	r.slices = map[int]*Slice{}
	r.stations = map[uint32]*Station{}
	r.meters = map[int]*Meter{}
	r.info = map[string]string{}
	r.boundHandle, r.boundName, r.myHandle = 0, "", 0
	r.ref, r.gps = Reference{}, GPS{}
	r.audioID = 0
	r.lastRx = time.Now()
	r.mu.Unlock()

	done := make(chan struct{})
	defer func() {
		close(done)
		conn.Close()
		if udp != nil {
			udp.Close()
		}
		r.mu.Lock()
		r.connected = false
		r.conn, r.udp = nil, nil
		r.audioID = 0
		r.boundHandle, r.boundName = 0, ""
		hadRef, cb := r.ref.Known, r.OnReference
		r.ref, r.gps = Reference{}, GPS{}
		r.mu.Unlock()
		if hadRef && cb != nil {
			cb(Reference{}) // no longer known
		}
		r.notify()
	}()

	// Watchdog: ping every 2 s, drop the link if nothing heard for 10 s.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				conn.Close()
				return
			case <-t.C:
				r.mu.Lock()
				silent := time.Since(r.lastRx)
				r.mu.Unlock()
				if silent > 10*time.Second {
					r.log.Error("radio silent for %v, closing connection", silent.Round(time.Second))
					conn.Close()
					return
				}
				r.Command("ping", nil)
			}
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		r.mu.Lock()
		r.lastRx = time.Now()
		r.mu.Unlock()
		r.handleLine(line)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("connection closed by radio")
}

// udpLoop dispatches DAX audio and meter packets. Only packets from the radio
// are accepted.
func (r *Radio) udpLoop(c *net.UDPConn, radio net.IP) {
	buf := make([]byte, 16384)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if radio != nil && !src.IP.Equal(radio) {
			continue
		}
		p, err := ParseVITA(buf[:n])
		if err != nil || p.OUI != flexOUI {
			continue
		}
		if p.PacketClass == classMeter {
			r.applyMeterValues(MeterValues(p))
			continue
		}
		rate := AudioRate(p.PacketClass)
		if rate == 0 {
			continue
		}
		r.mu.Lock()
		fn := r.audioFn
		ok := r.audioID != 0 && p.StreamID == r.audioID
		lost := 0
		if ok {
			if r.audioLast >= 0 {
				lost = (int(p.Count) - r.audioLast - 1 + 16) % 16
			}
			r.audioLast = int(p.Count)
		}
		r.mu.Unlock()
		if ok && fn != nil {
			fn(AudioLeft(p), rate, lost)
		}
	}
}

func (r *Radio) handleLine(line string) {
	if line == "" {
		return
	}
	switch line[0] {
	case 'V':
		r.log.Info("radio API version %s", line[1:])
	case 'H':
		h, _ := strconv.ParseUint(line[1:], 16, 32)
		r.mu.Lock()
		r.myHandle = uint32(h)
		r.connected = true
		port := r.udpPort
		hasUDP := r.udp != nil
		r.mu.Unlock()
		r.log.Info("connected, our client handle 0x%08X", h)
		for _, s := range []string{"sub client all", "sub slice all", "sub radio all", "sub meter all"} {
			r.Command(s, nil)
		}
		// A radio without GPS may refuse this.
		r.Command("sub gps all", func(uint32, string) {})
		if hasUDP {
			r.Command(fmt.Sprintf("client udpport %d", port), nil)
		}
		r.Command("info", r.onInfo)
		r.notify()
	case 'R':
		r.handleReply(line[1:])
	case 'S':
		r.log.Traffic("<- %s", line)
		if _, body, ok := strings.Cut(line, "|"); ok {
			if r.handleStatus(body) {
				r.notify()
			}
		}
	case 'M':
		r.log.Debug("radio message: %s", line[1:])
	default:
		r.log.Traffic("<- %s", line)
	}
}

func (r *Radio) handleReply(s string) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 2 {
		return
	}
	seq, _ := strconv.ParseUint(parts[0], 10, 32)
	code, _ := strconv.ParseUint(parts[1], 16, 32)
	msg := ""
	if len(parts) == 3 {
		msg = parts[2]
	}
	r.mu.Lock()
	cb := r.pending[uint32(seq)]
	delete(r.pending, uint32(seq))
	r.mu.Unlock()
	if cb != nil {
		cb(uint32(code), msg)
	}
}

// onInfo keeps the "info" reply (model, chassis serial, region, ...), so a
// radio reached by manual IP still identifies itself in the report.
func (r *Radio) onInfo(code uint32, msg string) {
	if code != 0 {
		return
	}
	info := ParseInfo(msg)
	r.mu.Lock()
	r.info = info
	if v := info["model"]; v != "" {
		r.model = v
	}
	if v := info["chassis_serial"]; v != "" && r.serial == "" {
		r.serial = v
	}
	if v := info["software_ver"]; v != "" && r.version == "" {
		r.version = v
	}
	if v := info["callsign"]; v != "" {
		r.callsign = v
	}
	r.mu.Unlock()
	r.notify()
}

// ParseInfo splits an info reply: key="value",key=value,...
func ParseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}

// Command sends an API command. cb (optional) receives the reply. Failures are
// always logged, so callers rarely need a callback.
func (r *Radio) Command(cmd string, cb replyFn) {
	r.mu.Lock()
	conn := r.conn
	if conn == nil {
		r.mu.Unlock()
		if cmd != "ping" {
			r.log.Warn("not connected, dropped command: %s", cmd)
		}
		if cb != nil {
			cb(0xFFFFFFFF, "not connected")
		}
		return
	}
	r.seq++
	seq := r.seq
	quiet := cmd == "ping"
	r.pending[seq] = func(code uint32, msg string) {
		if code != 0 {
			r.log.Warn("command failed (0x%08X %s): %s", code, msg, cmd)
		} else if !quiet {
			r.log.Traffic("<- R%d ok %s", seq, msg)
		}
		if cb != nil {
			cb(code, msg)
		}
	}
	r.mu.Unlock()
	if !quiet {
		r.log.Debug("-> C%d|%s", seq, cmd)
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "C%d|%s\n", seq, cmd); err != nil {
		r.log.Error("write to radio failed: %v", err)
		conn.Close()
	}
}

// CommandReply sends cmd and waits for the radio's reply.
func (r *Radio) CommandReply(cmd string, timeout time.Duration) (string, error) {
	type reply struct {
		code uint32
		msg  string
	}
	ch := make(chan reply, 1)
	r.Command(cmd, func(code uint32, msg string) { ch <- reply{code, msg} })
	select {
	case rep := <-ch:
		if rep.code != 0 {
			return rep.msg, fmt.Errorf("radio refused %q (0x%08X %s)", cmd, rep.code, rep.msg)
		}
		return rep.msg, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("no reply from the radio to %q", cmd)
	}
}

// CommandWait sends cmd and waits for the radio's reply.
func (r *Radio) CommandWait(cmd string, timeout time.Duration) error {
	_, err := r.CommandReply(cmd, timeout)
	return err
}

// handleStatus applies a status line. Returns true if state changed.
func (r *Radio) handleStatus(body string) bool {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "slice":
		if len(fields) < 2 {
			return false
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
		r.applySlice(idx, ParseKV(strings.Join(fields[2:], " ")))
		return true
	case "client":
		if len(fields) < 3 {
			return false
		}
		h, _ := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
		r.applyClient(uint32(h), fields[2], ParseKV(strings.Join(fields[3:], " ")))
		return true
	case "radio":
		if len(fields) >= 2 && fields[1] == "oscillator" {
			r.applyOscillator(ParseKV(strings.Join(fields[2:], " ")))
			return true
		}
		if len(fields) < 2 || !strings.Contains(fields[1], "=") {
			return false
		}
		kv := ParseKV(strings.Join(fields[1:], " "))
		r.mu.Lock()
		if v, ok := kv["nickname"]; ok {
			r.nickname = unescape(v)
		}
		if v, ok := kv["callsign"]; ok {
			r.callsign = v
		}
		if v, ok := kv["freq_error_ppb"]; ok {
			r.freqErrPPB = v
		}
		if v, ok := kv["cal_freq"]; ok {
			r.calFreq = v
		}
		r.mu.Unlock()
		return true
	case "gps":
		// gps lat=..#lon=..#freq_error=..#status=..#time=..; values may hold spaces.
		r.applyGPS(strings.TrimSpace(strings.TrimPrefix(body, "gps")))
		return true
	case "meter":
		r.applyMeterStatus(strings.TrimSpace(strings.TrimPrefix(body, "meter")))
		return true
	case "stream":
		r.applyStream(fields)
		return true
	}
	return false
}

// applyOscillator reads "radio oscillator state=gpsdo setting=auto locked=1
// ext_present=0 gpsdo_present=1 tcxo_present=1 [gnss_present=0]".
func (r *Radio) applyOscillator(kv map[string]string) {
	r.mu.Lock()
	old := r.ref
	ref := &r.ref
	ref.Known = true
	for k, v := range kv {
		switch k {
		case "state":
			ref.State = strings.ToLower(v)
		case "setting":
			ref.Setting = strings.ToLower(v)
		case "locked":
			ref.Locked = v == "1"
		case "tcxo_present":
			ref.TCXO = v == "1"
		case "gpsdo_present":
			ref.GPSDO = v == "1"
		case "ext_present":
			ref.External = v == "1"
		case "gnss_present":
			ref.GNSS = v == "1"
		}
	}
	now, cb := r.ref, r.OnReference
	r.mu.Unlock()
	if now != old {
		r.log.Info("reference: setting %s, using %s, locked %v (tcxo %v gpsdo %v ext %v gnss %v)",
			now.Setting, now.State, now.Locked, now.TCXO, now.GPSDO, now.External, now.GNSS)
		if cb != nil {
			cb(now)
		}
	}
}

func (r *Radio) applyGPS(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := &r.gps
	g.Known = true
	for _, kv := range strings.Split(s, "#") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		v = unescape(strings.TrimSpace(v))
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "freq_error":
			g.FreqError = v
		case "status":
			g.Status = v
		case "fix":
			g.Fix = v
		case "time":
			g.Time, g.TimeAt = v, time.Now()
		case "tracked":
			g.Tracked, _ = strconv.Atoi(v)
		case "visible":
			g.Visible, _ = strconv.Atoi(v)
		case "lat":
			g.Lat = v
		case "lon":
			g.Lon = v
		case "grid":
			g.Grid = v
		case "altitude":
			g.Altitude = v
		}
	}
}

// applyMeterStatus reads "1.src=RAD#1.num=0#1.nam=PATEMP#1.unit=degC#..."
// or "12 removed".
func (r *Radio) applyMeterStatus(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(s, "removed") {
		if id, err := strconv.Atoi(strings.Fields(s)[0]); err == nil {
			delete(r.meters, id)
		}
		return
	}
	for _, tok := range strings.Split(s, "#") {
		idx, rest, ok := strings.Cut(strings.TrimSpace(tok), ".")
		if !ok {
			continue
		}
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		id, err := strconv.Atoi(idx)
		if err != nil {
			continue
		}
		m := r.meters[id]
		if m == nil {
			m = &Meter{ID: id}
			r.meters[id] = m
		}
		v = unescape(v)
		switch k {
		case "src":
			m.Source = v
		case "nam":
			m.Name = v
		case "desc":
			m.Desc = v
		case "unit":
			m.Unit = v
		}
	}
}

func (r *Radio) applyMeterValues(vals map[int]int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, raw := range vals {
		m := r.meters[id]
		if m == nil {
			continue
		}
		m.Value, m.Fresh = scaleMeter(m.Unit, raw), true
	}
}

// scaleMeter converts a raw meter value as FlexLib does.
func scaleMeter(unit string, raw int16) float64 {
	switch strings.ToLower(unit) {
	case "db", "dbm", "dbfs", "swr":
		return float64(raw) / 128
	case "volts", "amps":
		return float64(raw) / 256
	case "degc", "degf":
		return float64(raw) / 64
	}
	return float64(raw)
}

func (r *Radio) applySlice(idx int, kv map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slices[idx]
	if s == nil {
		s = &Slice{Index: idx}
		r.slices[idx] = s
	}
	for k, v := range kv {
		switch k {
		case "in_use":
			s.InUse = v == "1"
		case "RF_frequency":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.FreqHz = int64(f*1e6 + 0.5)
			}
		case "mode":
			s.Mode = v
		case "client_handle":
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			s.ClientHandle = uint32(h)
		case "index_letter":
			s.Letter = v
		case "rit_on":
			s.RITOn = v == "1"
		case "rit_freq":
			s.RITFreq, _ = strconv.Atoi(v)
		case "digu_offset":
			s.DiguOffset, _ = strconv.Atoi(v)
		case "dax":
			s.DAX, _ = strconv.Atoi(v)
		}
	}
	if !s.InUse {
		delete(r.slices, idx)
	}
}

// applyStream notices the radio removing our DAX stream.
func (r *Radio) applyStream(fields []string) {
	if len(fields) < 3 {
		return
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
	if err != nil {
		return
	}
	if _, gone := ParseKV(strings.Join(fields[2:], " "))["removed"]; !gone {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.audioID != 0 && uint32(id) == r.audioID {
		r.log.Warn("radio removed our DAX stream 0x%08X", id)
		r.audioID = 0
	}
}

func (r *Radio) applyClient(h uint32, state string, kv map[string]string) {
	r.mu.Lock()
	if state == "disconnected" {
		if st, ok := r.stations[h]; ok {
			r.log.Info("station \"%s\" (%s) disconnected", st.Name, st.Program)
		}
		delete(r.stations, h)
		if h == r.boundHandle {
			r.log.Warn("bound station went away; waiting for it to return")
			r.boundHandle, r.boundName = 0, ""
		}
		r.mu.Unlock()
		r.tryBind()
		return
	}
	if h == r.myHandle {
		r.mu.Unlock()
		return
	}
	st := r.stations[h]
	if st == nil {
		st = &Station{Handle: h}
		r.stations[h] = st
	}
	if v, ok := kv["client_id"]; ok {
		st.ClientID = v
	}
	if v, ok := kv["program"]; ok {
		st.Program = unescape(v)
	}
	if v, ok := kv["station"]; ok {
		st.Name = unescape(v)
	}
	r.mu.Unlock()
	r.tryBind()
}

// tryBind binds to the wanted station if it is present and not yet bound.
// With no station chosen, a single GUI station is picked automatically.
func (r *Radio) tryBind() {
	r.mu.Lock()
	if !r.connected || r.boundHandle != 0 {
		r.mu.Unlock()
		return
	}
	var gui []*Station
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			gui = append(gui, st)
		}
	}
	var pick *Station
	for _, st := range gui {
		if st.Name == r.wantStation {
			pick = st
		}
	}
	if pick == nil && r.wantStation == "" && len(gui) == 1 {
		pick = gui[0]
	}
	if pick == nil {
		r.mu.Unlock()
		return
	}
	r.boundHandle, r.boundName = pick.Handle, pick.Name
	id, name := pick.ClientID, pick.Name
	r.mu.Unlock()
	r.log.Info("binding to station \"%s\"", name)
	r.Command("client bind client_id="+id, nil)
}

func (r *Radio) notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default: // an update is already pending
	}
}

func (r *Radio) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		Connected:    r.connected,
		IP:           r.ip,
		Model:        r.model,
		Serial:       r.serial,
		Version:      r.version,
		Nickname:     r.nickname,
		Callsign:     r.callsign,
		BoundStation: r.boundName,
		BoundHandle:  r.boundHandle,
		Ref:          r.ref,
		GPS:          r.gps,
		FreqErrPPB:   r.freqErrPPB,
		CalFreq:      r.calFreq,
		Info:         map[string]string{},
	}
	for k, v := range r.info {
		s.Info[k] = v
	}
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			s.Stations = append(s.Stations, *st)
		}
	}
	sort.Slice(s.Stations, func(i, j int) bool { return s.Stations[i].Name < s.Stations[j].Name })
	for _, sl := range r.slices {
		s.Slices = append(s.Slices, *sl)
	}
	sort.Slice(s.Slices, func(i, j int) bool { return s.Slices[i].Index < s.Slices[j].Index })
	for _, m := range r.meters {
		if m.Name != "" && (m.Source == "RAD" || m.IsTemp()) {
			s.Meters = append(s.Meters, *m)
		}
	}
	sort.Slice(s.Meters, func(i, j int) bool { return s.Meters[i].Name < s.Meters[j].Name })
	return s
}

// ResyncSlices makes the radio resend every slice's state. The radio does not
// echo our own changes back as status, so this is how they are confirmed.
func (r *Radio) ResyncSlices() error {
	err := r.CommandWait("sub slice all", 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	r.notify()
	return err
}

// TuneDigital puts a slice in DIGU with RIT off and no DIGU offset, so the
// slice frequency is the true suppressed-carrier (dial) frequency, then tunes
// it to dialHz and checks the radio reports it.
//
// A GUI client (SmartSDR, AetherSDR) may react to a mode change by re-applying
// its own DIGU offset and frequency, so the mode change is allowed to settle
// before tuning, and the tune is retried once if something put it back.
func (r *Radio) TuneDigital(slice int, dialHz int64) error {
	if err := r.CommandWait(fmt.Sprintf("slice set %d mode=DIGU", slice), 3*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	var got Slice
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.CommandWait(fmt.Sprintf("slice set %d digu_offset=0 rit_on=0", slice), 3*time.Second); err != nil {
			return err
		}
		if err := r.CommandWait(fmt.Sprintf("slice tune %d %.6f", slice, float64(dialHz)/1e6), 3*time.Second); err != nil {
			return err
		}
		time.Sleep(700 * time.Millisecond)
		if err := r.ResyncSlices(); err != nil {
			return err
		}
		r.mu.Lock()
		if s := r.slices[slice]; s != nil {
			got = *s
		}
		r.mu.Unlock()
		if got.FreqHz == dialHz && got.Mode == "DIGU" && got.DiguOffset == 0 && !got.RITOn {
			return nil
		}
		r.log.Warn("slice %d reads %d Hz %s offset %d after tuning to %d Hz; retrying", slice, got.FreqHz, got.Mode, got.DiguOffset, dialHz)
	}
	return fmt.Errorf("the radio reports %.3f kHz %s (DIGU offset %d) instead of %.3f kHz DIGU; another program may be changing it",
		float64(got.FreqHz)/1000, got.Mode, got.DiguOffset, float64(dialHz)/1000)
}

// RestoreSlice puts back a slice's mode, DIGU offset, RIT, DAX channel and
// frequency as they were in orig. The mode goes first: a GUI client re-applies
// its own settings after a mode change.
func (r *Radio) RestoreSlice(orig Slice) error {
	if err := r.CommandWait(fmt.Sprintf("slice set %d mode=%s", orig.Index, orig.Mode), 3*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if err := r.CommandWait(fmt.Sprintf("slice set %d dax=%d digu_offset=%d rit_on=%s", orig.Index, orig.DAX, orig.DiguOffset,
		map[bool]string{true: "1", false: "0"}[orig.RITOn]), 3*time.Second); err != nil {
		return err
	}
	if err := r.CommandWait(fmt.Sprintf("slice tune %d %.6f", orig.Index, float64(orig.FreqHz)/1e6), 3*time.Second); err != nil {
		return err
	}
	return r.ResyncSlices()
}

// StartAudio assigns DAX channel ch to the slice and opens a dax_rx stream
// for it. fn is called from the UDP goroutine with each packet's samples.
func (r *Radio) StartAudio(slice, ch int, fn AudioHandler) error {
	r.StopAudio()
	r.mu.Lock()
	ok := r.udp != nil
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no UDP socket for DAX audio")
	}
	if err := r.CommandWait(fmt.Sprintf("slice set %d dax=%d", slice, ch), 3*time.Second); err != nil {
		return err
	}
	msg, err := r.CommandReply(fmt.Sprintf("stream create type=dax_rx dax_channel=%d", ch), 5*time.Second)
	if err != nil {
		return err
	}
	id, perr := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(msg), "0x"), 16, 32)
	if perr != nil {
		return fmt.Errorf("bad DAX stream id %q", msg)
	}
	r.mu.Lock()
	r.audioID, r.audioFn, r.audioLast = uint32(id), fn, -1
	r.mu.Unlock()
	r.log.Info("DAX channel %d stream 0x%08X open for slice %d", ch, id, slice)
	return nil
}

// StopAudio removes our DAX stream.
func (r *Radio) StopAudio() {
	r.mu.Lock()
	id := r.audioID
	r.audioID, r.audioFn = 0, nil
	connected := r.connected
	r.mu.Unlock()
	if id != 0 && connected {
		r.Command(fmt.Sprintf("stream remove 0x%08X", id), nil)
		r.log.Info("removed DAX stream 0x%08X", id)
	}
}

// AudioOpen reports whether our DAX stream exists.
func (r *Radio) AudioOpen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.audioID != 0 && r.connected
}

// SliceError asks the radio for the slice's known tuning error ("slice
// get_error"): the difference between the requested and the actual frequency
// of its digital tuning, in mHz, for receive and transmit. Negative means the
// slice is tuned low. It excludes any error of the reference itself.
func (r *Radio) SliceError(slice int) (rxMilliHz, txMilliHz float64, err error) {
	msg, err := r.CommandReply(fmt.Sprintf("slice get_error %d", slice), 3*time.Second)
	if err != nil {
		return 0, 0, err
	}
	rx, tx, _ := strings.Cut(strings.TrimSpace(msg), ",")
	if rxMilliHz, err = strconv.ParseFloat(strings.TrimSpace(rx), 64); err != nil {
		return 0, 0, fmt.Errorf("bad get_error reply %q", msg)
	}
	txMilliHz, _ = strconv.ParseFloat(strings.TrimSpace(tx), 64)
	return rxMilliHz, txMilliHz, nil
}

// SetOscillator selects the radio's 10 MHz reference: auto, tcxo, gpsdo or
// external.
func (r *Radio) SetOscillator(setting string) error {
	switch setting {
	case "auto", "tcxo", "gpsdo", "external":
	default:
		return fmt.Errorf("unknown oscillator setting %q", setting)
	}
	if err := r.CommandWait("radio oscillator "+setting, 3*time.Second); err != nil {
		return err
	}
	// The radio accepted it, but it doesn't tell the client that made the
	// change when only the setting changes (gpsdo -> auto with the GPSDO still
	// in use): that connection keeps seeing the old setting, even after "sub
	// radio all", while a new connection sees the new one (FLEX-6600M 4.4.0).
	// So record it ourselves; a change of oscillator in use still arrives as
	// status.
	r.mu.Lock()
	old := r.ref
	r.ref.Setting = setting
	now, cb := r.ref, r.OnReference
	r.mu.Unlock()
	r.log.Info("oscillator setting %s -> %s (accepted by the radio)", old.Setting, setting)
	if cb != nil && now != old {
		cb(now)
	}
	r.notify()
	return nil
}
