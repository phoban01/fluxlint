package lint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

// notStrings returns the keys of a metadata map whose values YAML
// did not read as strings: 1.31, true, 0. A null counts as the empty string,
// as it does for the API server.
func notStrings(o model.Object, field string) []string {
	m, _ := model.Get(o, "metadata", field).(map[string]any)
	var out []string
	for k, v := range m {
		if _, ok := v.(string); !ok && v != nil {
			out = append(out, fmt.Sprintf("%s: %v", k, v))
		}
	}
	sort.Strings(out)
	return out
}

// metadataStrings reports label values that are not strings. Annotations are
// not checked: kustomize converts those to strings on the way out, and labels
// it leaves alone (tested against kustomize 5).
//
// For labels on anything a Flux Kustomization applies, the consequence is not
// an error but a silent loss. kustomize-controller stamps its ownership labels
// on every object by reading the labels, adding two and writing the map back
// (fluxcd/pkg/ssa, SetOwnerLabels). Reading fails as a whole when one value is
// not a string, the failure is discarded, and what is written back is Flux's
// two labels alone. The object is then valid, the apply succeeds and the
// Kustomization is Ready. Flux's maintainers call this intentional
// (fluxcd/pkg#1208), so the only place to catch it is before the merge.
func (r *run) metadataStrings() {
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if _, encrypted := o["sops"]; encrypted {
				continue
			}
			if bad := notStrings(o, "labels"); len(bad) > 0 {
				lost := 0
				if m, ok := model.Get(o, "metadata", "labels").(map[string]any); ok {
					lost = len(m)
				}
				msg := fmt.Sprintf("has a label whose value is not a string, so the API server rejects the object (quote the value): %s", strings.Join(bad, ", "))
				if !c.IsHelmRelease() {
					msg = fmt.Sprintf("has a label whose value is not a string (%s). Flux does not fail: it applies the object with none of its %d labels and reports Ready", strings.Join(bad, ", "), lost)
				}
				r.report("FL-V008", c, o, msg, "quote the value, or write it so that YAML reads a string: \"1.31\", \"true\", v1.31")
			}
		}
	}
}
