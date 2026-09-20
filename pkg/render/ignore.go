package render

import (
	"path/filepath"
	"strings"

	"github.com/fluxcd/pkg/sourceignore"
	"github.com/phoban01/fluxlint/pkg/model"
)

// ignoreFilter reports whether source-controller leaves a path out of the
// artifact it builds from root: its default exclusions, every .sourceignore
// file in the tree, then the source's spec.ignore. Files that never reach the
// artifact are never seen by kustomize-controller's manifest scan.
func ignoreFilter(root string, src model.Object) func(path string, isDir bool) bool {
	ps, err := sourceignore.LoadIgnorePatterns(root, nil)
	if err != nil {
		ps = nil
	}
	if ignore, ok := model.Get(src, "spec", "ignore").(string); ok {
		ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(ignore), nil)...)
	}
	m := sourceignore.NewDefaultMatcher(ps, nil)
	return func(path string, isDir bool) bool {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return false
		}
		return m.Match(strings.Split(filepath.ToSlash(rel), "/"), isDir)
	}
}
