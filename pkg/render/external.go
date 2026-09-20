package render

import (
	"os"
	"path/filepath"

	"github.com/phoban01/fluxlint/pkg/model"
	"golang.org/x/mod/modfile"
	"sigs.k8s.io/yaml"
)

// ContractFile is looked up in the directory a Kustomization renders, then at
// the root of its source.
const ContractFile = "fluxlint-contract.yaml"

// inspectSource reads what an external source says about itself: its Go
// module (for version-skew checks) and its contract, if it ships one.
func inspectSource(c *model.Component) {
	if c.SourceRoot == "" || c.IsHelmRelease() {
		return
	}
	if b, err := os.ReadFile(filepath.Join(c.SourceRoot, "go.mod")); err == nil {
		if f, err := modfile.ParseLax("go.mod", b, nil); err == nil && f.Module != nil {
			c.GoModule = f.Module.Mod.Path
			c.GoRequires = map[string]string{}
			for _, r := range f.Require {
				c.GoRequires[r.Mod.Path] = r.Mod.Version
			}
		}
	}
	for _, p := range []string{filepath.Join(c.SourceRoot, c.Path, ContractFile), filepath.Join(c.SourceRoot, ContractFile)} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var contract model.Contract
		if err := yaml.UnmarshalStrict(b, &contract); err != nil {
			c.RenderNotes = append(c.RenderNotes, ContractFile+" is invalid and was ignored: "+err.Error())
		} else {
			c.Contract = &contract
		}
		return
	}
}
