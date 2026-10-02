package lint

import (
	"sort"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

// NeedKind classifies what a component imports from another.
type NeedKind string

const (
	NeedNamespace NeedKind = "namespace"
	NeedCRD       NeedKind = "CRD"
	NeedSource    NeedKind = "source"
)

// Need is one import of Consumer that is exported by Provider.
type Need struct {
	Kind     NeedKind
	What     string
	Provider *model.Component
	Consumer *model.Component
	Object   model.Object
}

// Index is the export/import view of a rendered tree.
type Index struct {
	Tree  *model.Tree
	Cfg   *config.Config
	Owner map[string][]*model.Component // object ID -> owners

	Namespaces map[string][]*model.Component
	CRDs       map[string][]*model.Component // "group/Kind"
	Sources    map[string]*model.Component   // Ref.String()

	Needs []Need
	// Unprovided imports, deduplicated per component.
	MissingNamespaces []Need
	MissingCRDs       []Need
	MissingSources    []Need

	// Wiring and Unresolved describe what pods reference at runtime.
	Wiring     *wiring
	Unresolved []unresolved
}

var builtinNamespaces = map[string]bool{
	"default": true, "kube-system": true, "kube-public": true, "kube-node-lease": true,
}

var builtinGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "policy": true, "autoscaling": true,
	"rbac.authorization.k8s.io": true, "networking.k8s.io": true, "storage.k8s.io": true,
	"scheduling.k8s.io": true, "coordination.k8s.io": true, "node.k8s.io": true,
	"discovery.k8s.io": true, "admissionregistration.k8s.io": true,
	"apiextensions.k8s.io": true, "apiregistration.k8s.io": true,
	"certificates.k8s.io": true, "events.k8s.io": true, "authentication.k8s.io": true,
	"authorization.k8s.io": true, "flowcontrol.apiserver.k8s.io": true,
	"resource.k8s.io": true, "storagemigration.k8s.io": true,
}

// fluxGroups are provided by the Flux installation, which many repositories
// (Flux Operator, Terraform-managed bootstrap) do not keep in Git.
var fluxGroups = map[string]bool{
	model.FluxKustomizeGroup: true, model.FluxSourceGroup: true, model.FluxHelmGroup: true,
	"notification.toolkit.fluxcd.io": true, "image.toolkit.fluxcd.io": true,
	"fluxcd.controlplane.io": true,
}

// BuildIndex computes exports and resolves imports.
func BuildIndex(t *model.Tree, cfg *config.Config) *Index { return buildIndex(t, cfg, t.Components) }

