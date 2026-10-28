// Command oscset selects a radio's reference oscillator and prints what the
// radio reports before and after: oscset -radio <IP> -set auto|tcxo|gpsdo|external
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/va3mw/FlexRefLab/internal/flex"
	"github.com/va3mw/FlexRefLab/internal/logx"
)

func main() {
	ip := flag.String("radio", "", "radio IP")
	set := flag.String("set", "", "auto, tcxo, gpsdo or external")
	flag.Parse()
	l := logx.New(filepath.Join(os.TempDir(), "flexreflab-oscset"))
	defer l.Close()
	r := flex.NewRadio(l, nil)
	r.Connect(*ip, "", "", "", "")
	defer r.Disconnect()
	for i := 0; i < 50 && !r.Snapshot().Ref.Known; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("before: %+v\n", r.Snapshot().Ref)
	if *set != "" {
		err := r.SetOscillator(*set)
		fmt.Printf("set %s: err %v\nafter:  %+v\n", *set, err, r.Snapshot().Ref)
	}
}
