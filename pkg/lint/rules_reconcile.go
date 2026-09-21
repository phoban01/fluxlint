package lint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/graph"
	"github.com/phoban01/fluxlint/pkg/model"
)

// The rules in this file cover what a cluster test finds by reconciling for
// real: applies that land while an admission webhook is still starting, and
// fields that Flux and another controller both write.

func labelsOf(o model.Object) map[string]string {
	out := map[string]string{}
	if m, ok := model.Get(o, "metadata", "labels").(map[string]any); ok {
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// irregular plurals of built-in kinds; the rest follow the usual English rules
var plurals = map[string]string{"Endpoints": "endpoints"}

func pluralOf(kind string) string {
	if p, ok := plurals[kind]; ok {
		return p
	}
	k := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(k, "s"), strings.HasSuffix(k, "x"), strings.HasSuffix(k, "ch"):
		return k + "es"
	case strings.HasSuffix(k, "y") && !strings.HasSuffix(k, "ay") && !strings.HasSuffix(k, "ey"):
		return k[:len(k)-1] + "ies"
	}
	return k + "s"
}

func anyOf(list []any, want ...string) bool {
	for _, v := range list {
		for _, w := range want {
			if fmt.Sprint(v) == w {
				return true
			}
		}
	}
	return false
}

// gate is one fail-closed admission webhook.
type gate struct {
	owner  *model.Component
	config model.Object
	hook   any
}

// intercepts reports whether the API server calls the webhook when o is
// applied. It answers no whenever it cannot tell.
func (g gate) intercepts(o model.Object, resource string, namespaces map[string]model.Object) bool {
	if len(model.List(g.hook, "matchConditions")) > 0 {
		return false // CEL over the request: not evaluated
	}
	matched := false
	for _, rule := range model.List(g.hook, "rules") {
		if !anyOf(model.List(rule, "operations"), "*", "CREATE", "UPDATE") {
			continue
		}
		if !anyOf(model.List(rule, "apiGroups"), "*", o.Group()) {
			continue
		}
		if vs := model.List(rule, "apiVersions"); len(vs) > 0 && !anyOf(vs, "*", o.Version()) {
			continue
		}
		if !anyOf(model.List(rule, "resources"), "*", "*/*", resource) {
			continue
		}
		switch scope := model.Str(rule, "scope"); {
		case scope == "Namespaced" && o.Namespace() == "", scope == "Cluster" && o.Namespace() != "":
			continue
		}
		matched = true
	}
	if !matched {
		return false
	}
	if sel := model.Get(g.hook, "objectSelector"); sel != nil && !selectorMatches(sel, labelsOf(o)) {
		return false
	}
	if sel, ok := model.Get(g.hook, "namespaceSelector").(map[string]any); ok && len(sel) > 0 {
		// a Namespace is selected by its own labels, anything namespaced by
		// the labels of its namespace, anything else always
		switch {
		case o.Kind() == "Namespace" && o.Group() == "":
			return selectorMatches(sel, withNameLabel(o))
		case o.Namespace() != "":
			ns, known := namespaces[o.Namespace()]
			return known && selectorMatches(sel, withNameLabel(ns))
		}
	}
	return true
}

// withNameLabel adds the label the API server sets on every namespace.
func withNameLabel(ns model.Object) map[string]string {
	l := labelsOf(ns)
	l["kubernetes.io/metadata.name"] = ns.Name()
	return l
}

// admissionWindows reports applies that can land between a fail-closed
// webhook being registered and its backend serving. Before the registration
// the API server does not call the webhook; after the backend is up it
// answers. In between, every matching request is rejected.
func (r *run) admissionWindows(declared *graph.Graph) {
	var gates []gate
	namespaces := map[string]model.Object{}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() == "Namespace" && o.Group() == "" {
				namespaces[o.Name()] = o
			}
			if o.Group() != "admissionregistration.k8s.io" || !strings.HasSuffix(o.Kind(), "WebhookConfiguration") {
				continue
			}
			for _, wh := range model.List(o, "webhooks") {
				if model.Str(wh, "failurePolicy") == "Ignore" || model.Get(wh, "clientConfig", "service") == nil {
					continue
				}
				gates = append(gates, gate{c, o, wh})
			}
		}
	}
	if len(gates) == 0 {
		return
	}
	plural := map[string]string{}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() == "CustomResourceDefinition" {
				plural[model.Str(o, "spec", "group")+"/"+model.Str(o, "spec", "names", "kind")] = model.Str(o, "spec", "names", "plural")
			}
		}
	}

	type hit struct {
		first model.Object
		count int
	}
	for _, c := range r.ix.Tree.Components {
		hits := map[*model.Component]*hit{}
		hooks := map[*model.Component]gate{}
		for _, o := range c.Objects {
			resource := plural[o.GK()]
			if resource == "" {
				resource = pluralOf(o.Kind())
			}
			for _, g := range gates {
				// within one apply, or an apply made by the webhook's own
				// component, the order is Flux's to get right
				if c.Within(g.owner) || g.owner.Within(c) || !g.intercepts(o, resource, namespaces) {
					continue
				}
				if hits[g.owner] == nil {
					hits[g.owner] = &hit{first: o}
					hooks[g.owner] = g
				}
				hits[g.owner].count++
				break
			}
		}
		owners := make([]*model.Component, 0, len(hits))
		for p := range hits {
			owners = append(owners, p)
		}
		sort.Slice(owners, func(i, j int) bool { return owners[i].String() < owners[j].String() })
		for _, p := range owners {
			h, g := hits[p], hooks[p]
			if declared.Reachable(readyOf(c), startOf(p), nil) {
				continue // applied before the webhook is registered
			}
			penalty := c.RetryInterval
			if !c.HasRetryInterval {
				penalty = c.Interval
			}
			what := fmt.Sprintf("%s (and %d more)", h.first, h.count-1)
			if h.count == 1 {
				what = h.first.String()
			}
			hook := fmt.Sprintf("webhook %s of %s", model.Str(g.hook, "name"), g.config)
			switch after := declared.Reachable(readyOf(p), startOf(c), nil); {
			case after && (p.BlocksOnHealth() || p.IsHelmRelease()):
				continue
			case after:
				r.report("FL-R008", c, h.first, fmt.Sprintf("%s is admitted by fail-closed %s; %s is ordered after %s, but %s does not wait for health, so it is Ready once applied, not once the webhook answers",
					what, hook, c, p, p),
					fmt.Sprintf("set wait: true (or healthChecks for the webhook Deployment) on %s", waiter(c, p)),
					fmt.Sprintf("an apply rejected in that window is retried after %v", penalty))
			default:
				r.report("FL-R008", c, h.first, fmt.Sprintf("%s is admitted by fail-closed %s, installed by %s, and nothing orders the two: an apply that lands while the webhook starts is rejected",
					what, hook, p),
					orderAfter(c, p),
					fmt.Sprintf("an apply rejected in that window is retried after %v", penalty))
			}
		}
	}
}

