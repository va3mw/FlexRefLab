package flex

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/va3mw/FlexRefLab/internal/logx"
)

func newTestRadio(t *testing.T) *Radio {
	lg := logx.New(filepath.Join(t.TempDir(), "logs"))
	t.Cleanup(lg.Close)
	r := NewRadio(lg, nil)
	r.meters, r.slices, r.stations, r.info = map[int]*Meter{}, map[int]*Slice{}, map[uint32]*Station{}, map[string]string{}
	return r
}

func TestStatusParsing(t *testing.T) {
	r := newTestRadio(t)
	var got Reference
	r.OnReference = func(x Reference) { got = x }
	r.handleStatus("radio oscillator state=gpsdo setting=auto locked=1 ext_present=1 gpsdo_present=0 tcxo_present=1 gnss_present=1")
	if !got.Known || got.State != "gpsdo" || !got.Locked || !got.GNSS || got.GPSDO || !got.External {
		t.Errorf("oscillator %+v", got)
	}
	r.handleStatus("gps lat=43.1#lon=-79.2#grid=FN03#altitude=100 m#tracked=9#visible=14#speed=0 kts#freq_error=0.003#status=Fine Lock#time=12:00:01Z#fix=TimeOnly")
	if g := r.gps; g.Tracked != 9 || g.Visible != 14 || g.Status != "Fine Lock" || g.Fix != "TimeOnly" || g.FreqError != "0.003" {
		t.Errorf("gps %+v", g)
	}
	r.handleStatus("radio freq_error_ppb=-12 cal_freq=10.000000 nickname=Bench")
	if r.freqErrPPB != "-12" || r.calFreq != "10.000000" || r.nickname != "Bench" {
		t.Errorf("radio status %q %q %q", r.freqErrPPB, r.calFreq, r.nickname)
	}
	r.handleStatus("meter 7.src=RAD#7.num=0#7.nam=PATEMP#7.low=-50.0#7.hi=100.0#7.desc=PA\x7ftemperature#7.unit=degC#7.fps=1#")
	r.handleStatus("meter 8.src=SLC#8.num=0#8.nam=LEVEL#8.unit=dBm#")
	// A meter packet: id 7 = 38.5 °C (raw ×64), id 8 = -73 dBm (raw ×128).
	pl := make([]byte, 8)
	binary.BigEndian.PutUint16(pl[0:], 7)
	binary.BigEndian.PutUint16(pl[2:], uint16(int16(38.5*64)))
	binary.BigEndian.PutUint16(pl[4:], 8)
	lvl := int16(-73 * 128)
	binary.BigEndian.PutUint16(pl[6:], uint16(lvl))
	r.applyMeterValues(MeterValues(Packet{Payload: pl}))
	snap := r.Snapshot()
	if tmp := snap.Temps(); math.Abs(tmp["PATEMP"]-38.5) > 1e-9 || len(tmp) != 1 {
		t.Errorf("temps %v", tmp)
	}
	if len(snap.Meters) != 1 || snap.Meters[0].Desc != "PA temperature" {
		t.Errorf("meters %+v (slice meters must be left out)", snap.Meters)
	}
	r.handleStatus("meter 7 removed")
	if len(r.Snapshot().Meters) != 0 {
		t.Error("meter not removed")
	}
}

func TestParseVITAAudio(t *testing.T) {
	// Header: type 1, class id, TSI=3, TSF=1, 7 header words + 4 payload words.
	words := 7 + 4
	b := make([]byte, words*4)
	be := binary.BigEndian
	be.PutUint32(b[0:], 1<<28|1<<27|3<<22|1<<20|5<<16|uint32(words))
	be.PutUint32(b[4:], 0x40000008)
	be.PutUint32(b[8:], flexOUI)
	be.PutUint32(b[12:], 0x534C<<16|classFloat24k)
	for i, v := range []float32{0.5, -0.5, 0.25, -0.25} {
		be.PutUint32(b[28+i*4:], math.Float32bits(v))
	}
	p, err := ParseVITA(b)
	if err != nil || p.StreamID != 0x40000008 || p.Count != 5 || !p.HasTS || AudioRate(p.PacketClass) != 24000 {
		t.Fatalf("packet %+v %v", p, err)
	}
	if l := AudioLeft(p); len(l) != 2 || l[0] != 0.5 || l[1] != 0.25 {
		t.Errorf("left channel %v", l)
	}
}
