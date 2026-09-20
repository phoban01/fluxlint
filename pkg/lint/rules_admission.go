package lint

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	psaapi "k8s.io/pod-security-admission/api"
	psapolicy "k8s.io/pod-security-admission/policy"
)

func (r *run) admissionRules() {
	r.podSecurity()
	r.customResources()
}

// podSecurity evaluates every pod template against the Pod Security level its
// namespace enforces, with the evaluator the API server itself uses. A
// violation means the pods are never created.
func (r *run) podSecurity() {
	evaluator, err := psapolicy.NewEvaluator(psapolicy.DefaultChecks(), nil)
	if err != nil {
		return
	}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			wl, ok := podTemplate(o)
			if !ok {
				continue
			}
			labels, known := r.ix.Wiring.nsLabels[o.Namespace()]
			if !known {
				continue // namespace not rendered from Git: its policy is unknown
			}
			level, err := psaapi.ParseLevel(labels[psaapi.EnforceLevelLabel])
			if err != nil || level == psaapi.LevelPrivileged {
				continue
			}
			version := psaapi.LatestVersion()
			if v, err := psaapi.ParseVersion(labels[psaapi.EnforceVersionLabel]); err == nil {
				version = v
			}
			var spec corev1.PodSpec
			var meta metav1.ObjectMeta
			if !convert(wl.Spec, &spec) || !convert(wl.Meta, &meta) {
				continue
			}
			var reasons []string
			for _, res := range evaluator.EvaluatePod(psaapi.LevelVersion{Level: level, Version: version}, &meta, &spec) {
				if !res.Allowed {
					reasons = append(reasons, res.ForbiddenReason+" ("+res.ForbiddenDetail+")")
				}
			}
			if len(reasons) > 0 {
				sort.Strings(reasons)
				r.report("FL-V002", c, o, fmt.Sprintf("namespace %s enforces Pod Security %q: pods will be rejected", o.Namespace(), level), reasons...)
			}
		}
	}
}

func convert(in, out any) bool {
	if in == nil {
		return true
	}
	b, err := json.Marshal(in)
	return err == nil && json.Unmarshal(b, out) == nil
}

// crdVersion is one served version of a rendered CRD, ready to validate with.
type crdVersion struct {
	validator  validation.SchemaValidator
	structural *structuralschema.Structural
}

// customResources checks every custom resource against the CRD that some
// component renders: the apiVersion must be served, and the object must pass
// the schema after defaulting, as it would in the API server.
func (r *run) customResources() {
	crds := map[string]map[string]*crdVersion{} // "group/Kind" -> version -> schema
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() != "CustomResourceDefinition" || o.Group() != "apiextensions.k8s.io" || o.Version() != "v1" {
				continue
			}
			var crd apiextv1.CustomResourceDefinition
			if !convert(map[string]any(o), &crd) {
				continue
			}
			gk := crd.Spec.Group + "/" + crd.Spec.Names.Kind
			for _, v := range crd.Spec.Versions {
				if !v.Served {
					continue
				}
				if crds[gk] == nil {
					crds[gk] = map[string]*crdVersion{}
				}
				cv := &crdVersion{}
				crds[gk][v.Name] = cv
				if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
					continue
				}
				var internal apiextensions.JSONSchemaProps
				if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v.Schema.OpenAPIV3Schema, &internal, nil); err != nil {
					continue
				}
				if val, _, err := validation.NewSchemaValidator(&internal); err == nil {
					cv.validator = val
				}
				if s, err := structuralschema.NewStructural(&internal); err == nil {
					cv.structural = s
				}
			}
		}
	}

	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			versions, ok := crds[o.GK()]
			if !ok {
				continue
			}
			cv, served := versions[o.Version()]
			if !served {
				r.report("FL-G006", c, o, fmt.Sprintf("uses %s, but the rendered CRD serves only %s", o.APIVersion(), strings.Join(sortedKeys(versions), ", ")))
				continue
			}
			if cv.validator == nil {
				continue
			}
			obj := runtime.DeepCopyJSON(jsonable(o))
			if cv.structural != nil {
				structuraldefaulting.Default(obj, cv.structural)
			}
			var problems []string
			for _, e := range validation.ValidateCustomResource(nil, obj, cv.validator) {
				problems = append(problems, e.Error())
			}
			if len(problems) > 0 {
				sort.Strings(problems)
				if len(problems) > 6 {
					problems = append(problems[:6], fmt.Sprintf("… %d more", len(problems)-6))
				}
				r.report("FL-V001", c, o, fmt.Sprintf("rejected by the %s schema of its CRD", o.Version()), problems...)
			}
		}
	}
}

// jsonable normalises YAML-decoded values (ints, nested model.Object) into the
// JSON shapes the API machinery expects.
func jsonable(o model.Object) map[string]any {
	var out map[string]any
	b, _ := json.Marshal(o)
	_ = json.Unmarshal(b, &out)
	return out
}