var caInjection = []string{"cert-manager.io/inject-ca-from", "cert-manager.io/inject-ca-from-secret", "cert-manager.io/inject-apiserver-ca"}

// contestedFields reports fields that the manifests set and another
// controller also writes. Flux applies server-side and takes the field back on
// every reconcile; the other controller writes it again.
func (r *run) contestedFields() {
	type target struct{ group, kind, namespace, name string }
	scaled := map[target]model.Object{}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			var ref any
			switch {
			case o.Group() == "autoscaling" && o.Kind() == "HorizontalPodAutoscaler":
				ref = model.Get(o, "spec", "scaleTargetRef")
			case o.Group() == "keda.sh" && o.Kind() == "ScaledObject":
				ref = model.Get(o, "spec", "scaleTargetRef")
			}
			if ref == nil {
				continue
			}
			group, _, _ := strings.Cut(model.Str(ref, "apiVersion"), "/")
			if !strings.Contains(model.Str(ref, "apiVersion"), "/") {
				group = "apps" // KEDA defaults to apps/v1 Deployment
			}
			kind := model.Str(ref, "kind")
			if kind == "" {
				kind = "Deployment"
			}
			scaled[target{group, kind, o.Namespace(), model.Str(ref, "name")}] = o
		}
	}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if scaler, ok := scaled[target{o.Group(), o.Kind(), o.Namespace(), o.Name()}]; ok && model.Get(o, "spec", "replicas") != nil {
				r.report("FL-R009", c, o, fmt.Sprintf("spec.replicas is set, and %s scales the same workload: every reconcile resets the replica count and the autoscaler changes it back", scaler),
					"remove spec.replicas from the manifest")
			}
			annotations, _ := model.Get(o, "metadata", "annotations").(map[string]any)
			injected := ""
			for _, a := range caInjection {
				if _, ok := annotations[a]; ok {
					injected = a
				}
			}
			if injected == "" {
				continue
			}
			var bundles []any
			for _, wh := range model.List(o, "webhooks") {
				bundles = append(bundles, model.Get(wh, "clientConfig", "caBundle"))
			}
			bundles = append(bundles, model.Get(o, "spec", "conversion", "webhook", "clientConfig", "caBundle"),
				model.Get(o, "spec", "caBundle"))
			for _, b := range bundles {
				if s, _ := b.(string); s != "" {
					r.report("FL-R009", c, o, fmt.Sprintf("caBundle is set, and the %s annotation asks cert-manager to fill it in: every reconcile restores the value in Git and cert-manager replaces it", injected),
						"remove caBundle from the manifest")
					break
				}
			}
		}
	}
}

