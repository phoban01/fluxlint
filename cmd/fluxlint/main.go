// Command fluxlint statically analyses a Flux repository.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
	"github.com/phoban01/fluxlint/pkg/report"
	"github.com/phoban01/fluxlint/pkg/source"
)

const usage = `fluxlint — static convergence and timing analysis for Flux repositories

Usage:
  fluxlint check   [flags] [entrypoint ...]   analyse the repository
  fluxlint graph   [flags] [entrypoint]       print the dependency graph (dot or mermaid)
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
	case "graph":
		os.Exit(graphCmd(os.Args[2:]))
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
	repo, cfgPath, format, failOn, cacheDir, output, base string
	verbose, offline, refresh                             bool
}

func newResolver(o *checkOpts, cfg *config.Config) (*source.Resolver, error) {
	opts := source.Options{CacheDir: o.cacheDir}
	if opts.CacheDir == "" && cfg.Sources.CacheDir != "" {
		opts.CacheDir = filepath.Join(o.repo, cfg.Sources.CacheDir)
	}
	switch {
	case o.offline && o.refresh:
		return nil, fmt.Errorf("--offline and --refresh are mutually exclusive")
	case o.offline:
		opts.Mode = source.Offline
	case o.refresh:
		opts.Mode = source.Refresh
	}
	for _, ov := range cfg.Sources.Overrides {
		opts.Overrides = append(opts.Overrides, source.Override{
			Kind: ov.Kind, Namespace: ov.Namespace, Name: ov.Name, Path: filepath.Join(o.repo, ov.Path),
		})
	}
	return source.New(opts)
}

func checkFlags(o *checkOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&o.repo, "repo", ".", "repository root")
	fs.StringVar(&o.cfgPath, "config", "", "config file (default <repo>/"+config.DefaultFile+")")
	fs.StringVar(&o.format, "format", "text", "output format: text, json, gitlab (Code Quality), sarif or github (workflow annotations)")
	fs.StringVar(&o.output, "output", "", "write the report to this file and print the text report to stdout")
	fs.StringVar(&o.base, "base", "", "Git ref to compare with (e.g. origin/main): only new findings fail the run, and the transition itself is analysed")
	fs.StringVar(&o.failOn, "fail-on", "error", "lowest severity that fails the run: error or warning")
	fs.BoolVar(&o.verbose, "v", false, "also list suggestions (info)")
	fs.BoolVar(&o.offline, "offline", false, "never use the network: external sources must already be cached")
	fs.BoolVar(&o.refresh, "refresh", false, "re-resolve floating refs (branches, semver ranges) instead of using the cache")
	fs.StringVar(&o.cacheDir, "cache-dir", "", "source cache (default: user cache directory, or sources.cacheDir)")
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

	resolver, err := newResolver(o, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	baseDir := ""
	if o.base != "" {
		dir, cleanup, err := checkoutBase(o.repo, o.base)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		defer cleanup()
		baseDir = dir
	}

	start := time.Now()
	results := make([]*lint.Result, len(entrypoints))
	errs := make([]error, len(entrypoints))
	var wg sync.WaitGroup
	for i, ep := range entrypoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree, err := render.Tree(context.Background(), o.repo, ep, cfg, resolver)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", ep, err)
				return
			}
			results[i] = lint.Run(tree, cfg)
			if baseDir == "" {
				return
			}
			// an entrypoint that does not exist at the base is simply new
			if st, err := os.Stat(filepath.Join(baseDir, ep)); err != nil || !st.IsDir() {
				return
			}
			baseTree, err := render.Tree(context.Background(), baseDir, ep, cfg, resolver)
			if err != nil {
				errs[i] = fmt.Errorf("%s at %s: %w", ep, o.base, err)
				return
			}
			lint.Compare(lint.Run(baseTree, cfg), results[i], cfg)
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
	out := io.Writer(os.Stdout)
	if o.output != "" {
		f, err := os.Create(o.output)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		defer f.Close()
		out = f
		// a CI job wants the machine-readable file and a readable log
		if o.format != "text" {
			report.Text(os.Stdout, sum, o.verbose)
		}
	}
	var werr error
	switch o.format {
	case "json":
		werr = report.JSON(out, sum)
	case "gitlab":
		werr = report.GitLab(out, sum, o.verbose)
	case "sarif":
		werr = report.SARIF(out, sum, o.verbose)
	case "github":
		report.GitHub(out, sum, o.verbose)
	case "text":
		report.Text(out, sum, o.verbose)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown format %q\n", o.format)
		return 2
	}
	if werr != nil {
		fmt.Fprintln(os.Stderr, "error:", werr)
		return 2
	}
	if sum.Failed(lint.Severity(o.failOn)) {
		return 1
	}
	return 0
}

// graphCmd prints the component graph: dependsOn solid, parent/child dotted,
// imports dashed, the critical path bold and cycles red.
func graphCmd(args []string) int {
	o := &checkOpts{}
	fs := checkFlags(o)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.format == "text" {
		o.format = "dot"
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
	if len(entrypoints) != 1 {
		fmt.Fprintln(os.Stderr, "error: graph draws one entrypoint at a time; name it")
		return 2
	}
	resolver, err := newResolver(o, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	tree, err := render.Tree(context.Background(), o.repo, entrypoints[0], cfg, resolver)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	switch o.format {
	case "dot":
		lint.WriteDOT(os.Stdout, tree, cfg)
	case "mermaid":
		lint.WriteMermaid(os.Stdout, tree, cfg)
	default:
		fmt.Fprintf(os.Stderr, "error: graph formats are dot and mermaid, not %q\n", o.format)
		return 2
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
