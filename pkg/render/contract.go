package render

import (
	"fmt"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

// readContract parses a contract found in a Git source or a chart. where
// names the place for notes. An entry under requires.containers that names no
// container is dropped with a note: only a contract attached to an image can
// leave the container implicit, and holding every container of a component
// to it would hold sidecars to it too.
func readContract(b []byte, where string) (*model.Contract, []string) {
	var c model.Contract
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, []string{fmt.Sprintf("%s is invalid and was ignored: %v", where, err)}
	}
	if err := c.Validate(); err != nil {
		return nil, []string{fmt.Sprintf("%s is invalid and was ignored: %v", where, err)}
	}
	var notes []string
	kept := c.Requires.Containers[:0]
	for i, ctr := range c.Requires.Containers {
		if !ctr.Selected() {
			notes = append(notes, fmt.Sprintf("%s: requires.containers[%d] names no container (set name or image) and was ignored; only a contract attached to an image can leave it out", where, i))
			continue
		}
		kept = append(kept, ctr)
	}
	c.Requires.Containers = kept
	return &c, notes
}

// mergeContract appends src's requirements to dst. dst must be a copy the
// caller owns: contracts are shared through the build cache.
func mergeContract(dst, src *model.Contract) {
	dst.Requires.CRDs = append(dst.Requires.CRDs, src.Requires.CRDs...)
	dst.Requires.Secrets = append(dst.Requires.Secrets, src.Requires.Secrets...)
	dst.Requires.ConfigMaps = append(dst.Requires.ConfigMaps, src.Requires.ConfigMaps...)
	dst.Requires.Containers = append(dst.Requires.Containers, src.Requires.Containers...)
}
