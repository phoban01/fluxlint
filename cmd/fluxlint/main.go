// Command fluxlint statically analyses a Flux repository.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
	"github.com/phoban01/fluxlint/pkg/report"
)

const usage = `fluxlint — static convergence and timing analysis for Flux repositories

Usage:
  fluxlint check   [flags] [entrypoint ...]   analyse the repository
  fluxlint explain <rule>                      describe a rule
  fluxlint rules                               list all rules

An entrypoint is the directory a cluster's bootstrap Kustomization points at
(the --path given to 'flux bootstrap'), relative to the repository root.
Entrypoints may also be listed in .fluxlint.yaml.

Exit codes: 0 clean, 1 findings at or above --fail-on, 2 usage or I/O error.

Flags for check:
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func main() {
	if len(os.Args) < 2 {
		printUsage(nil)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "check":
		os.Exit(check(os.Args[2:]))
	case "explain":
		os.Exit(explain(os.Args[2:]))
	case "rules":
		for _, r := range lint.Rules {
			fmt.Printf("%-8s %-26s %s\n", r.ID, r.Name, r.Severity)
		}
	case "-h", "--help", "help":
		printUsage(nil)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printUsage(nil)
		os.Exit(2)
	}
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, usage)
	if fs == nil {
		fs = checkFlags(&checkOpts{})
	}
	fs.PrintDefaults()
}

type checkOpts struct {
	repo, cfgPath, format, failOn string
	verbose                       bool
}

func checkFlags(o *checkOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&o.repo, "repo", ".", "repository root")
	fs.StringVar(&o.cfgPath, "config", "", "config file (default <repo>/"+config.DefaultFile+")")
	fs.StringVar(&o.format, "format", "text", "output format: text or json")
	fs.StringVar(&o.failOn, "fail-on", "error", "lowest severity that fails the run: error or warning")
	fs.BoolVar(&o.verbose, "v", false, "also list suggestions (info)")
	fs.Usage = func() { printUsage(fs) }
	return fs
}

func check(args []string) int {
	o := &checkOpts{}
	fs := checkFlags(o)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.cfgPath == "" {
		o.cfgPath = filepath.Join(o.repo, config.DefaultFile)
	}
	cfg, err := config.Load(o.cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	entrypoints := fs.Args()
	if len(entrypoints) == 0 {
		entrypoints = cfg.Entrypoints
	}
	if len(entrypoints) == 0 {
		fmt.Fprintln(os.Stderr, "error: no entrypoints: pass them as arguments or list them in", config.DefaultFile)
		return 2
	}

	start := time.Now()
	results := make([]*lint.Result, len(entrypoints))
	errs := make([]error, len(entrypoints))
	var wg sync.WaitGroup
	for i, ep := range entrypoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree, err := render.Tree(o.repo, ep, cfg)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", ep, err)
				return
			}
			results[i] = lint.Run(tree, cfg)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
	}

	sum := report.Summary{Results: results, Elapsed: time.Since(start)}
	switch o.format {
	case "json":
		if err := report.JSON(os.Stdout, sum); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
	default:
		report.Text(os.Stdout, sum, o.verbose)
	}
	if sum.Failed(lint.Severity(o.failOn)) {
		return 1
	}
	return 0
}

func explain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: fluxlint explain <rule>")
		return 2
	}
	r, ok := lint.RuleByID(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown rule %q; see 'fluxlint rules'\n", args[0])
		return 2
	}
	fmt.Printf("%s  %s  (default severity: %s)\n\n%s\n", r.ID, r.Name, r.Severity, r.Help)
	return 0
}
