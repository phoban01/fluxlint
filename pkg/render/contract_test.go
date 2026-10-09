package render

import (
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

func mustContainer(y string) model.ContractContainer {
	var c model.ContractContainer
	if err := yaml.UnmarshalStrict([]byte(y), &c); err != nil {
		panic(err)
	}
	return c
}

func TestReadContractDropsEntriesThatNameNoContainer(t *testing.T) {
	c, notes := readContract([]byte(`requires:
  containers:
    - env: [A]
    - name: manager
      env: [B]
    - image: registry.example.test/*
      args: {required: [--x]}
`), "fluxlint-contract.yaml")
	if c == nil || len(c.Requires.Containers) != 2 || c.Requires.Containers[0].Name != "manager" || c.Requires.Containers[1].Image == "" {
		t.Fatalf("kept %+v", c)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "requires.containers[0] names no container") {
		t.Errorf("notes = %q", notes)
	}

	if c, notes := readContract([]byte("requires:\n  containers:\n    - name: m\n      args: {required: [--a], accepted: [--b]}\n"), "x"); c != nil || len(notes) != 1 || !strings.Contains(notes[0], "not in args.accepted") {
		t.Errorf("an invalid contract is dropped with a note: %+v %q", c, notes)
	}
}

func TestPlacedOnImageBindsUnselectedEntriesToTheImage(t *testing.T) {
	found, _ := readContract([]byte("requires:\n  secrets:\n    - {name: creds}\n"), "x")
	found.Requires.Containers = append(found.Requires.Containers,
		mustContainer("env: [A]"),
		mustContainer("name: sidecar\nenv: [B]"))
	placed := placedOnImage(found, "registry.example.test/op:v1", []string{"a", "b"})
	if len(placed.Requires.Secrets) != 2 || placed.Requires.Secrets[0].Namespace != "a" || placed.Requires.Secrets[1].Namespace != "b" {
		t.Errorf("secrets = %+v", placed.Requires.Secrets)
	}
	if got := placed.Requires.Containers; len(got) != 2 || got[0].Image != "registry.example.test/op:v1" || got[1].Image != "" || got[1].Name != "sidecar" {
		t.Errorf("containers = %+v", got)
	}
	if found.Requires.Containers[0].Image != "" {
		t.Error("the fetched contract was changed in place; it may be cached")
	}
}
