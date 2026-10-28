// Package fmtdata reads WSJT-X FreqCal measurement files (fmt.all) and
// turns them into calibration fits and carrier-frequency answers.
//
// WSJT-X writes one line per measurement (lib/freqcal.f90):
//
//	HH:MM:SS  kHz  CAL  Offset  fMeas  DF  Level  S/N  [*]
//
// kHz is (dial + Rx offset) / 1000 truncated, so it only identifies the
// station; the exact dial must come from the operator (or the radio). The
// trailing "*" marks S/N below 20 dB. Times are UTC with no date.
package fmtdata

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Line is one FreqCal measurement.
type Line struct {
	N      int     `json:"n"`      // 1-based line number in the file
	Time   string  `json:"time"`   // HH:MM:SS UTC
	Sec    int     `json:"sec"`    // seconds of the UTC day
	KHz    int     `json:"khz"`    // station frequency in kHz (truncated)
	Cal    int     `json:"cal"`    // CAL column (1 in FreqCal mode)
	Offset int     `json:"offset"` // WSJT-X Rx audio frequency, Hz
	FMeas  float64 `json:"fmeas"`  // measured audio frequency, Hz
	DF     float64 `json:"df"`     // fMeas - Offset, Hz
	Level  float64 `json:"level"`  // dB
	SNR    float64 `json:"snr"`    // dB
	Flag   bool    `json:"flag"`   // WSJT-X "*" (S/N < 20 dB)
	Raw    string  `json:"raw"`
}

// BadLine is a non-blank line that could not be read as a measurement.
type BadLine struct {
	N   int    `json:"n"`
	Raw string `json:"raw"`
}

// File is a parsed fmt.all.
type File struct {
	Path  string    `json:"path"`
	SHA   string    `json:"sha"`
	Lines []Line    `json:"lines"`
	Bad   []BadLine `json:"bad"`
}

// ParseFile reads and parses path.
func ParseFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	f.Path = path
	f.SHA = hex.EncodeToString(sum[:])
	return f, nil
}

// Parse reads fmt.all lines from r. Unreadable lines are kept in Bad.
func Parse(r io.Reader) (*File, error) {
	f := &File{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		raw := strings.TrimRight(sc.Text(), "\x00\r\n\t ")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		l, ok := ParseLine(raw)
		if !ok {
			f.Bad = append(f.Bad, BadLine{N: n, Raw: raw})
			continue
		}
		l.N = n
		f.Lines = append(f.Lines, l)
	}
	return f, sc.Err()
}

// ParseLine decodes one measurement line.
func ParseLine(raw string) (Line, bool) {
	s := strings.ReplaceAll(raw, "\x00", "")
	flag := false
	if t := strings.TrimSpace(s); strings.HasSuffix(t, "*") {
		flag = true
		s = strings.TrimSuffix(t, "*")
	}
	fs := strings.Fields(s)
	// Tolerate a leading date (yyyy-mm-dd or yymmdd) from older tools.
	if len(fs) == 9 && (strings.Count(fs[0], "-") == 2 || (len(fs[0]) == 6 && !strings.Contains(fs[0], ":"))) {
		fs = fs[1:]
	}
	if len(fs) != 8 {
		return Line{}, false
	}
	sec, ok := parseHMS(fs[0])
	if !ok {
		return Line{}, false
	}
	l := Line{Time: fmtHMS(sec), Sec: sec, Flag: flag, Raw: raw}
	var err error
	ints := []*int{&l.KHz, &l.Cal, &l.Offset}
	for i, p := range ints {
		if *p, err = strconv.Atoi(fs[1+i]); err != nil {
			return Line{}, false
		}
	}
	floats := []*float64{&l.FMeas, &l.DF, &l.Level, &l.SNR}
	for i, p := range floats {
		if *p, err = strconv.ParseFloat(fs[4+i], 64); err != nil {
			return Line{}, false
		}
	}
	// WSJT-X writes -99.9 when no peak was found.
	if l.FMeas <= 0 || l.KHz <= 0 {
		return Line{}, false
	}
	return l, true
}

// parseHMS accepts HH:MM:SS or HHMMSS.
func parseHMS(s string) (int, bool) {
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 6 {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	h, m, sec := v/10000, (v/100)%100, v%100
	if h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	return h*3600 + m*60 + sec, true
}

func fmtHMS(sec int) string {
	sec = ((sec % 86400) + 86400) % 86400
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec/60)%60, sec%60)
}

// SecDiff returns a-b in seconds, wrapped into [-12h, +12h) so times on
// either side of 0000Z compare sensibly.
func SecDiff(a, b int) int {
	d := ((a-b)%86400 + 86400) % 86400
	if d >= 43200 {
		d -= 86400
	}
	return d
}
