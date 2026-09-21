package render

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"helm.sh/helm/v3/pkg/strvals"
	"sigs.k8s.io/yaml"
)

// helmSpec is what helm-controller derives from a HelmRelease before installing.
type helmSpec struct {
	ReleaseName, Namespace string
	Values                 map[string]any
	PostRenderers          []overlay // spec.postRenderers[].kustomize, in order
	Notes                  []string
	// APIVersions is what the target cluster serves, for
	// .Capabilities.APIVersions. Empty: Helm's built-in list.
	APIVersions []string `json:"-"`
}

// helmSpecOf follows helm-controller: the release lives in
// spec.targetNamespace (default: the HelmRelease's namespace) and is named
// spec.releaseName, defaulting to [targetNamespace-]name. valuesFrom entries
// are merged in order and inline spec.values win.
func (r *renderer) helmSpecOf(hr model.Object) helmSpec {
	s := helmSpec{Namespace: hr.Namespace(), ReleaseName: hr.Name(), Values: map[string]any{}}
	if tn := model.Str(hr, "spec", "targetNamespace"); tn != "" {
		s.Namespace = tn
		s.ReleaseName = tn + "-" + hr.Name()
	}
	if rn := model.Str(hr, "spec", "releaseName"); rn != "" {
		s.ReleaseName = rn
	}
	for _, vf := range model.List(hr, "spec", "valuesFrom") {
		kind, name := model.Str(vf, "kind"), model.Str(vf, "name")
		key := model.Str(vf, "valuesKey")
		if key == "" {
			key = "values.yaml"
		}
		data, ok := r.data[kind+"/"+hr.Namespace()+"/"+name]
		if !ok {
			if !model.Bool(vf, "optional") {
				s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s is not rendered by any component; its values are left at chart defaults", kind, name))
			}
			continue
		}
		if tp := model.Str(vf, "targetPath"); tp != "" {
			v, ok := data[key]
			if !ok {
				if !model.Bool(vf, "optional") {
					s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s has no key %s; %s is left at the chart default", kind, name, key, tp))
				}
				continue
			}
			if err := setPathValue(s.Values, tp, v); err != nil {
				s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s: cannot set %s: %v", kind, name, tp, err))
			}
			continue
		}
		var vals map[string]any
		if err := yaml.Unmarshal([]byte(data[key]), &vals); err != nil {
			s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s key %s is not valid YAML: %v", kind, name, key, err))
			continue
		}
		s.Values = mergeValues(s.Values, vals)
	}
	if inline, ok := model.Get(hr, "spec", "values").(map[string]any); ok {
		s.Values = mergeValues(s.Values, inline)
	}
	for _, pr := range model.List(hr, "spec", "postRenderers") {
		if k := model.Get(pr, "kustomize"); k != nil {
			s.PostRenderers = append(s.PostRenderers, overlay{Patches: model.List(k, "patches"), Images: model.List(k, "images")})
		}
	}
	return s
}

// setPathValue is helm-controller's ReplacePathValue: the value is set with
// --set semantics, or --set-string ones when it is wrapped in quotes.
func setPathValue(values map[string]any, path, value string) error {
	const single, double = "'", "\""
	quoted := func(q string) bool {
		return len(value) > 1 && strings.HasPrefix(value, q) && strings.HasSuffix(value, q)
	}
	if quoted(single) || quoted(double) {
		return strvals.ParseIntoString(path+"="+value[1:len(value)-1], values)
	}
	return strvals.ParseInto(path+"="+strings.ReplaceAll(value, ",", "\\,"), values)
}

