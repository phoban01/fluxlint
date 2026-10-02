package lint

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

func (r *run) graphRules() {
	ix := r.ix

	for _, c := range ix.Tree.Components {
		if c.External && c.Opaque != "" && c.BuildErr == nil &&
			c.Opaque != model.OpaqueNoResolver && c.Opaque != model.OpaqueUndefinedSource {
			msg := "not analysed: " + c.Opaque
			if c.SourceErr != nil {
				msg += ": " + c.SourceErr.Error()
			}
			if c.SourceMissing {
				// not a gap in the analysis but a defect: Flux will not find it either
				r.reportAs(Error, "FL-X001", c, nil, "its source does not exist: "+c.SourceErr.Error())
			} else {
				r.report("FL-X001", c, nil, msg)
			}
		}
		for _, note := range c.RenderNotes {
			r.report("FL-X003", c, nil, note)
		}
		if c.FloatingRef != "" {
			r.report("FL-X002", c, nil, fmt.Sprintf("source %s: %s (analysed %s)", c.Source, c.FloatingRef, c.SourceRevision))
		}
		switch {
		case c.BuildErr == nil:
		case c.IsHelmRelease():
			r.report("FL-G008", c, nil, fmt.Sprintf("chart %s cannot be rendered with this release's values: %v", c.SourceRevision, c.BuildErr))
		default:
			r.report("FL-G008", c, nil, fmt.Sprintf("cannot render path %q: %v", c.Path, c.BuildErr))
		}
		for _, d := range c.DependsOn {
			if ix.Tree.ByKey[c.DepKey(d)] == nil {
				r.report("FL-G001", c, nil, fmt.Sprintf("dependsOn %s/%s, which does not exist", d.Namespace, d.Name))
			}
		}
	}
	for _, n := range ix.MissingSources {
		r.report("FL-G001", n.Consumer, n.Object, fmt.Sprintf("references source %s, which does not exist", n.What))
	}

	// dual ownership
	for _, id := range sortedKeys(ix.Owner) {
		owners := ix.Owner[id]
		if len(owners) < 2 {
			continue
		}
		names := make([]string, len(owners))
		for i, o := range owners {
			names[i] = o.String()
		}
		var obj model.Object
		same := true
		for _, owner := range owners {
			for _, o := range owner.Objects {
				if strings.HasSuffix(id, "\x00"+o.ID()) || o.ID() == id {
					if obj == nil {
						obj = o
					} else if !reflect.DeepEqual(normalise(map[string]any(obj)), normalise(map[string]any(o))) {
						same = false
					}
				}
			}
		}
		msg := "managed by " + strings.Join(names, " and ")
		if same && obj != nil && obj.Kind() == "Namespace" && obj.Group() == "" {
			// every kubebuilder project ships its own Namespace, and clusters
			// also declare them centrally. Identical copies do not fight; the
			// hazard is that pruning either owner deletes the namespace for both
			r.reportAs(Warning, "FL-G003", nil, obj, msg+". The copies are identical, so they do not overwrite each other, but if either owner stops rendering it and prunes, the namespace and everything in it is deleted")
			continue
		}
		r.report("FL-G003", nil, obj, msg)
	}

	for _, n := range ix.MissingNamespaces {
		r.report("FL-G004", n.Consumer, n.Object, fmt.Sprintf("namespace %q is not created by any component", n.What))
	}

	// one finding per API group keeps this readable while sources are opaque
	byGroup := map[string][]string{}
	for _, n := range ix.MissingCRDs {
		g := n.Object.Group()
		byGroup[g] = append(byGroup[g], n.Object.Kind())
	}
	for _, g := range sortedKeys(byGroup) {
		kinds := dedupe(byGroup[g])
		r.report("FL-G005", nil, nil, fmt.Sprintf("no component installs CRDs for group %s (kinds: %s)", g, strings.Join(kinds, ", ")))
	}

	// deadlocks
	full := fullGraph(ix)
	for _, scc := range full.Cycles() {
		var detail []string
		comps := map[string]bool{}
		for _, e := range full.EdgesWithin(scc) {
			if e.Reason == "apply and health timeout" {
				continue
			}
			detail = append(detail, fmt.Sprintf("%s -> %s  (%s)", e.From, e.To, e.Reason))
		}
		for _, n := range scc {
			comps[n[strings.Index(n, "(")+1:len(n)-1]] = true
		}
		sort.Strings(detail)
		// anchor the finding on a participant that is defined in the repository
		var anchor *model.Component
		for _, n := range scc {
			if c := r.componentOfEvent(n); c != nil && anchor == nil {
				if file, _ := r.locate(c, nil); file != "" {
					anchor = c
				}
			}
		}
		r.report("FL-G002", anchor, nil,
			"cannot converge from an empty cluster: cycle between "+strings.Join(sortedKeys(comps), ", "), detail...)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (r *run) substitutionRules() {
	for _, c := range r.ix.Tree.Components {
		for _, sf := range c.MissingSubstituteFrom {
			r.report("FL-S002", c, nil, fmt.Sprintf("substituteFrom %s/%s is not rendered by any component", sf.Kind, sf.Name))
		}
		reported := map[string]bool{}
		for _, u := range c.VarUses {
			if u.Defined || u.HasDefault || reported[u.Name] {
				continue
			}
			reported[u.Name] = true
			r.report("FL-S001", c, u.Object, fmt.Sprintf("${%s} is not defined and has no default; Flux substitutes \"\"", u.Name))
		}
		literal := map[string]bool{}
		for _, u := range c.LiteralVars {
			if !u.Object.IsFluxKustomization() { // a child's spec may be substituted by whoever applies it
				literal[u.Name] = true
			}
		}
		if names := sortedKeys(literal); len(names) > 0 {
			shown := names
			if len(shown) > 5 {
				shown = append(append([]string{}, names[:5]...), fmt.Sprintf("… %d more", len(names)-5))
			}
			r.report("FL-S004", c, nil, "has no postBuild, so these expressions are applied verbatim (fine for shell snippets, a bug if substitution was intended): "+strings.Join(shown, ", "))
		}
	}
}
