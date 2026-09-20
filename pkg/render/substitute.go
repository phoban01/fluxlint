package render

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"

	"github.com/fluxcd/pkg/envsubst"
	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

const substituteAnnotation = "kustomize.toolkit.fluxcd.io/substitute"

// literalVar finds ${...} in components that have no postBuild, where Flux
// leaves the text untouched.
var literalVar = regexp.MustCompile(`\$\{[_a-zA-Z][_a-zA-Z0-9]*[^}]*\}`)

func substitutionDisabled(o model.Object) bool {
	return model.Str(o, "metadata", "annotations", substituteAnnotation) == "disabled" ||
		model.Str(o, "metadata", "labels", substituteAnnotation) == "disabled"
}

// substitute applies Flux post-build substitution to every object using Flux's
// own envsubst implementation, and records each variable lookup.
func substitute(objs []model.Object, vars map[string]string) ([]model.Object, []model.VarUse, error) {
	out := make([]model.Object, 0, len(objs))
	var uses []model.VarUse
	for _, o := range objs {
		if substitutionDisabled(o) {
			out = append(out, o)
			continue
		}
		raw, err := yaml.Marshal(o)
		if err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		res, err := envsubst.Eval(string(raw), func(name string) (string, bool) {
			seen[name] = true
			return vars[name], true // Flux is non-strict: undefined becomes ""
		})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: variable substitution failed: %w", o, err)
		}
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			_, defined := vars[n]
			u := model.VarUse{Name: n, Object: o, Defined: defined}
			if !defined {
				// Strict evaluation for just this variable fails only when the
				// expression has no default.
				_, strictErr := envsubst.Eval(string(raw), func(name string) (string, bool) {
					if name == n {
						return "", false
					}
					return vars[name], true
				})
				u.HasDefault = strictErr == nil
			}
			uses = append(uses, u)
		}
		var sub model.Object
		if err := yaml.Unmarshal([]byte(res), &sub); err != nil {
			return nil, nil, fmt.Errorf("%s: invalid YAML after substitution: %w", o, err)
		}
		out = append(out, sub)
	}
	return out, uses, nil
}

// literalVars lists ${...} occurrences that will reach the cluster verbatim.
func literalVars(objs []model.Object) []model.VarUse {
	var uses []model.VarUse
	for _, o := range objs {
		raw, err := yaml.Marshal(o)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, m := range literalVar.FindAllString(string(raw), -1) {
			if !seen[m] {
				seen[m] = true
				uses = append(uses, model.VarUse{Name: m, Object: o})
			}
		}
	}
	return uses
}

// dataOf returns the key/values of a ConfigMap or Secret.
func dataOf(o model.Object) map[string]string {
	out := map[string]string{}
	if m, ok := model.Get(o, "data").(map[string]any); ok {
		for k, v := range m {
			s, _ := v.(string)
			if o.Kind() == "Secret" {
				if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
					s = string(dec)
				}
			}
			out[k] = s
		}
	}
	if m, ok := model.Get(o, "stringData").(map[string]any); ok {
		for k, v := range m {
			s, _ := v.(string)
			out[k] = s
		}
	}
	return out
}