// unstableRenders reports chart objects that differ between two renders of
// the same release.
func (r *run) unstableRenders() {
	for _, c := range r.ix.Tree.Components {
		if len(c.Unstable) == 0 {
			continue
		}
		byID := map[string]model.Object{}
		for _, o := range c.Objects {
			byID[o.ID()] = o
		}
		for _, id := range c.Unstable {
			o, ok := byID[id]
			if !ok {
				continue // removed or renamed by a post-renderer
			}
			sev := Info
			if o.Kind() == "Secret" || strings.HasSuffix(o.Kind(), "WebhookConfiguration") {
				sev = Warning
			}
			r.reportAs(sev, "FL-R010", c, o, "renders differently each time, so every upgrade of the release replaces it",
				"keep the existing value with lookup, or pass the value in through valuesFrom")
		}
	}
}

// waiter is the component whose readiness c can wait for in order to follow
// p. dependsOn only joins objects of one kind, so a Kustomization follows a
// HelmRelease by depending on the Kustomization that applies it.
func waiter(c, p *model.Component) *model.Component {
	for p.Kind != c.Kind && p.Parent != nil {
		p = p.Parent
	}
	return p
}

func orderAfter(c, p *model.Component) string {
	w := waiter(c, p)
	switch {
	case w == p && p.IsHelmRelease():
		return fmt.Sprintf("add dependsOn: %s to %s", p, c) // helm-controller waits for the release's workloads
	case w == p:
		return fmt.Sprintf("add dependsOn: %s to %s, and wait: true on %s", p, c, p)
	}
	return fmt.Sprintf("add dependsOn: %s to %s, and wait: true on %s, which applies %s", w, c, w, p)
}

// imageRules reports workloads whose image the registry does not have.
func (r *run) imageRules() {
	for _, c := range r.ix.Tree.Components {
		if len(c.ImageProblems) == 0 {
			continue
		}
		for _, o := range c.Objects {
			wl, ok := podTemplate(o)
			if !ok {
				continue
			}
			sev := Error
			if wl.Replicas != nil && *wl.Replicas == 0 {
				sev = Warning
			}
			for _, list := range []string{"initContainers", "containers"} {
				for _, ctr := range model.List(wl.Spec, list) {
					img := model.Str(ctr, "image")
					if why, bad := c.ImageProblems[img]; bad {
						r.reportAs(sev, "FL-X004", c, o, fmt.Sprintf("container %s uses image %s: %s", model.Str(ctr, "name"), img, why))
					}
				}
			}
		}
	}
}

