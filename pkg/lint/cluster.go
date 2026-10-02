package lint

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

// ParseClusterState reads the output of
//
//	kubectl get kustomizations.kustomize.toolkit.fluxcd.io,helmreleases.helm.toolkit.fluxcd.io -A -o json
//
// (or -o yaml): a List, or a single object.
func ParseClusterState(r io.Reader) (map[string]config.ClusterStatus, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	items := model.List(doc, "items")
	if items == nil && model.Str(doc, "kind") != "" {
		items = []any{doc}
	}
	out := map[string]config.ClusterStatus{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		o := model.Object(m)
		kind := ""
		switch {
		case o.IsFluxKustomization():
			kind = model.KindKustomization
		case o.IsHelmRelease():
			kind = model.KindHelmRelease
		default:
			continue
		}
		st := config.ClusterStatus{Reason: "NoStatus", Message: "the object has no Ready condition yet"}
		for _, c := range model.List(o, "status", "conditions") {
			if model.Str(c, "type") == "Ready" {
				st = config.ClusterStatus{Ready: model.Str(c, "status") == "True", Reason: model.Str(c, "reason"), Message: model.Str(c, "message")}
			}
		}
		if model.Bool(o, "spec", "suspend") {
			continue // says nothing about the manifests
		}
		out[model.KeyFor(kind, o.Namespace(), o.Name())] = st
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Flux Kustomization or HelmRelease found")
	}
	return out, nil
}

// mentions reports whether text names the component, and not merely another
// whose name starts the same way.
func mentions(text, name string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		end := i + j + len(name)
		if end == len(text) || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789-.", rune(text[end])) {
			return true
		}
		i = end
	}
}

// clusterRules compares the findings so far with what a real cluster
// reported for the same revision. It must run after every other rule.
func (r *run) clusterRules() {
	state := r.cfg.ClusterState
	if len(state) == 0 {
		return
	}
	var predicted strings.Builder // everything a finding that can explain a failure says
	errorsOn := map[string][]string{}
	for _, f := range r.findings {
		// "not analysed", timing advice and the comparison itself explain no failure
		if f.Severity == Info || !explainsFailure(f.Rule) {
			continue
		}
		predicted.WriteString(f.Component + "\n" + f.Message + "\n" + strings.Join(f.Detail, "\n") + "\n")
		if f.Severity == Error && f.Component != "" {
			errorsOn[f.Component] = append(errorsOn[f.Component], f.Rule)
		}
	}
	failed := func(c *model.Component) bool {
		st, ok := state[c.Key()]
		return ok && !st.Ready
	}

	var absent, blind []string
	for _, c := range r.ix.Tree.Components {
		st, ok := state[c.Key()]
		switch {
		case !ok:
			if !c.IsRoot {
				absent = append(absent, c.String())
			}
		case st.Ready:
			if rules := errorsOn[c.String()]; len(rules) > 0 {
				sort.Strings(rules)
				r.report("FL-O002", c, nil, fmt.Sprintf("is Ready in the cluster, although %s reported an error on it: either the finding is wrong, or the cluster was helped along by something that is not in Git",
					strings.Join(uniq(rules), ", ")))
			}
		default:
			if c.Opaque != "" {
				// fluxlint did not render it, so it can neither have predicted
				// the failure nor have missed it
				blind = append(blind, c.String()+": "+c.Opaque)
				continue
			}
			if mentions(predicted.String(), c.String()) {
				continue
			}
			// a failure that only repeats another one is not a second gap
			cause := ""
			for _, d := range c.DependsOn {
				if dep := r.ix.Tree.ByKey[c.DepKey(d)]; dep != nil && failed(dep) {
					cause = dep.String()
				}
			}
			for _, child := range c.Children {
				if failed(child) {
					cause = child.String()
				}
			}
			if cause != "" {
				continue
			}
			msg := st.Message
			if len(msg) > 400 {
				msg = msg[:400] + " …"
			}
			r.report("FL-O001", c, nil, "is not Ready in the cluster, and no finding predicted it", st.Reason+": "+msg)
		}
	}
	var unknown []string
	for key := range state {
		if r.ix.Tree.ByKey[key] == nil {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(absent)
	sort.Strings(unknown)
	if len(absent) > 0 {
		r.report("FL-O003", nil, nil, fmt.Sprintf("%d component(s) rendered from Git are not in the cluster state, so nothing was compared for them", len(absent)), absent...)
	}
	if len(blind) > 0 {
		sort.Strings(blind)
		r.report("FL-O003", nil, nil, fmt.Sprintf("%d component(s) are not Ready in the cluster but were not rendered, so there is no verdict on them", len(blind)), blind...)
	}
	if len(unknown) > 0 {
		r.report("FL-O003", nil, nil, fmt.Sprintf("%d Flux object(s) in the cluster state are not rendered from this entrypoint", len(unknown)), unknown...)
	}
}

func uniq(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// explainsFailure reports whether a rule's findings can say why a component
// is not Ready. A source that could not be fetched (FL-X001) is a gap in the
// analysis, and timing advice describes delays, not failures.
func explainsFailure(rule string) bool {
	switch {
	case rule == "FL-X001", rule == "FL-X002", rule == "FL-X003":
		return false
	case strings.HasPrefix(rule, "FL-T"), strings.HasPrefix(rule, "FL-O"):
		return rule == "FL-T100"
	}
	return true
}
