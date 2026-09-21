package lint

import (
	"sort"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

// A Flux Kustomization or HelmRelease with spec.kubeConfig applies to another
// cluster. Objects in different clusters never meet: two cert-managers, one
// per cluster, are not one object with two owners, and a Secret in one does
// not satisfy a pod in the other. Ordering is different. dependsOn, wait and
// timeouts live in the cluster Flux runs in and span all of them.
//
// So there is one index per cluster for everything that matches objects by
// name, and one merged index for the graph.

// clusterIndexes returns an index per target cluster, the local one first.
func clusterIndexes(t *model.Tree, cfg *config.Config) []*Index {
	byCluster := map[string][]*model.Component{}
	for _, c := range t.Components {
		byCluster[c.Cluster] = append(byCluster[c.Cluster], c)
	}
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	sort.Strings(names) // "" sorts first
	out := make([]*Index, 0, len(names))
	for _, name := range names {
		view := *t
		view.Components = byCluster[name] // ByKey stays whole: dependsOn crosses clusters
		out = append(out, buildIndex(&view, cfg, t.Components))
	}
	return out
}

// mergeIndexes is the view of the whole tree that the graph rules need: every
// import and every unmet import, each resolved within its own cluster.
func mergeIndexes(t *model.Tree, cfg *config.Config, parts []*Index) *Index {
	if len(parts) == 1 {
		return parts[0]
	}
	g := &Index{Tree: t, Cfg: cfg, Owner: map[string][]*model.Component{},
		Namespaces: parts[0].Namespaces, CRDs: parts[0].CRDs, Sources: parts[0].Sources, Wiring: parts[0].Wiring}
	for _, p := range parts {
		cluster := ""
		if len(p.Tree.Components) > 0 {
			cluster = p.Tree.Components[0].Cluster
		}
		for id, owners := range p.Owner {
			g.Owner[cluster+"\x00"+id] = owners
		}
		g.Needs = append(g.Needs, p.Needs...)
		g.MissingNamespaces = append(g.MissingNamespaces, p.MissingNamespaces...)
		g.MissingCRDs = append(g.MissingCRDs, p.MissingCRDs...)
		g.MissingSources = append(g.MissingSources, p.MissingSources...)
		g.Unresolved = append(g.Unresolved, p.Unresolved...)
	}
	return g
}

// Built-in kinds that have no namespace. The API server ignores
// metadata.namespace on them, and charts do set it by mistake.
var clusterScopedKinds = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true, "CustomResourceDefinition": true,
	"StorageClass": true, "PriorityClass": true, "IngressClass": true, "RuntimeClass": true,
	"APIService": true, "VolumeAttachment": true, "CSIDriver": true, "CSINode": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
	"MutatingAdmissionPolicy": true, "MutatingAdmissionPolicyBinding": true,
	"CertificateSigningRequest": true, "FlowSchema": true, "PriorityLevelConfiguration": true,
	"ClusterRole": true, "ClusterRoleBinding": true, "DeviceClass": true,
}

// clusterScoped reports whether o has no namespace whatever its metadata
// says: a cluster-scoped built-in kind, or a custom kind whose rendered CRD
// says scope: Cluster.
func clusterScoped(o model.Object, crdScope map[string]string) bool {
	if builtinGroups[o.Group()] {
		return clusterScopedKinds[o.Kind()]
	}
	return crdScope[o.GK()] == "Cluster"
}
