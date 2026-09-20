package render

import (
	"os"
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
}
