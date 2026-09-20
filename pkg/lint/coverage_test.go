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
