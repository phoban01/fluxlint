package lint

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

// NeedRuntime is something a pod needs before it can start (a Secret,
// ConfigMap or ServiceAccount). Unlike the other needs it never fails an
// apply: the kubelet simply waits, so it matters for readiness only.
const NeedRuntime NeedKind = "runtime"

// unresolved is a pod reference nothing satisfies.
type unresolved struct {
	Consumer *model.Component
	Workload model.Object
	Ref      ref
	// MissingKeys is set when the object exists but lacks these keys.
	MissingKeys []string
	Producers   []keyed
}

// linkRuntime resolves every pod reference against the wiring index, adding
// needs for the ones that cross components.
func (ix *Index) linkRuntime() {
	w := buildWiring(ix)
	ix.Wiring = w
	for _, c := range ix.Tree.Components {
		seen := map[string]bool{}
		for _, o := range c.Objects {
			wl, ok := podTemplate(o)
			if !ok {
				continue
			}
			ns := o.Namespace()
			for _, rf := range podRefs(wl.Spec) {
				if rf.Optional {
					continue
				}
				if rf.Kind == "ServiceAccount" {
					p, ok := w.accounts[nn(ns, rf.Name)]
					if !ok {
						ix.Unresolved = append(ix.Unresolved, unresolved{Consumer: c, Workload: o, Ref: rf})
					} else if key := "sa|" + nn(ns, rf.Name); !seen[key] {
						seen[key] = true
						ix.addRuntimeNeed(c, p, o, "ServiceAccount "+nn(ns, rf.Name))
					}
					continue
				}
				index := w.secrets
				if rf.Kind == "ConfigMap" {
					index = w.configMaps
				}
				producers := index[nn(ns, rf.Name)]
				if len(producers) == 0 {
					ix.Unresolved = append(ix.Unresolved, unresolved{Consumer: c, Workload: o, Ref: rf})
					continue
				}
				var missing []string
				for _, k := range rf.Keys {
					if k != "" && !anyHasKey(producers, k) {
						missing = append(missing, k)
					}
				}
				if len(missing) > 0 {
					ix.Unresolved = append(ix.Unresolved, unresolved{Consumer: c, Workload: o, Ref: rf, MissingKeys: missing, Producers: producers})
				}
				if key := rf.Kind + "|" + nn(ns, rf.Name); !seen[key] && producers[0].Producer != nil {
					seen[key] = true
					ix.addRuntimeNeed(c, producers[0].Producer, o, rf.Kind+" "+nn(ns, rf.Name))
				}
			}
		}
	}
}

func anyHasKey(ps []keyed, key string) bool {
	for _, p := range ps {
		if p.Keys == nil || p.Keys[key] {
			return true
		}
	}
	return false
}

func (ix *Index) addRuntimeNeed(consumer, provider *model.Component, o model.Object, what string) {
	if consumer.Within(provider) {
		return
	}
	ix.Needs = append(ix.Needs, Need{Kind: NeedRuntime, What: what, Provider: provider, Consumer: consumer, Object: o})
}

// kubeRootCA and friends exist in every namespace.
var ambientConfigMaps = map[string]bool{"kube-root-ca.crt": true}

func (r *run) runtimeRules() {
	ix := r.ix
	opaque := 0
	for _, c := range ix.Tree.Components {
		if c.Opaque != "" {
			opaque++
		}
	}
	for _, u := range ix.Unresolved {
		if u.Ref.Kind == "ConfigMap" && ambientConfigMaps[u.Ref.Name] {
			continue
		}
		rule := "FL-R001"
		if u.Ref.Kind == "ServiceAccount" || u.Ref.Via == "imagePullSecrets" {
			rule = "FL-R002"
		}
		ns := u.Workload.Namespace()
		if len(u.MissingKeys) > 0 {
			r.report(rule, u.Consumer, u.Workload, fmt.Sprintf("%s needs key %s of %s %s, but %s only defines %s",
				u.Ref.Via, quoted(u.MissingKeys), u.Ref.Kind, nn(ns, u.Ref.Name), describeProducers(u.Producers), definedKeys(u.Producers)))
			continue
		}
		msg := fmt.Sprintf("%s references %s %s, which nothing creates", u.Ref.Via, u.Ref.Kind, nn(ns, u.Ref.Name))
		if opaque > 0 {
			// the producer may live in something we could not render
			r.reportAs(Warning, rule, u.Consumer, u.Workload, msg,
				fmt.Sprintf("low confidence: %d component(s) are not rendered and may create it", opaque))
			continue
		}
		if u.Ref.Via == "imagePullSecrets" {
			// the kubelet carries on without a pull secret it cannot find; only a
			// private image then fails to pull
			r.reportAs(Warning, rule, u.Consumer, u.Workload, msg, "the pod still starts if its images can be pulled without this Secret")
			continue
		}
		r.report(rule, u.Consumer, u.Workload, msg)
	}
	r.indexedPatches()
	r.serviceRules()
}

func quoted(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = strconv.Quote(k)
	}
	return strings.Join(q, ", ")
}

