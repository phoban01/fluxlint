package lint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/lint"
)

// A rule nobody tests is a rule nobody can change safely. This does not prove a
// test is good, only that one exists: every rule ID must appear in a test file
// here or in the end-to-end suite.
func TestEveryRuleHasATest(t *testing.T) {
	var tests strings.Builder
	for _, pattern := range []string{"*_test.go", "../../e2e/*_test.go"} {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if filepath.Base(f) == "coverage_test.go" {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			tests.Write(b)
		}
	}
	for _, r := range lint.Rules {
		if !strings.Contains(tests.String(), `"`+r.ID+`"`) {
			t.Errorf("%s (%s) is not exercised by any test", r.ID, r.Name)
		}
	}
}

// Rule IDs and names are an interface: people write them in .fluxlint.yaml,
// in CI allow-lists and in code review. testdata/rules.frozen lists every
// rule that has shipped. A rule may be added, and then a line is added here;
// a line is never changed or removed. A rule that stops existing keeps its
// line and its catalogue entry, with Help saying what replaced it.
func TestRuleIDsAreFrozen(t *testing.T) {
	b, err := os.ReadFile("testdata/rules.frozen")
	if err != nil {
		t.Fatal(err)
	}
	frozen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		id, name, _ := strings.Cut(line, " ")
		frozen[id] = name
	}
	for id, name := range frozen {
		r, ok := lint.RuleByID(id)
		switch {
		case !ok:
			t.Errorf("%s (%s) has shipped and must stay in the catalogue", id, name)
		case r.Name != name:
			t.Errorf("%s was shipped as %q and is now %q: names do not change", id, name, r.Name)
		}
	}
	for _, r := range lint.Rules {
		if _, ok := frozen[r.ID]; !ok {
			t.Errorf("%s %s is new: add it to testdata/rules.frozen", r.ID, r.Name)
		}
	}
}
