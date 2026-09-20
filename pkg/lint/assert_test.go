package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
)

func assertionRepo(t *testing.T) string {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "apps", "apps", "  postBuild:\n    substitute:\n      live: green\n      green_scale: \"0\"\n      blue_scale: \"2\"\n"))
	write(t, dir, "apps/all.yaml",
		strings.Replace(deployment("web-green", "default", 0, "containers:\n  - name: c\n    image: example.test/c:1\n"), "replicas: 0", "replicas: ${green_scale}", 1)+
			strings.Replace(deployment("web-blue", "default", 0, "containers:\n  - name: c\n    image: example.test/c:1\n"), "replicas: 0", "replicas: ${blue_scale}", 1))
	return dir
}

func TestAssertions(t *testing.T) {
	cfg := config.Default()
	cfg.Assertions = []config.Assertion{
		{
			Name:    "live colour serves traffic",
			Match:   config.Match{Kind: "Deployment", Name: "web-*"},
			Expr:    `!object.metadata.name.endsWith("-" + vars.live) || object.spec.replicas > 0`,
			Message: "the live colour is scaled to zero",
		},
		{Name: "images are pinned", Match: config.Match{Kind: "Deployment"},
			Expr: `object.spec.template.spec.containers.all(c, !c.image.endsWith(":latest"))`},
		{Name: "guards nothing", Match: config.Match{Kind: "StatefulSet"}, Expr: "true", MustMatch: true},
		{Name: "typo", Match: config.Match{Kind: "Deployment", Name: "web-blue"}, Expr: `object.spec.replicaz > 0`},
		{Name: "advice", Match: config.Match{Kind: "Deployment", Name: "web-blue"}, Expr: `object.spec.replicas >= 3`, Severity: "warning"},
	}
	r := analyseDir(t, assertionRepo(t), cfg)
	got := find(r, "FL-A001")
	text := messages(got)
	for _, want := range []string{
		"Deployment/default/web-green: live colour serves traffic: the live colour is scaled to zero",
		"guards nothing: no rendered object matches",
		"typo: could not evaluate", // a silent pass would hide the mistake
		"Deployment/default/web-blue: advice: assertion failed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if len(got) != 4 {
		t.Errorf("want 4 failures (blue passes the first, both pass the second), got %d:\n%s", len(got), text)
	}
	for _, f := range got {
		if strings.Contains(f.Message, "advice") && f.Severity != "warning" {
			t.Errorf("severity override ignored: %+v", f)
		}
	}
}

func TestAssertionThatDoesNotCompile(t *testing.T) {
	cfg := config.Default()
	cfg.Assertions = []config.Assertion{
		{Name: "broken", Expr: `object.spec.replicas >`},
		{Name: "not a bool", Expr: `"text"`},
	}
	r := analyseDir(t, assertionRepo(t), cfg)
	if got := find(r, "FL-A002"); len(got) != 2 {
		t.Fatalf("both assertions are invalid:\n%s", messages(r.Findings))
	}
}
