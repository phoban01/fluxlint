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
	"strings"

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
	// Containers is what must reach the process itself: environment
	// variables and command-line flags.
	Containers []Container `json:"containers,omitempty"`
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

// Container is what one container's process needs on its command line and in
// its environment. Name and Image select the container; Image uses the
// pattern syntax of fluxlint's contracts.images ("*" matches anything) and
// is compared without the tag or digest. A contract attached to an image may
// omit both: it applies to the containers that run that image. A contract in
// a Git repository or chart must set at least one, or fluxlint ignores the
// entry, because applying it to every container would also hold sidecars to
// it.
type Container struct {
	Name  string `json:"name,omitempty"`
	Image string `json:"image,omitempty"`
	// Env lists environment variables the process reads and does not start
	// without. A variable set to the empty string counts as unset.
	Env []string `json:"env,omitempty"`
	// Args describes the command-line flags.
	Args Args `json:"args,omitempty"`
}

// Args describes a container's command-line flags. Names are compared
// without their leading dashes, so "--namespace" and "namespace" are the
// same flag.
type Args struct {
	// Required flags must be passed.
	Required []string `json:"required,omitempty"`
	// Accepted is the closed set of flags this version parses. When set,
	// any other flag on the command line is an error: the binary exits on
	// an unknown flag. Required must be a subset.
	Accepted []string `json:"accepted,omitempty"`
}

// FlagName is the name under which a flag is compared: the token up to the
// first "=", without leading dashes.
func FlagName(token string) string {
	name, _, _ := strings.Cut(token, "=")
	return strings.TrimLeft(name, "-")
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
	// the same rules, in the same words, as fluxlint's model.Contract.Validate;
	// testdata/invalid holds a case per rule and both modules run them
	for i, ctr := range c.Requires.Containers {
		for j, name := range ctr.Env {
			if name == "" {
				return fmt.Errorf("requires.containers[%d].env[%d]: a name is required", i, j)
			}
		}
		accepted := map[string]bool{}
		for j, f := range ctr.Args.Accepted {
			if FlagName(f) == "" {
				return fmt.Errorf("requires.containers[%d].args.accepted[%d]: a flag name is required", i, j)
			}
			accepted[FlagName(f)] = true
		}
		for j, f := range ctr.Args.Required {
			if FlagName(f) == "" {
				return fmt.Errorf("requires.containers[%d].args.required[%d]: a flag name is required", i, j)
			}
			if len(accepted) > 0 && !accepted[FlagName(f)] {
				return fmt.Errorf("requires.containers[%d].args.required[%d]: %s is not in args.accepted", i, j, f)
			}
		}
	}
	return nil
}
