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
	// Version of fluxlint, for reports that are read somewhere else.
	Version string
}

func (s Summary) count(sev lint.Severity) int {
	n := 0
	for _, r := range s.Results {
		for _, f := range r.Findings {
			if f.Severity == sev && !f.Existing {
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
		existing := 0
		for _, f := range r.Findings {
			if f.Existing {
				existing++
				continue
			}
			if f.Severity == lint.Info && !verbose && f.Rule != "FL-T001" && !strings.HasPrefix(f.Rule, "FL-D") {
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
		if existing > 0 {
			fmt.Fprintf(w, "  (%d finding(s) already present at the base commit are not shown)\n", existing)
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

// component says what was analysed: a run with no findings on a component
// that could not be rendered says nothing about it.
type component struct {
	Component string `json:"component"`
	// Cluster is set when the component applies to another cluster
	// (spec.kubeConfig): the kubeconfig Secret that names it.
	Cluster  string `json:"cluster,omitempty"`
	Parent   string `json:"parent,omitempty"`
	Rendered bool   `json:"rendered"`
	// NotRendered is why: the source was unavailable, the build failed, …
	NotRendered string `json:"notRendered,omitempty"`
	Error       string `json:"error,omitempty"`
	Objects     int    `json:"objects"`
	Source      string `json:"source,omitempty"`
	Revision    string `json:"revision,omitempty"`
	FloatingRef string `json:"floatingRef,omitempty"`
	Path        string `json:"path,omitempty"`
}

// JSON writes machine readable output: the findings, and enough about the
// run to judge them (the version, what each component was rendered from, and
// which Kubernetes release built-in objects were checked against).
func JSON(w io.Writer, s Summary) error {
	type entry struct {
		Entrypoint  string         `json:"entrypoint"`
		KubeRelease string         `json:"kubeRelease,omitempty"`
		Findings    []lint.Finding `json:"findings"`
		Timing      *lint.Timing   `json:"timing,omitempty"`
		Components  []component    `json:"components"`
	}
	out := struct {
		Version     string  `json:"version,omitempty"`
		Entrypoints []entry `json:"entrypoints"`
		ElapsedMS   int64   `json:"elapsedMs"`
	}{Version: s.Version, ElapsedMS: s.Elapsed.Milliseconds()}
	for _, r := range s.Results {
		f := r.Findings
		if f == nil {
			f = []lint.Finding{}
		}
		e := entry{Entrypoint: r.Tree.Entrypoint, KubeRelease: r.Tree.KubeRelease, Findings: f, Timing: r.Timing, Components: []component{}}
		for _, c := range r.Tree.Components {
			cc := component{Component: c.String(), Cluster: c.Cluster, Rendered: c.Opaque == "", NotRendered: c.Opaque, Objects: len(c.Objects),
				Revision: c.SourceRevision, FloatingRef: c.FloatingRef, Path: c.Path}
			if c.Parent != nil {
				cc.Parent = c.Parent.String()
			}
			if c.Source.Name != "" {
				cc.Source = c.Source.String()
			}
			switch {
			case c.BuildErr != nil:
				cc.Error = c.BuildErr.Error()
			case c.SourceErr != nil:
				cc.Error = c.SourceErr.Error()
			}
			e.Components = append(e.Components, cc)
		}
		out.Entrypoints = append(out.Entrypoints, e)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
