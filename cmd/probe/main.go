// Command probe exercises the radio side of Flex Reference Lab against a real
// radio: it connects, prints the reference, GPS, meters and slices, reads
// slice get_error, then (with -secs > 0) tunes the slice, opens DAX audio and
// prints tracker readings. It restores the slice's frequency, mode and DAX
// channel afterwards. It never changes the reference oscillator.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/va3mw/FlexRefLab/internal/flex"
	"github.com/va3mw/FlexRefLab/internal/logx"
	"github.com/va3mw/FlexRefLab/internal/measure"
	"github.com/va3mw/FlexRefLab/internal/store"
)

func main() {
	name := flag.String("radio", "", "nickname, serial or IP of the radio")
	station := flag.String("station", "", "GUI station to bind to (empty: the only one)")
	letter := flag.String("slice", "A", "slice letter")
	carrier := flag.Float64("carrier", 10e6, "true carrier frequency, Hz")
	tone := flag.Float64("tone", 1000, "audio tone, Hz")
	dax := flag.Int("dax", 1, "DAX channel")
	secs := flag.Int("secs", 0, "seconds to measure (0: read-only)")
	verbose := flag.Bool("v", false, "print the log")
	flag.Parse()

	l := logx.New(filepath.Join(os.TempDir(), "flexreflab-probe"))
	defer l.Close()
	if *verbose {
		l.OnEntry(func(e logx.Entry) { fmt.Printf("  log %s %s [%s] %s\n", e.Time, e.Level, e.Source, e.Msg) })
	}
	disc := flex.NewDiscovery(l, nil)
	disc.Start()
	ip, serial, model, ver := *name, "", "", ""
	for i := 0; i < 8 && !strings.Contains(ip, "."); i++ {
		for _, r := range disc.List() {
			if strings.EqualFold(r.Nickname, *name) || r.Serial == *name {
				ip, serial, model, ver = r.IP, r.Serial, r.Model, r.Version
			}
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(ip, ".") {
		fmt.Println("radio not found; seen:")
		for _, r := range disc.List() {
			fmt.Printf("  %s %q %s %s\n", r.Model, r.Nickname, r.IP, r.Serial)
		}
		os.Exit(1)
	}
	fmt.Printf("radio %s %s at %s, firmware %s\n", model, serial, ip, ver)
	r := flex.NewRadio(l, nil)
	r.Connect(ip, model, serial, ver, *station)
	defer r.Disconnect()
	var snap flex.Snapshot
	for i := 0; i < 50; i++ {
		time.Sleep(200 * time.Millisecond)
		snap = r.Snapshot()
		if snap.Connected && snap.BoundHandle != 0 && snap.Ref.Known && len(snap.Meters) > 0 {
			break
		}
	}
	time.Sleep(2 * time.Second) // let meter values arrive
	snap = r.Snapshot()
	if !snap.Connected {
		fmt.Println("could not connect")
		os.Exit(1)
	}
	fmt.Printf("stations: ")
	for _, s := range snap.Stations {
		fmt.Printf("%q (%s) ", s.Name, s.Program)
	}
	fmt.Printf("\nbound to %q\n", snap.BoundStation)
	fmt.Printf("reference: %+v\n", snap.Ref)
	fmt.Printf("gps: %+v\n", snap.GPS)
	fmt.Printf("freq_error_ppb=%q cal_freq=%q\n", snap.FreqErrPPB, snap.CalFreq)
	fmt.Printf("info: %v\n", snap.Info)
	for _, m := range snap.Meters {
		fmt.Printf("meter %3d %-4s %-12s %-6s %8.2f fresh=%v  %s\n", m.ID, m.Source, m.Name, m.Unit, m.Value, m.Fresh, m.Desc)
	}
	fmt.Printf("temps: %v\n", snap.Temps())
	for _, s := range snap.Slices {
		fmt.Printf("slice %d %s %.6f MHz %s handle 0x%X dax=%d digu_offset=%d rit=%v\n", s.Index, s.Letter, float64(s.FreqHz)/1e6, s.Mode, s.ClientHandle, s.DAX, s.DiguOffset, s.RITOn)
	}
	sl, ok := snap.SliceByLetter(*letter)
	if !ok {
		fmt.Printf("slice %s not owned by the bound station\n", *letter)
		os.Exit(1)
	}
	rx, tx, err := r.SliceError(sl.Index)
	fmt.Printf("slice get_error %d: rx %+.6f mHz tx %+.6f mHz err %v\n", sl.Index, rx, tx, err)
	if *secs <= 0 {
		return
	}

	// Measure, then put the slice back.
	orig := sl
	defer func() {
		r.StopAudio()
		r.CommandWait(fmt.Sprintf("slice set %d mode=%s", orig.Index, orig.Mode), 3*time.Second)
		time.Sleep(500 * time.Millisecond)
		r.CommandWait(fmt.Sprintf("slice set %d dax=%d digu_offset=%d", orig.Index, orig.DAX, orig.DiguOffset), 3*time.Second)
		r.CommandWait(fmt.Sprintf("slice tune %d %.6f", orig.Index, float64(orig.FreqHz)/1e6), 3*time.Second)
		r.ResyncSlices()
		after := r.Snapshot()
		s, _ := after.SliceByLetter(*letter)
		fmt.Printf("restored slice %s: %.6f MHz %s dax=%d offset=%d\n", s.Letter, float64(s.FreqHz)/1e6, s.Mode, s.DAX, s.DiguOffset)
	}()
	dial := int64(*carrier - *tone + 0.5)
	if err := r.TuneDigital(sl.Index, dial); err != nil {
		fmt.Println("tune:", err)
		return
	}
	rx, _, err = r.SliceError(sl.Index)
	fmt.Printf("tuned %.6f MHz DIGU; get_error now %+.6f mHz (%v)\n", float64(dial)/1e6, rx, err)
	tr := measure.NewTracker(*carrier-float64(dial), 300, 1)
	gates := make(chan measure.Gate, 64)
	pkts, lostTot := 0, 0
	rate := 0
	if err := r.StartAudio(sl.Index, *dax, func(s []float32, rt, lost int) {
		pkts++
		lostTot += lost
		rate = rt
		for _, g := range tr.Push(s, rt, lost, time.Now()) {
			gates <- g
		}
	}); err != nil {
		fmt.Println("DAX:", err)
		return
	}
	end := time.After(time.Duration(*secs) * time.Second)
	base := float64(dial) + rx/1000
	for {
		select {
		case g := <-gates:
			st := tr.Status()
			s := r.Snapshot()
			ppb := store.PPBFrom(base+g.FitHz, *carrier)
			fmt.Printf("%s valid=%-5v %-24s tone %10.5f fit %10.5f Hz  %+9.3f ppb  SNR %5.1f dB  lvl %6.1f dBFS  lost %d  nco %.2f peak %.2f/%.0f dB  ref %s locked=%v gps %d/%d\n",
				g.End.Format("15:04:05"), g.Valid, g.Why, g.FreqHz, g.FitHz, ppb, g.SNRdB, g.LevelDB, g.Lost, st.NCO, st.PeakHz, st.PeakSNR,
				s.Ref.State, s.Ref.Locked, s.GPS.Tracked, s.GPS.Visible)
		case <-end:
			fmt.Printf("%d packets at %d Hz, %d lost\n", pkts, rate, lostTot)
			return
		}
	}
}
