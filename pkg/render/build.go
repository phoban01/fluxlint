package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// kustomize keeps process-global state; kustomize-controller serialises builds
// for the same reason.
var buildMu sync.Mutex

var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

func hasKustomization(dir string) bool {
	for _, f := range kustomizationFiles {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// overlay is what a Flux Kustomization layers on top of the directory it
// points at. kustomize-controller edits the kustomization.yaml in place; we
// wrap the directory instead so the working tree is never modified.
type overlay struct {
	Patches         []any
	Images          []any
	Components      []string
	TargetNamespace string
	NamePrefix      string
	NameSuffix      string
}

func (o overlay) empty() bool {
	return len(o.Patches) == 0 && len(o.Images) == 0 && len(o.Components) == 0 &&
		o.TargetNamespace == "" && o.NamePrefix == "" && o.NameSuffix == ""
}

func overlayFromSpec(spec model.Object) overlay {
	o := overlay{
		Patches:         model.List(spec, "spec", "patches"),
		Images:          model.List(spec, "spec", "images"),
		TargetNamespace: model.Str(spec, "spec", "targetNamespace"),
		NamePrefix:      model.Str(spec, "spec", "namePrefix"),
		NameSuffix:      model.Str(spec, "spec", "nameSuffix"),
	}
	for _, c := range model.List(spec, "spec", "components") {
		if s, ok := c.(string); ok {
			o.Components = append(o.Components, s)
		}
	}
	return o
}

// build renders dir the way kustomize-controller would: a kustomization.yaml is
// generated when the directory has none, and the Flux-level overlay is applied.
func build(dir string, ov overlay) ([]model.Object, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", dir)
	}

	target := dir
	if !hasKustomization(dir) || !ov.empty() {
		tmp, err := os.MkdirTemp("", "fluxlint-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
			return nil, err
		}
		if err := writeWrapper(tmp, dir, ov); err != nil {
			return nil, err
		}
		target = tmp
	}

	buildMu.Lock()
	defer buildMu.Unlock()
	// kustomize prints deprecation notices straight to os.Stderr. They concern
	// the authors of whatever is being rendered (often a third-party repository),
	// not the convergence question, so keep them out of the report.
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		stderr := os.Stderr
		os.Stderr = devnull
		defer func() { os.Stderr = stderr; devnull.Close() }()
	}
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone
	rm, err := krusty.MakeKustomizer(opts).Run(filesys.MakeFsOnDisk(), target)
	if err != nil {
		return nil, err
	}
	out := make([]model.Object, 0, rm.Size())
	for _, r := range rm.Resources() {
		m, err := r.Map()
		if err != nil {
			return nil, err
		}
		out = append(out, model.Object(m))
	}
	return out, nil
}

func writeWrapper(tmp, dir string, ov overlay) error {
	rel := func(p string) (string, error) { return filepath.Rel(tmp, p) }
	k := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
	}
	var resources []string
	if hasKustomization(dir) {
		r, err := rel(dir)
		if err != nil {
			return err
		}
		resources = []string{r}
	} else {
		found, err := generateResources(dir)
		if err != nil {
			return err
		}
		for _, f := range found {
			r, err := rel(f)
			if err != nil {
				return err
			}
			resources = append(resources, r)
		}
	}
	k["resources"] = resources
	if len(ov.Patches) > 0 {
		k["patches"] = ov.Patches
	}
	if len(ov.Images) > 0 {
		k["images"] = ov.Images
	}
	if ov.TargetNamespace != "" {
		k["namespace"] = ov.TargetNamespace
	}
	if ov.NamePrefix != "" {
		k["namePrefix"] = ov.NamePrefix
	}
	if ov.NameSuffix != "" {
		k["nameSuffix"] = ov.NameSuffix
	}
	if len(ov.Components) > 0 {
		var comps []string
		for _, c := range ov.Components {
			r, err := rel(filepath.Join(dir, c))
			if err != nil {
				return err
			}
			comps = append(comps, r)
		}
		k["components"] = comps
	}
	b, err := yaml.Marshal(k)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(tmp, "kustomization.yaml"), b, 0o600)
}

// generateResources mirrors Flux's kustomization generator: every manifest file
// under dir is a resource, except that a sub-directory with its own
// kustomization is included as a directory and not descended into.
func generateResources(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && hasKustomization(p) {
				out = append(out, p)
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(p) {
		case ".yaml", ".yml":
		default:
			return nil
		}
		if ok, err := looksLikeManifest(p); err != nil || !ok {
			return err
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

func looksLikeManifest(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, doc := range bytes.Split(b, []byte("\n---")) {
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return true, nil // include it so the build reports the syntax error, as Flux would
		}
		if m == nil {
			continue
		}
		_, hasKind := m["kind"]
		_, hasAPI := m["apiVersion"]
		return hasKind && hasAPI, nil
	}
	return false, nil
}
