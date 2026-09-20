package contract

import (
	"strings"
	"testing"
)

func TestLoadEveryField(t *testing.T) {
	c, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Requires.CRDs) != 2 || c.Requires.CRDs[0].Version != "v1" || c.Requires.CRDs[1].Version != "" {
		t.Errorf("crds = %+v", c.Requires.CRDs)
	}
	if len(c.Requires.Secrets) != 2 || c.Requires.Secrets[1].Namespace != "shared" || c.Requires.Secrets[0].Keys[1] != "SERVICE_TOKEN" {
		t.Errorf("secrets = %+v", c.Requires.Secrets)
	}
	if len(c.Requires.ConfigMaps) != 1 || c.Requires.ConfigMaps[0].Keys[0] != "region" {
		t.Errorf("configMaps = %+v", c.Requires.ConfigMaps)
	}
}

func TestParseRejectsWhatCannotBeChecked(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"misspelt section": {"requires:\n  crd:\n    - {group: a.example, kind: A}\n", "unknown field"},
		"misspelt field":   {"requires:\n  secrets:\n    - {name: s, key: [a]}\n", "unknown field"},
		"crd without kind": {"requires:\n  crds:\n    - {group: a.example}\n", "group and kind are required"},
		"unnamed secret":   {"requires:\n  secrets:\n    - {keys: [a]}\n", "name is required"},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
	if _, err := Load("testdata/absent.yaml"); err == nil {
		t.Error("a missing file is an error")
	}
}
