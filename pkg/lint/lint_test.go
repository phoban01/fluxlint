package lint_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/render"
)

func analyse(t *testing.T, fixture string, cfg *config.Config) *lint.Result {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	tree, err := render.Tree(filepath.Join("testdata", fixture), "clusters/prod", cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return lint.Run(tree, cfg)
}

// problems returns the sorted rule IDs of every error and warning.
func problems(r *lint.Result) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity != lint.Info {
			out = append(out, f.Rule)
		}
	}
	sort.Strings(out)
	return out
}

func find(r *lint.Result, rule string) []lint.Finding {
	var out []lint.Finding
	for _, f := range r.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func component(t *testing.T, r *lint.Result, key string) *model.Component {
	t.Helper()
	c := r.Tree.ByKey[key]
	if c == nil {
		t.Fatalf("component %s not rendered", key)
	}
	return c
}

func TestCleanRepositoryHasNoProblems(t *testing.T) {
	r := analyse(t, "clean", nil)
	if got := problems(r); len(got) != 0 {
		t.Fatalf("expected no problems, got %v: %+v", got, r.Findings)
	}
}

func TestDeadlockThroughHelmSource(t *testing.T) {
	r := analyse(t, "deadlock", nil)
	got := find(r, "FL-G002")
	if len(got) != 1 {
		t.Fatalf("expected one deadlock, got %+v", r.Findings)
	}
	detail := strings.Join(got[0].Detail, "\n")
	for _, want := range []string{"dependsOn", "HelmRepository/flux-system/podinfo"} {
		if !strings.Contains(detail, want) {
			t.Errorf("cycle explanation lacks %q:\n%s", want, detail)
		}
	}
}

func TestDeadlockParentNeedsNamespaceFromItsChild(t *testing.T) {
	r := analyse(t, "nested", nil)
	if got := find(r, "FL-G002"); len(got) != 1 {
		t.Fatalf("expected one deadlock, got %+v", r.Findings)
	}
}

func TestBrokenReferences(t *testing.T) {
	r := analyse(t, "broken", nil)
	want := []string{
		"FL-G001", // dependsOn does-not-exist
		"FL-G001", // HelmRelease -> undefined HelmRepository
		"FL-G003", // Namespace/shared rendered twice
		"FL-G004", // ConfigMap into namespace "nowhere"
	}
	if got := problems(r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("problems = %v, want %v\n%+v", got, want, r.Findings)
	}
}

func TestExternalNamespaceSilencesMissingNamespace(t *testing.T) {
	cfg := config.Default()
	cfg.Externals.Namespaces = []string{"nowhere"}
	if got := find(analyse(t, "broken", cfg), "FL-G004"); len(got) != 0 {
		t.Fatalf("external namespace still reported: %+v", got)
	}
}

func TestSubstitution(t *testing.T) {
	r := analyse(t, "subst", nil)

	undefined := find(r, "FL-S001")
	if len(undefined) != 1 || !strings.Contains(undefined[0].Message, "${missing}") {
		t.Errorf("want exactly ${missing} undefined, got %+v", undefined)
	}
	missing := find(r, "FL-S002")
	if len(missing) != 1 || !strings.Contains(missing[0].Message, "not-in-git") {
		t.Errorf("want only not-in-git reported (maybe is optional), got %+v", missing)
	}

	var out, script model.Object
	for _, o := range component(t, r, "flux-system/apps").Objects {
		switch o.Name() {
		case "out":
			out = o
		case "script":
			script = o
		}
	}
	for key, want := range map[string]string{"a": "from-inline", "b": "eu-west-1", "c": "", "d": "safe"} {
		if got := model.Str(out, "data", key); got != want {
			t.Errorf("data.%s = %q, want %q", key, got, want)
		}
	}
	if got := model.Str(script, "data", "run.sh"); got != "echo ${HOME}" {
		t.Errorf("substitute: disabled was not honoured: %q", got)
	}
}

func TestExternalSubstitutionSource(t *testing.T) {
	cfg := config.Default()
	cfg.Externals.Substitutions = []config.ExternalSubstitution{{Kind: "ConfigMap", Name: "not-in-git", Variables: []string{"missing"}}}
	r := analyse(t, "subst", cfg)
	if got := problems(r); len(got) != 0 {
		t.Fatalf("declared external should satisfy both findings, got %v", got)
	}
}

func TestGeneratedKustomizationAndFluxOverlay(t *testing.T) {
	r := analyse(t, "overlay", nil)
	if got := problems(r); len(got) != 0 {
		t.Fatalf("unexpected problems %v: %+v", got, r.Findings)
	}
	names := map[string]model.Object{}
	for _, o := range component(t, r, "flux-system/apps").Objects {
		names[o.Name()] = o
	}
	if len(names) != 2 {
		t.Fatalf("want settings-green and second-green only, got %v", names)
	}
	settings, ok := names["settings-green"]
	if !ok {
		t.Fatalf("nameSuffix not applied: %v", names)
	}
	if settings.Namespace() != "overlay-ns" {
		t.Errorf("targetNamespace not applied: %q", settings.Namespace())
	}
	if got := model.Str(settings, "data", "value"); got != "patched" {
		t.Errorf("spec.patches not applied: %q", got)
	}
	if _, ok := names["second-green"]; !ok {
		t.Errorf("sub-directory with its own kustomization was not included: %v", names)
	}
}

func TestTiming(t *testing.T) {
	r := analyse(t, "timing", nil)
	if r.Timing == nil {
		t.Fatal("no timing result")
	}
	// a waits 5m, b polls a (30s) then waits 2m, c polls b (30s)
	if want := 8 * time.Minute; r.Timing.Bound != want {
		t.Errorf("bound = %v, want %v", r.Timing.Bound, want)
	}
	last := r.Timing.CriticalPath[len(r.Timing.CriticalPath)-1]
	if !strings.Contains(last.Event, "flux-system/c") {
		t.Errorf("critical path should end at c, ends at %s", last.Event)
	}

	redundant := find(r, "FL-T005")
	if len(redundant) != 1 || redundant[0].Component != "flux-system/c" {
		t.Errorf("c -> a is implied by c -> b -> a: %+v", redundant)
	}
	unjustified := find(r, "FL-T004")
	if len(unjustified) != 1 || !strings.Contains(unjustified[0].Message, "flux-system/b") {
		t.Errorf("only c -> b is unjustified (b -> a is justified by namespace team-a): %+v", unjustified)
	}

	cfg := config.Default()
	cfg.Timing.MaxBootstrapBound.Duration = 5 * time.Minute
	if got := find(analyse(t, "timing", cfg), "FL-T100"); len(got) != 1 {
		t.Errorf("budget of 5m should fail an 8m bound: %+v", got)
	}
}
