package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

// The contract format is defined twice: here, and in the contract module that
// operators import. Both parse the same file strictly, so a field added to one
// and not the other fails a test.
func TestContractFormatMatchesTheLibrary(t *testing.T) {
	b, err := os.ReadFile("../../contract/testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c model.Contract
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		t.Fatalf("fluxlint cannot read a contract the library accepts: %v", err)
	}
	if len(c.Requires.CRDs) != 2 || len(c.Requires.Secrets) != 2 || len(c.Requires.ConfigMaps) != 1 || c.Requires.Secrets[1].Namespace != "shared" {
		t.Errorf("parsed %+v", c.Requires)
	}
	if len(c.Requires.Containers) != 1 || c.Requires.Containers[0].Name != "manager" || len(c.Requires.Containers[0].Env) != 2 ||
		len(c.Requires.Containers[0].Args.Required) != 1 || len(c.Requires.Containers[0].Args.Accepted) != 3 {
		t.Errorf("parsed containers %+v", c.Requires.Containers)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// The library's invalid fixtures must be rejected here too, with the same
// words: a validation rule that exists on one side only is a contract the
// author's tests reject and fluxlint accepts, or the reverse.
func TestContractRulesMatchTheLibrary(t *testing.T) {
	files, err := filepath.Glob("../../contract/testdata/invalid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := strings.Cut(string(b), "\n")
		want := strings.TrimPrefix(first, "# want: ")
		if _, err := parseContract(b); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", filepath.Base(f), err, want)
		}
	}
}