// buildIndex indexes the components of t. Flux sources are looked up among
// all: a source is an object of the cluster Flux runs in, even when what is
// built from it is applied to another cluster.
func buildIndex(t *model.Tree, cfg *config.Config, all []*model.Component) *Index {
	ix := &Index{
		Tree: t, Cfg: cfg,
		Owner:      map[string][]*model.Component{},
		Namespaces: map[string][]*model.Component{},
		CRDs:       map[string][]*model.Component{},
		Sources:    map[string]*model.Component{},
	}
	for _, c := range t.Components {
		for _, o := range c.Objects {
			ix.Owner[o.ID()] = append(ix.Owner[o.ID()], c)
			switch {
			case o.Kind() == "Namespace" && o.Group() == "":
				ix.Namespaces[o.Name()] = append(ix.Namespaces[o.Name()], c)
			case o.Kind() == "CustomResourceDefinition":
				gk := model.Str(o, "spec", "group") + "/" + model.Str(o, "spec", "names", "kind")
				ix.CRDs[gk] = append(ix.CRDs[gk], c)
			}
		}
		// install.createNamespace creates the namespace if absent without
		// owning it, so it is an export but never a dual-ownership conflict
		if ns := c.CreatesNamespace; ns != "" {
			ix.Namespaces[ns] = append(ix.Namespaces[ns], c)
		}
	}

	for _, c := range all {
		for _, o := range c.Objects {
			if o.IsSource() {
				ref := model.Ref{Kind: o.Kind(), Namespace: o.Namespace(), Name: o.Name()}
				ix.Sources[ref.String()] = c
			}
		}
	}

	extNS := map[string]bool{cfg.RepoSource.Namespace: true}
	for _, n := range cfg.Externals.Namespaces {
		extNS[n] = true
	}
	runtimeCRDs := map[string]*model.Component{}
	for _, rc := range cfg.Externals.RuntimeCRDs {
		// A provider absent from this entrypoint is fine as long as nothing
		// here uses the group; if something does, it surfaces as FL-G005.
		if p := t.ByKey[rc.ProvidedBy]; p != nil {
			runtimeCRDs[rc.Group] = p
		}
	}
	extGroups := map[string]bool{}
	for _, g := range cfg.Externals.CRDGroups {
		extGroups[g] = true
	}

	crdScope := map[string]string{}
	for _, c := range t.Components {
		for _, o := range c.Objects {
			if o.Kind() == "CustomResourceDefinition" {
				crdScope[model.Str(o, "spec", "group")+"/"+model.Str(o, "spec", "names", "kind")] = model.Str(o, "spec", "scope")
			}
		}
	}
	for _, c := range t.Components {
		seen := map[string]bool{}
		once := func(k string) bool {
			if seen[k] {
				return false
			}
			seen[k] = true
			return true
		}
		for _, o := range c.Objects {
			if ns := o.Namespace(); ns != "" && !builtinNamespaces[ns] && !clusterScoped(o, crdScope) && once("ns|"+ns) {
				n := Need{Kind: NeedNamespace, What: ns, Consumer: c, Object: o}
				switch provs := ix.Namespaces[ns]; {
				case len(provs) > 0:
					ix.addNeed(n, provs)
				case !extNS[ns]:
					ix.MissingNamespaces = append(ix.MissingNamespaces, n)
				}
			}
			if g := o.Group(); !builtinGroups[g] && once("crd|"+o.GK()) {
				n := Need{Kind: NeedCRD, What: o.GK(), Consumer: c, Object: o}
				switch provs := ix.CRDs[o.GK()]; {
				case len(provs) > 0:
					ix.addNeed(n, provs)
				case runtimeCRDs[g] != nil:
					ix.addNeed(n, []*model.Component{runtimeCRDs[g]})
				case !extGroups[g] && !fluxGroups[g]:
					ix.MissingCRDs = append(ix.MissingCRDs, n)
				}
			}
			if ref, ok := sourceRefOf(o); ok && once("src|"+ref.String()) {
				n := Need{Kind: NeedSource, What: ref.String(), Consumer: c, Object: o}
				// the Kustomization / HelmRelease object itself applies fine; the
				// component it creates is what needs the source
				kind := model.KindKustomization
				if o.IsHelmRelease() {
					kind = model.KindHelmRelease
				}
				if child := t.ByKey[model.KeyFor(kind, o.Namespace(), o.Name())]; child != nil {
					n.Consumer = child
				}
				if p, found := ix.Sources[ref.String()]; found {
					ix.addNeed(n, []*model.Component{p})
				} else if !ix.isRepoSource(ref) {
					ix.MissingSources = append(ix.MissingSources, n)
				}
			}
		}
	}
	ix.linkRuntime()
	ix.linkControllers()
	return ix
}

func (ix *Index) isRepoSource(r model.Ref) bool {
	s := ix.Cfg.RepoSource
	return r.Kind == s.Kind && r.Name == s.Name && r.Namespace == s.Namespace
}

func (ix *Index) addNeed(n Need, provs []*model.Component) {
	for _, p := range provs {
		// Satisfied by construction: the same apply (Flux stages CRDs and
		// namespaces first), or an ancestor whose apply created the consumer.
		if n.Consumer.Within(p) {
			return
		}
	}
	n.Provider = provs[0]
	ix.Needs = append(ix.Needs, n)
}

// sourceRefOf extracts the Flux source a HelmRelease or Kustomization reads.
func sourceRefOf(o model.Object) (model.Ref, bool) {
	var at any
	switch {
	case o.IsFluxKustomization():
		at = model.Get(o, "spec", "sourceRef")
	case o.Group() == model.FluxHelmGroup && o.Kind() == "HelmRelease":
		if at = model.Get(o, "spec", "chartRef"); at == nil {
			at = model.Get(o, "spec", "chart", "spec", "sourceRef")
		}
	}
	if at == nil {
		return model.Ref{}, false
	}
	r := model.Ref{Kind: model.Str(at, "kind"), Name: model.Str(at, "name"), Namespace: model.Str(at, "namespace")}
	if r.Namespace == "" {
		r.Namespace = o.Namespace()
	}
	return r, r.Name != ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