// secretOwners reports a Secret that two ExternalSecrets both want to own.
// The operator lets the first one win; the second never becomes Ready, and
// neither does the ClusterExternalSecret that created it.
func (r *run) secretOwners() {
	type claim struct {
		name string // of the ExternalSecret
		c    *model.Component
		o    model.Object
	}
	claims := map[string][]claim{}
	add := func(namespace, esName string, spec any, c *model.Component, o model.Object) {
		if p := model.Str(spec, "target", "creationPolicy"); p != "" && p != "Owner" {
			return // Merge, Orphan and None do not take ownership
		}
		target, _ := externalSecretTarget(spec, esName)
		key := nn(namespace, target)
		for _, have := range claims[key] {
			if have.name == esName {
				return
			}
		}
		claims[key] = append(claims[key], claim{esName, c, o})
	}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Group() != "external-secrets.io" {
				continue
			}
			switch o.Kind() {
			case "ExternalSecret":
				add(o.Namespace(), o.Name(), model.Get(o, "spec"), c, o)
			case "ClusterExternalSecret":
				esName := model.Str(o, "spec", "externalSecretName")
				if esName == "" {
					esName = o.Name()
				}
				for _, ns := range r.ix.Wiring.clusterExternalSecretNamespaces(o) {
					add(ns, esName, model.Get(o, "spec", "externalSecretSpec"), c, o)
				}
			}
		}
	}
	// one finding per pair of objects, however many namespaces they share
	type pair struct{ first, second string }
	shared := map[pair][]string{}
	second := map[pair]claim{}
	for key, cs := range claims {
		for i := 1; i < len(cs); i++ {
			p := pair{cs[0].o.String(), cs[i].o.String()}
			shared[p] = append(shared[p], key)
			second[p] = cs[i]
		}
	}
	pairs := make([]pair, 0, len(shared))
	for p := range shared {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].first+pairs[i].second < pairs[j].first+pairs[j].second })
	for _, p := range pairs {
		keys := shared[p]
		sort.Strings(keys)
		cl := second[p]
		r.report("FL-R011", cl.c, cl.o, fmt.Sprintf("and %s both create Secret %s and both want to own it: the operator refuses the second, which never becomes Ready", p.first, strings.Join(keys, ", ")),
			"give them different target names, or set target.creationPolicy: Merge on the one that only adds to the Secret")
	}
}

// certificateIssuers reports a cert-manager Certificate whose issuer nothing
// renders. The Certificate is accepted and stays pending; the Secret it should
// produce never appears, and every pod that mounts it waits.
func (r *run) certificateIssuers() {
	issuers := map[string]bool{}
	opaque := 0
	for _, c := range r.ix.Tree.Components {
		if c.Opaque != "" {
			opaque++
		}
		for _, o := range c.Objects {
			if o.Group() == "cert-manager.io" && (o.Kind() == "Issuer" || o.Kind() == "ClusterIssuer") {
				issuers[o.Kind()+"|"+o.Namespace()+"|"+o.Name()] = true
			}
		}
	}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Group() != "cert-manager.io" || o.Kind() != "Certificate" {
				continue
			}
			ref := model.Get(o, "spec", "issuerRef")
			if g := model.Str(ref, "group"); g != "" && g != "cert-manager.io" {
				continue // an external issuer: its kinds are not ours to know
			}
			kind, name, ns := model.Str(ref, "kind"), model.Str(ref, "name"), o.Namespace()
			if kind == "" {
				kind = "Issuer"
			}
			if kind == "ClusterIssuer" {
				ns = ""
			}
			if name == "" || issuers[kind+"|"+ns+"|"+name] {
				continue
			}
			what := kind + " " + name
			if ns != "" {
				what = kind + " " + nn(ns, name)
			}
			msg := fmt.Sprintf("is issued by %s, which nothing creates: the Certificate stays pending and Secret %s never appears", what, nn(o.Namespace(), model.Str(o, "spec", "secretName")))
			if opaque > 0 {
				r.reportAs(Warning, "FL-R012", c, o, msg, fmt.Sprintf("low confidence: %d component(s) are not rendered and may create it", opaque))
				continue
			}
			r.report("FL-R012", c, o, msg)
		}
	}
}
