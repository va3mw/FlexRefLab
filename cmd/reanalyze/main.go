// Command reanalyze rebuilds report.html and analysis.json for saved runs, so
// old runs pick up improvements to the analysis. Usage: reanalyze <run dir>...
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/va3mw/FlexRefLab/internal/analysis"
	"github.com/va3mw/FlexRefLab/internal/report"
	"github.com/va3mw/FlexRefLab/internal/safejson"
	"github.com/va3mw/FlexRefLab/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: reanalyze <run dir>...")
		os.Exit(2)
	}
	for _, dir := range os.Args[1:] {
		run, err := store.Root{Dir: filepath.Dir(dir)}.Load(filepath.Base(dir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", dir, err)
			continue
		}
		res := analysis.Analyze(run)
		html := report.HTML(run, res, report.Build(run, res, 1000), run.Meta.AppVersion+" (reanalysed)")
		os.WriteFile(filepath.Join(dir, "report.html"), []byte(html), 0o644)
		b, _ := json.MarshalIndent(safejson.Tree(res), "", " ")
		os.WriteFile(filepath.Join(dir, "analysis.json"), b, 0o644)
		fmt.Printf("%s: %d samples\n", run.Meta.ID, len(run.Samples))
		for _, f := range res.Findings {
			fmt.Printf("  [%s] %s\n", f.Level, f.Msg)
		}
		for _, s := range res.Segments {
			fmt.Printf("  segment %d %s: mean %+.3f ppb σ %.3f, GPS freq_error %+.5f ppb, slice err %+.4f mHz, %s\n",
				s.Index+1, s.RefName, s.MeanPPB, s.StdPPB, s.GPSErrPPB, s.SliceErrMH, s.Noise)
		}
	}
}
