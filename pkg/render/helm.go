package render

import (
	"fmt"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"sigs.k8s.io/yaml"
)

// helmSpec is what helm-controller derives from a HelmRelease before installing.
type helmSpec struct {
	ReleaseName, Namespace string
	Values                 map[string]any
	Notes                  []string
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
		if model.Str(vf, "targetPath") != "" {
			s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s uses targetPath, which is not modelled; that value is left at the chart default", kind, name))
			continue
		}
		data, ok := r.data[kind+"/"+hr.Namespace()+"/"+name]
		if !ok {
			if !model.Bool(vf, "optional") {
				s.Notes = append(s.Notes, fmt.Sprintf("valuesFrom %s/%s is not rendered by any component; its values are left at chart defaults", kind, name))
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
	if len(model.List(hr, "spec", "postRenderers")) > 0 {
		s.Notes = append(s.Notes, "spec.postRenderers are not applied yet")
	}
	return s
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

// helmTemplate is `helm template --include-crds`, in process.
func helmTemplate(chartPath string, s helmSpec, kubeVersion string) ([]model.Object, error) {
	ch, err := loader.Load(chartPath)
	if err != nil {
		return nil, err
	}
	cfg := &action.Configuration{Log: func(string, ...any) {}}
	inst := action.NewInstall(cfg)
	inst.DryRun, inst.ClientOnly, inst.Replace = true, true, true
	inst.IncludeCRDs = true
	inst.ReleaseName, inst.Namespace = s.ReleaseName, s.Namespace
	if kubeVersion != "" {
		kv, err := chartutil.ParseKubeVersion(kubeVersion)
		if err != nil {
			return nil, fmt.Errorf("kubeVersion %q: %w", kubeVersion, err)
		}
		inst.KubeVersion = kv
	}
	rel, err := inst.Run(ch, s.Values)
	if err != nil {
		return nil, err
	}

	var out []model.Object
	for _, doc := range strings.Split("\n"+rel.Manifest, "\n---") {
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
	return out, nil
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
