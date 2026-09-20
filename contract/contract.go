// Package contract reads fluxlint-contract.yaml: what a controller declares it
// cannot run without. fluxlint holds every repository that deploys the
// controller to the contract. Package contracttest holds the controller itself
// to it, so the contract cannot drift from the code.
//
// This is its own small module on purpose. Importing it does not pull
// fluxlint's dependencies, or the versions it pins, into your operator.
package contract

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// FileName is where fluxlint looks: next to the kustomization a Flux
// Kustomization renders, at the root of the repository, or beside Chart.yaml.
const FileName = "fluxlint-contract.yaml"

// Contract is the content of fluxlint-contract.yaml.
type Contract struct {
	Requires Requires `json:"requires"`
}

type Requires struct {
	// CRDs the controller watches and cannot start without.
	CRDs []CRD `json:"crds,omitempty"`
	// Secrets and ConfigMaps the controller reads at runtime.
	Secrets    []Config `json:"secrets,omitempty"`
	ConfigMaps []Config `json:"configMaps,omitempty"`
}

type CRD struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
	// Version is optional. When set, the installed CRD must serve it.
	Version string `json:"version,omitempty"`
}

type Config struct {
	Name string `json:"name"`
	// Namespace is optional and defaults to where the controller runs.
	Namespace string `json:"namespace,omitempty"`
	// Keys the controller reads. Each must exist.
	Keys []string `json:"keys,omitempty"`
}

// Load reads and validates the contract at path.
func Load(path string) (*Contract, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse is strict: an unknown field is an error, because a misspelt
// requirement would otherwise be a requirement nobody checks.
func Parse(b []byte) (*Contract, error) {
	var c Contract
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// Validate reports the first entry that cannot be checked.
func (c *Contract) Validate() error {
	for i, crd := range c.Requires.CRDs {
		if crd.Group == "" || crd.Kind == "" {
			return fmt.Errorf("requires.crds[%d]: group and kind are required", i)
		}
	}
	for kind, list := range map[string][]Config{"secrets": c.Requires.Secrets, "configMaps": c.Requires.ConfigMaps} {
		for i, cfg := range list {
			if cfg.Name == "" {
				return fmt.Errorf("requires.%s[%d]: name is required", kind, i)
			}
		}
	}
	return nil
}