func mergeValues(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		if vm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeValues(bm, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// chartContract reads the contract a chart ships beside its Chart.yaml. A
// chart's subcharts are part of the same release, so theirs are merged in.
func chartContract(ch *chart.Chart) (*model.Contract, []string) {
	var out *model.Contract
	var notes []string
	for _, f := range ch.Files {
		if f.Name != ContractFile {
			continue
		}
		var c model.Contract
		if err := yaml.UnmarshalStrict(f.Data, &c); err != nil {
			notes = append(notes, fmt.Sprintf("%s in chart %s is invalid and was ignored: %v", ContractFile, ch.Name(), err))
			continue
		}
		out = &c
	}
	for _, dep := range ch.Dependencies() {
		sub, n := chartContract(dep)
		notes = append(notes, n...)
		if sub == nil {
			continue
		}
		if out == nil {
			out = &model.Contract{}
		}
		out.Requires.CRDs = append(out.Requires.CRDs, sub.Requires.CRDs...)
		out.Requires.Secrets = append(out.Requires.Secrets, sub.Requires.Secrets...)
		out.Requires.ConfigMaps = append(out.Requires.ConfigMaps, sub.Requires.ConfigMaps...)
	}
	return out, notes
}

// helmTemplate is `helm template --include-crds`, in process.
func helmTemplate(ch *chart.Chart, s helmSpec, kubeVersion string) ([]model.Object, []string, error) {
	manifest, err := helmManifest(ch, s, kubeVersion)
	if err != nil {
		return nil, nil, err
	}
	objs, err := helmObjects(ch, s, manifest)
	if err != nil {
		return nil, nil, err
	}
	return objs, unstableObjects(ch, s, kubeVersion, manifest), nil
}

// helmManifest renders the release, install and upgrade hooks included.
func helmManifest(ch *chart.Chart, s helmSpec, kubeVersion string) (string, error) {
	cfg := &action.Configuration{Log: func(string, ...any) {}}
	inst := action.NewInstall(cfg)
	inst.DryRun, inst.ClientOnly, inst.Replace = true, true, true
	inst.IncludeCRDs = true
	inst.ReleaseName, inst.Namespace = s.ReleaseName, s.Namespace
	if kubeVersion != "" {
		kv, err := chartutil.ParseKubeVersion(kubeVersion)
		if err != nil {
			return "", fmt.Errorf("kubeVersion %q: %w", kubeVersion, err)
		}
		inst.KubeVersion = kv
	}
	if len(s.APIVersions) > 0 {
		// Client-only mode always answers .Capabilities.APIVersions with Helm's
		// built-in list, which has no kinds and still has API versions removed
		// years ago, so a chart that asks "is policy/v1/PodDisruptionBudget
		// served?" renders for a cluster that does not exist. helm-controller
		// asks the real cluster. An in-memory stand-in lets the list be set.
		caps := chartutil.DefaultCapabilities.Copy()
		caps.APIVersions = chartutil.VersionSet(s.APIVersions)
		if inst.KubeVersion != nil {
			caps.KubeVersion = *inst.KubeVersion
		}
		cfg.Capabilities = caps
		cfg.KubeClient = &kubefake.PrintingKubeClient{Out: io.Discard}
		cfg.Releases = storage.Init(driver.NewMemory())
		inst.ClientOnly = false
	}
	rel, err := inst.Run(ch, s.Values)
	if err != nil {
		return "", err
	}

	// Hooks that run on install or upgrade create real objects (Jobs, the
	// ServiceAccounts and ConfigMaps they need); test and delete hooks do not
	// take part in convergence.
	manifest := rel.Manifest
	for _, hook := range rel.Hooks {
		for _, ev := range hook.Events {
			if ev == release.HookPreInstall || ev == release.HookPostInstall || ev == release.HookPreUpgrade || ev == release.HookPostUpgrade {
				manifest += "\n---\n# Source: " + hook.Path + "\n" + hook.Manifest
				break
			}
		}
	}

	return manifest, nil
}

// helmObjects parses a rendered manifest and applies the post-renderers.
func helmObjects(ch *chart.Chart, s helmSpec, manifest string) ([]model.Object, error) {
	var err error
	var out []model.Object
	for _, doc := range strings.Split("\n"+manifest, "\n---") {
		var o model.Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			return nil, fmt.Errorf("chart %s rendered invalid YAML: %w", ch.Name(), err)
		}
		if o == nil || o.Kind() == "" {
			continue
		}
		// helm applies namespaced objects that carry no namespace into the
		// release namespace
		if o.Namespace() == "" && !clusterScoped(o) {
			meta, _ := o["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				o["metadata"] = meta
			}
			meta["namespace"] = s.Namespace
		}
		out = append(out, o)
	}
	for _, pr := range s.PostRenderers {
		if out, err = postRender(out, pr); err != nil {
			return nil, fmt.Errorf("postRenderers: %w", err)
		}
	}
	return out, nil
}

// postRender applies one kustomize post-renderer the way helm-controller
// does: the rendered manifests become the resources of a kustomization that
// carries the patches and images.
func postRender(objs []model.Object, ov overlay) ([]model.Object, error) {
	dir, err := os.MkdirTemp("", "fluxlint-postrender-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var all []byte
	for _, o := range objs {
		b, err := yaml.Marshal(o)
		if err != nil {
			return nil, err
		}
		all = append(append(all, "---\n"...), b...)
	}
	if err := os.WriteFile(filepath.Join(dir, "all.yaml"), all, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - all.yaml\n"), 0o600); err != nil {
		return nil, err
	}
	out, _, err := build(dir, ov)
	return out, err
}

var clusterScopedKinds = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true, "CustomResourceDefinition": true,
	"StorageClass": true, "PriorityClass": true, "IngressClass": true, "RuntimeClass": true,
	"APIService": true, "CSIDriver": true, "CSINode": true, "VolumeAttachment": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
	"PodSecurityPolicy": true, "CertificateSigningRequest": true, "FlowSchema": true,
	"PriorityLevelConfiguration": true,
}

// clusterScoped is a heuristic: without the CRD at hand a custom resource's
// scope is unknown, and "Cluster…" is the near-universal naming convention.
func clusterScoped(o model.Object) bool {
	return clusterScopedKinds[o.Kind()] || strings.HasPrefix(o.Kind(), "Cluster")
}

// addDependencies does for a chart directory what source-controller does
// before packaging a chart from Git: dependencies listed in Chart.yaml that
// are not vendored under charts/ are loaded from their file:// path or fetched
// from their repository. fetch resolves name@version in a repository URL to a
// local chart.
func addDependencies(ch *chart.Chart, dir string, fetch func(repoURL, name, version string) (string, error)) []string {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || ch.Metadata == nil {
		return nil // a packaged chart carries its dependencies
	}
	present := map[string]bool{}
	for _, d := range ch.Dependencies() {
		present[d.Name()] = true
	}
	var notes []string
	for _, dep := range ch.Metadata.Dependencies {
		if present[dep.Name] {
			continue
		}
		var path string
		var err error
		switch {
		case strings.HasPrefix(dep.Repository, "file://"):
			path = filepath.Join(dir, strings.TrimPrefix(dep.Repository, "file://"))
		case dep.Repository == "" || strings.HasPrefix(dep.Repository, "@") || strings.HasPrefix(dep.Repository, "alias:"):
			err = fmt.Errorf("repository %q is a local helm alias", dep.Repository)
		default:
			path, err = fetch(dep.Repository, dep.Name, dep.Version)
		}
		var sub *chart.Chart
		if err == nil {
			sub, err = loader.Load(path)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("chart dependency %s %s could not be loaded, so its objects are missing from the analysis: %v", dep.Name, dep.Version, err))
			continue
		}
		notes = append(notes, addDependencies(sub, path, fetch)...)
		ch.AddDependency(sub)
	}
	return notes
}

// fetchChart resolves name@version from a Helm repository URL, as a chart
// dependency names it.
func (r *renderer) fetchChart(ctx context.Context, namespace, repoURL, name, version string) (string, error) {
	if r.resolver == nil {
		return "", fmt.Errorf("no source resolver")
	}
	res, err := r.resolver.Chart(ctx,
		model.Object{"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": name, "version": version}}}},
		model.Object{"kind": "HelmRepository", "metadata": map[string]any{"name": "dependency:" + repoURL, "namespace": namespace}, "spec": map[string]any{"url": repoURL}})
	if err != nil {
		return "", err
	}
	return res.Dir, nil
}
