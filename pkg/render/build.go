package render

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/provider"
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
	// Ignored reports paths the source leaves out of its artifact (see
	// ignoreFilter). It narrows the generated resource list; nil ignores nothing.
	Ignored func(path string, isDir bool) bool
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
//
// The second result gives, for each object, the absolute path of the file it
// came from ("" when unknown, e.g. generated objects without a source file).
func build(dir string, ov overlay) ([]model.Object, []string, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("path %q is not a directory", dir)
	}

	// Always build through a wrapper: it carries the Flux overlay and turns on
	// kustomize's origin annotations, which is how findings get a file.
	target, err := os.MkdirTemp("", "fluxlint-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(target)
	if target, err = filepath.EvalSymlinks(target); err != nil {
		return nil, nil, err
	}
	if err := writeWrapper(target, dir, ov); err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	out := make([]model.Object, 0, rm.Size())
	origins := make([]string, 0, rm.Size())
	for _, r := range rm.Resources() {
		m, err := r.Map()
		if err != nil {
			return nil, nil, err
		}
		o := model.Object(m)
		origins = append(origins, takeOrigin(o, target))
		out = append(out, o)
	}
	return out, origins, nil
}

const originAnnotation = "config.kubernetes.io/origin"

// takeOrigin removes kustomize's origin annotation from o and returns the
// absolute path it names.
func takeOrigin(o model.Object, base string) string {
	ann, _ := model.Get(o, "metadata", "annotations").(map[string]any)
	raw, _ := ann[originAnnotation].(string)
	if raw == "" {
		return ""
	}
	delete(ann, originAnnotation)
	if len(ann) == 0 {
		delete(o["metadata"].(map[string]any), "annotations")
	}
	var origin struct {
		Path         string `json:"path"`
		Repo         string `json:"repo"`
		ConfiguredIn string `json:"configuredIn"`
	}
	if err := yaml.Unmarshal([]byte(raw), &origin); err != nil || origin.Repo != "" {
		return ""
	}
	p := origin.Path
	if p == "" {
		p = origin.ConfiguredIn
	}
	if p == "" {
		return ""
	}
	return filepath.Clean(filepath.Join(base, p))
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
		found, err := generateResources(dir, ov.Ignored)
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
	k["buildMetadata"] = []string{"originAnnotations"}
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

// generateResources mirrors the scan in Flux's kustomization generator
// (fluxcd/pkg/kustomize): every .yaml/.yml file under dir is a resource and
// must decode as Kubernetes objects — a stray values file fails the build, as
// it does in the cluster — except that a sub-directory with its own
// kustomization is included as a directory and not descended into. Files that
// source-controller leaves out of every artifact are skipped.
func generateResources(dir string, ignored func(string, bool) bool) ([]string, error) {
	rf := provider.NewDefaultDepProvider().GetResourceFactory()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) (walkErr error) {
		if err != nil {
			return err
		}
		if p != dir && ignored != nil && ignored(p, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && excludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			if p != dir && hasKustomization(p) {
				out = append(out, p)
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); (ext != ".yaml" && ext != ".yml") || excludedFiles[d.Name()] {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		defer func() { // kustomize's parser can panic on malformed input
			if r := recover(); r != nil {
				walkErr = fmt.Errorf("recovered from panic while parsing YAML file %s: %v", filepath.Base(p), r)
			}
		}()
		if _, err := rf.SliceFromBytes(content); err != nil {
			return fmt.Errorf("failed to decode Kubernetes YAML from %s: %w", p, err)
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// source-controller's default exclusions that can match a YAML file or a
// directory containing one.
var (
	excludedDirs  = map[string]bool{".git": true, ".github": true, ".circleci": true}
	excludedFiles = map[string]bool{
		".travis.yml": true, ".gitlab-ci.yml": true, "appveyor.yml": true, ".drone.yml": true,
		"cloudbuild.yaml": true, "codeship-services.yml": true, "codeship-steps.yml": true,
	}
)
