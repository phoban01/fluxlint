// Package report formats lint results.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/phoban01/fluxlint/pkg/lint"
)

// Summary of one run across entrypoints.
type Summary struct {
	Results []*lint.Result
	Elapsed time.Duration
}

func (s Summary) count(sev lint.Severity) int {
	n := 0
	for _, r := range s.Results {
		for _, f := range r.Findings {
			if f.Severity == sev {
				n++
			}
		}
	}
	return n
}

// Failed reports whether any finding is at or above the threshold.
func (s Summary) Failed(failOn lint.Severity) bool {
	if s.count(lint.Error) > 0 {
		return true
	}
	return failOn == lint.Warning && s.count(lint.Warning) > 0
}

// Text writes a human readable report.
func Text(w io.Writer, s Summary, verbose bool) {
	for _, r := range s.Results {
		opaque, objects := 0, 0
		for _, c := range r.Tree.Components {
			objects += len(c.Objects)
			if c.Opaque != "" {
				opaque++
			}
		}
		fmt.Fprintf(w, "%s: %d components (%d not rendered), %d objects\n", r.Tree.Entrypoint, len(r.Tree.Components), opaque, objects)
		for _, f := range r.Findings {
			if f.Severity == lint.Info && !verbose && f.Rule != "FL-T001" {
				continue
			}
			where := f.Component
			if f.Object != "" {
				where = strings.TrimPrefix(where+" "+f.Object, " ")
			}
			if where != "" {
				where += ": "
			}
			fmt.Fprintf(w, "  %-7s %s %s  %s%s\n", f.Severity, f.Rule, f.Name, where, f.Message)
			if f.File != "" {
				fmt.Fprintf(w, "            at %s:%d\n", f.File, f.Line)
			}
			for _, d := range f.Detail {
				fmt.Fprintf(w, "            %s\n", d)
			}
		}
		fmt.Fprintln(w)
	}
	hidden := ""
	if n := s.count(lint.Info); n > 0 && !verbose {
		hidden = " (use -v to list suggestions)"
	}
	fmt.Fprintf(w, "%d errors, %d warnings, %d suggestions%s in %v\n",
		s.count(lint.Error), s.count(lint.Warning), s.count(lint.Info), hidden, s.Elapsed.Round(time.Millisecond))
}

// JSON writes machine readable output.
func JSON(w io.Writer, s Summary) error {
	type entry struct {
		Entrypoint string         `json:"entrypoint"`
		Findings   []lint.Finding `json:"findings"`
		Timing     *lint.Timing   `json:"timing,omitempty"`
	}
	out := struct {
		Entrypoints []entry `json:"entrypoints"`
		ElapsedMS   int64   `json:"elapsedMs"`
	}{ElapsedMS: s.Elapsed.Milliseconds()}
	for _, r := range s.Results {
		f := r.Findings
		if f == nil {
			f = []lint.Finding{}
		}
		out.Entrypoints = append(out.Entrypoints, entry{r.Tree.Entrypoint, f, r.Timing})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
