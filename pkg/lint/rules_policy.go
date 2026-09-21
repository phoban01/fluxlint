package lint

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/policy"
)

// policySet compiles the ValidatingAdmissionPolicies the tree installs.
func (r *run) policySet() (*policy.Set, map[string]string) {
	var all []map[string]any
	plural := map[string]string{}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			all = append(all, o)
			if o.Kind() == "CustomResourceDefinition" {
				plural[model.Str(o, "spec", "group")+"/"+model.Str(o, "spec", "names", "kind")] = model.Str(o, "spec", "names", "plural")
			}
		}
	}
	return policy.NewSet(all), plural
}

// applier is the identity Flux applies c's objects with.
func applier(c *model.Component) string {
	controller := "kustomize-controller"
	if c.IsHelmRelease() {
		controller = "helm-controller"
	}
	if sa := model.Str(c.Spec, "spec", "serviceAccountName"); sa != "" {
		return "system:serviceaccount:" + c.Namespace + ":" + sa
	}
	return "system:serviceaccount:flux-system:" + controller
}

func (r *run) reportViolations(c *model.Component, o model.Object, vs []policy.Violation, verb string) {
	for _, v := range vs {
		where := fmt.Sprintf("ValidatingAdmissionPolicy %s (binding %s)", v.Policy, v.Binding)
		switch {
		case v.Undecided:
			r.reportAs(Info, "FL-V005", c, o, fmt.Sprintf("%s applies to this %s but could not be evaluated offline: %s", where, verb, v.Message))
		case v.Action == "Deny":
			r.report("FL-V005", c, o, fmt.Sprintf("%s rejects this %s: %s", where, verb, v.Message))
		default:
			r.reportAs(Warning, "FL-V005", c, o, fmt.Sprintf("%s warns on this %s: %s", where, verb, v.Message))
		}
	}
}

// admissionPolicies evaluates every rendered object as a create: what a new
// cluster, or a new object, goes through.
func (r *run) admissionPolicies() {
	set, plural := r.policySet()
	names := make([]string, 0, len(set.CompileErrors))
	for name := range set.CompileErrors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, c := range r.ix.Tree.Components {
			for _, o := range c.Objects {
				if o.Kind() == "ValidatingAdmissionPolicy" && o.Name() == name {
					r.report("FL-V006", c, o, fmt.Sprintf("does not compile: %v", set.CompileErrors[name]))
				}
			}
		}
	}
	if set.Empty() {
		return
	}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			r.reportViolations(c, o, set.Evaluate(context.Background(), policy.Request{Object: o, Resource: resourceOf(o, plural), User: applier(c)}), "object")
		}
	}
}

// updatePolicies evaluates every object the change modifies as an update of
// what the base rendered, which is where oldObject rules (immutability, state
// transitions) speak. The policies are the head's.
func (r *run) updatePolicies(before, after map[string]owned) {
	set, plural := r.policySet()
	if set.Empty() {
		return
	}
	ids := make([]string, 0, len(after))
	for id := range after {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := after[id]
		b, existed := before[id]
		if !existed || reflect.DeepEqual(normalise(map[string]any(b.o)), normalise(map[string]any(a.o))) {
			continue
		}
		req := policy.Request{Object: a.o, Old: b.o, Resource: resourceOf(a.o, plural), User: applier(a.c)}
		r.reportViolations(a.c, a.o, set.Evaluate(context.Background(), req), "update")
	}
}

func resourceOf(o model.Object, plural map[string]string) string {
	if p := plural[o.GK()]; p != "" {
		return p
	}
	return pluralOf(o.Kind())
}

// kyvernoPolicies hands the repository's Kyverno validation policies, and
// everything else it renders, to the kyverno CLI.
func (r *run) kyvernoPolicies() {
	if r.cfg.Rules.Off["FL-V007"] {
		return
	}
	var policies, resources []map[string]any
	owners := map[string]owned{}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if policy.IsKyvernoPolicy(o) {
				policies = append(policies, o)
				continue
			}
			resources = append(resources, o)
			owners[o.APIVersion()+"|"+o.Kind()+"|"+o.Namespace()+"|"+o.Name()] = owned{c, o}
		}
	}
	if len(policies) == 0 {
		return
	}
	results, err := policy.Kyverno(context.Background(), r.cfg.Kyverno.Command, policies, resources)
	switch {
	case errors.Is(err, policy.ErrNoKyverno):
		r.reportAs(Info, "FL-V007", nil, nil, fmt.Sprintf("%d Kyverno validation policies were not evaluated: the kyverno CLI is not on PATH", len(policies)))
		return
	case err != nil:
		r.reportAs(Info, "FL-V007", nil, nil, fmt.Sprintf("%d Kyverno validation policies were not evaluated: %v", len(policies), err))
		return
	}
	for _, res := range results {
		own, ok := owners[res.APIVersion+"|"+res.Kind+"|"+res.Namespace+"|"+res.Name]
		if !ok {
			continue // an object Kyverno derived, such as the Pod of a Deployment
		}
		where := fmt.Sprintf("Kyverno policy %s, rule %s", res.Policy, res.Rule)
		switch {
		case res.Result == "fail" && res.Enforced:
			r.report("FL-V007", own.c, own.o, fmt.Sprintf("%s rejects this object: %s", where, res.Message))
		case res.Result == "fail":
			r.reportAs(Info, "FL-V007", own.c, own.o, fmt.Sprintf("%s (Audit) fails for this object: %s", where, res.Message))
		case res.Result == "error" && res.Enforced:
			r.reportAs(Info, "FL-V007", own.c, own.o, fmt.Sprintf("%s could not be evaluated offline: %s", where, res.Message))
		}
	}
}
