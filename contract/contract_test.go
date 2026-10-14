package contract

import (
	"os"
	"path/filepath"
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
	if len(c.Requires.Containers) != 1 {
		t.Fatalf("containers = %+v", c.Requires.Containers)
	}
	ctr := c.Requires.Containers[0]
	if ctr.Name != "manager" || ctr.Image != "registry.example.test/platform/operator" || len(ctr.Env) != 2 ||
		len(ctr.Args.Required) != 1 || len(ctr.Args.Accepted) != 3 {
		t.Errorf("containers[0] = %+v", ctr)
	}
}

func TestFlagName(t *testing.T) {
	for token, want := range map[string]string{
		"--namespace": "namespace", "-namespace": "namespace", "namespace": "namespace",
		"--namespace=x": "namespace", "--leader-elect=true": "leader-elect", "-v=2": "v", "value": "value",
	} {
		if got := FlagName(token); got != want {
			t.Errorf("FlagName(%q) = %q, want %q", token, got, want)
		}
	}
}

// testdata/invalid holds one file per rule. Each starts with "# want: " and
// the error it must produce. fluxlint runs the same files against its own
// copy of the rules, so a rule added to one side and not the other fails.
func TestParseRejectsWhatCannotBeChecked(t *testing.T) {
	files, err := filepath.Glob("testdata/invalid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := strings.Cut(string(b), "\n")
		want, ok := strings.CutPrefix(first, "# want: ")
		if !ok {
			t.Fatalf("%s: first line must be '# want: <error>'", f)
		}
		if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", f, err, want)
		}
	}
	if _, err := Load("testdata/absent.yaml"); err == nil {
		t.Error("a missing file is an error")
	}
}