func definedKeys(ps []keyed) string {
	all := map[string]bool{}
	for _, p := range ps {
		for k := range p.Keys {
			all[k] = true
		}
	}
	if len(all) == 0 {
		return "no keys"
	}
	return quoted(sortedKeys(all))
}

// listIndex matches a JSON-pointer segment that addresses an element of a list
// of named or ordered things by position. Appends ("/-") and container indices
// are left alone: the first is safe and the second is rarely ambiguous.
var listIndex = regexp.MustCompile(`/(env|envFrom|volumes|volumeMounts|ports|args|command)/(\d+)(/|$)`)

// indexedPatches flags JSON patches in a Flux Kustomization that address list
// elements by position. They silently retarget when upstream adds or reorders
// an element, which is exactly what a version bump does.
func (r *run) indexedPatches() {
	for _, c := range r.ix.Tree.Components {
		if c.IsHelmRelease() || c.Spec == nil {
			continue
		}
		var detail []string
		seen := map[string]bool{}
		for _, p := range model.List(c.Spec, "spec", "patches") {
			target := model.Str(p, "target", "kind") + "/" + model.Str(p, "target", "name")
			for _, line := range strings.Split(model.Str(p, "patch"), "\n") {
				path, ok := strings.CutPrefix(strings.TrimSpace(line), "path:")
				if !ok || !listIndex.MatchString(path) {
					continue
				}
				path = strings.Trim(strings.TrimSpace(path), `"'`)
				if key := target + path; !seen[key] {
					seen[key] = true
					detail = append(detail, target+" "+path+r.resolvesTo(c, p, path))
				}
			}
		}
		if len(detail) > 0 {
			r.report("FL-R003", c, nil, fmt.Sprintf("%d patch path(s) address list elements by position; an upstream change to the list (typically a version bump) silently retargets them", len(detail)), detail...)
		}
	}
}

// resolvesTo says what a positional path points at in the rendered output.
func (r *run) resolvesTo(c *model.Component, patch any, path string) string {
	kind, name := model.Str(patch, "target", "kind"), model.Str(patch, "target", "name")
	for _, o := range c.Objects {
		if o.Kind() != kind || !strings.Contains(o.Name(), name) {
			continue
		}
		var cur any = map[string]any(o)
		var lastNamed string
		for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
			if i, err := strconv.Atoi(seg); err == nil {
				l, ok := cur.([]any)
				if !ok || i >= len(l) {
					return "  -> index out of range in the rendered " + kind
				}
				cur = l[i]
				if n := model.Str(cur, "name"); n != "" {
					lastNamed = n
				}
				continue
			}
			cur = model.Get(cur, seg)
			if cur == nil {
				break
			}
		}
		if lastNamed != "" {
			return fmt.Sprintf("  -> currently %q", lastNamed)
		}
	}
	return ""
}

// serviceRules checks Services and the admission webhooks that depend on them.
func (r *run) serviceRules() {
	type svc struct {
		owner *model.Component
		obj   model.Object
	}
	services := map[string]svc{}
	var workloads []workload
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Group() == "" && o.Kind() == "Service" {
				services[nn(o.Namespace(), o.Name())] = svc{c, o}
			}
			if wl, ok := podTemplate(o); ok {
				workloads = append(workloads, wl)
			}
		}
	}
	backends := func(s model.Object) []workload {
		sel, ok := model.Get(s, "spec", "selector").(map[string]any)
		if !ok || len(sel) == 0 {
			return nil
		}
		var out []workload
		for _, wl := range workloads {
			if wl.Object.Namespace() != s.Namespace() {
				continue
			}
			if selectorMatches(map[string]any{"matchLabels": sel}, wl.Labels) {
				out = append(out, wl)
			}
		}
		return out
	}

	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Group() != "admissionregistration.k8s.io" || !strings.HasSuffix(o.Kind(), "WebhookConfiguration") {
				continue
			}
			for _, wh := range model.List(o, "webhooks") {
				ref := model.Get(wh, "clientConfig", "service")
				if ref == nil {
					continue
				}
				failClosed := model.Str(wh, "failurePolicy") != "Ignore" // Fail is the default
				key := nn(model.Str(ref, "namespace"), model.Str(ref, "name"))
				s, ok := services[key]
				if !ok {
					r.report("FL-R004", c, o, fmt.Sprintf("webhook %s calls Service %s, which nothing creates", model.Str(wh, "name"), key))
					continue
				}
				bs := backends(s.obj)
				if len(bs) == 0 {
					r.report("FL-R004", c, o, fmt.Sprintf("webhook %s calls Service %s, whose selector matches no pod template", model.Str(wh, "name"), key))
					continue
				}
				live := false
				for _, b := range bs {
					live = live || b.Replicas == nil || *b.Replicas > 0
				}
				if !live && failClosed {
					r.report("FL-R004", c, o, fmt.Sprintf("webhook %s fails closed, but every workload behind Service %s is scaled to 0: all matching API requests will be rejected",
						model.Str(wh, "name"), key), "workload: "+bs[0].Object.String())
				}
			}
		}
	}
}
