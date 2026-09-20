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
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
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

// ignored returns the filter for a source rooted at root, building it once:
// loading the patterns walks the whole tree.
func (r *renderer) ignored(root, source string) func(string, bool) bool {
	key := root + "\x00" + source
	if f, ok := r.ignores[key]; ok {
		return f
	}
	if r.ignores == nil {
		r.ignores = map[string]func(string, bool) bool{}
	}
	f := ignoreFilter(root, r.sources[source])
	r.ignores[key] = f
	return f
}
