package lint

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

// locate finds the repository file (and line) to attach a finding to: the
// object's own manifest when it comes from the repository, otherwise the
// manifest of the nearest component that does — a finding inside a chart or an
// external repository points at the HelmRelease / Kustomization that pulls it in.
func (r *run) locate(c *model.Component, o model.Object) (string, int) {
	if o != nil {
		owners := r.ix.Owner[o.ID()]
		if c != nil {
			owners = append([]*model.Component{c}, owners...)
		}
		for _, owner := range owners {
			if file := owner.Origins[o.ID()]; file != "" {
				return file, r.lineOf(file, o)
			}
		}
	}
	for x := c; x != nil && x.Parent != nil; x = x.Parent {
		if file := x.Parent.Origins[x.Spec.ID()]; file != "" {
			return file, r.lineOf(file, x.Spec)
		}
	}
	return "", 0
}

// lineOf returns the line of the YAML document in file that declares o, or 1.
func (r *run) lineOf(file string, o model.Object) int {
	lines, ok := r.files[file]
	if !ok {
		b, _ := os.ReadFile(filepath.Join(r.ix.Tree.RepoRoot, file))
		lines = strings.Split(string(b), "\n")
		r.files[file] = lines
	}
	// the rendered name may carry a prefix/suffix the manifest does not
	matchesName := func(l string) bool {
		n, ok := strings.CutPrefix(strings.TrimSpace(l), "name:")
		n = strings.Trim(strings.TrimSpace(n), `"'`)
		return ok && n != "" && strings.Contains(o.Name(), n)
	}
	kindLine, named := 0, false
	flush := func() int {
		if kindLine > 0 && named {
			return kindLine
		}
		return 0
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "---") {
			if at := flush(); at > 0 {
				return at
			}
			kindLine, named = 0, false
			continue
		}
		if strings.HasPrefix(l, "kind:") && strings.TrimSpace(strings.TrimPrefix(l, "kind:")) == o.Kind() {
			kindLine = i + 1
		}
		if strings.HasPrefix(l, "  name:") && matchesName(l) {
			named = true
		}
	}
	if at := flush(); at > 0 {
		return at
	}
	return 1
}
