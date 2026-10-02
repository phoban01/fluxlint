package lint

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

// A Cluster API ClusterResourceSet delivers objects to workload clusters. It
// names Secrets or ConfigMaps in its own namespace whose values are manifests,
// and Cluster API applies those manifests to every Cluster of that namespace
// that its clusterSelector matches. The manifests are therefore in Git, one
// level down: in a Secret's data, or in the target.template of the
// ExternalSecret that produces the Secret. Following them says what a workload
// cluster contains besides what Flux applies to it through spec.kubeConfig.

const resourceSetGroup = "addons.cluster.x-k8s.io"

var templateAction = regexp.MustCompile(`\{\{[^}]*\}\}`)

// manifestsIn parses the documents of one value of a resource Secret or
// ConfigMap. Template actions of an ExternalSecret become a placeholder: the
// value is unknown, the object and its keys are not.
func manifestsIn(text string) []model.Object {
	text = templateAction.ReplaceAllString(text, "templated")
	var out []model.Object
	for _, doc := range strings.Split("\n"+text, "\n---") {
		var o model.Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil || o == nil || o.Kind() == "" || o.Name() == "" {
			continue
		}
		out = append(out, o)
	}
	return out
}

// payloads maps "namespace/name" of every Secret and ConfigMap in the local
// cluster that can be a ClusterResourceSet resource to the manifests it holds.
func payloads(local []*model.Component, w *wiring) map[string][]model.Object {
	out := map[string][]model.Object{}
	add := func(kind, namespace, name string, values map[string]any, encoded bool) {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			text := fmt.Sprint(values[k])
			if encoded {
				b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
				if err != nil {
					continue
				}
				text = string(b)
			}
			key := kind + "|" + nn(namespace, name)
			out[key] = append(out[key], manifestsIn(text)...)
		}
	}
	values := func(o any, path ...string) map[string]any {
		m, _ := model.Get(o, path...).(map[string]any)
		return m
	}
	for _, c := range local {
		for _, o := range c.Objects {
			switch {
			case o.Group() == "" && o.Kind() == "Secret":
				add("Secret", o.Namespace(), o.Name(), values(o, "stringData"), false)
				add("Secret", o.Namespace(), o.Name(), values(o, "data"), true)
			case o.Group() == "" && o.Kind() == "ConfigMap":
				add("ConfigMap", o.Namespace(), o.Name(), values(o, "data"), false)
			case o.Group() == "external-secrets.io" && o.Kind() == "ExternalSecret":
				name, _ := externalSecretTarget(model.Get(o, "spec"), o.Name())
				add("Secret", o.Namespace(), name, values(o, "spec", "target", "template", "data"), false)
			case o.Group() == "external-secrets.io" && o.Kind() == "ClusterExternalSecret":
				esName := model.Str(o, "spec", "externalSecretName")
				if esName == "" {
					esName = o.Name()
				}
				spec := model.Get(o, "spec", "externalSecretSpec")
				name, _ := externalSecretTarget(spec, esName)
				// its namespaces are often selected by labels that are set at runtime;
				// a ClusterResourceSet that names the Secret settles where it is
				add("Secret", "*", name, values(spec, "target", "template", "data"), false)
				for _, ns := range w.clusterExternalSecretNamespaces(o) {
					add("Secret", ns, name, values(spec, "target", "template", "data"), false)
				}
			}
		}
	}
	return out
}

// resourceSetComponents returns, per target cluster, a synthetic component
// for every ClusterResourceSet that delivers to it, holding the objects it
// delivers. They take part in everything that matches objects by name and in
// nothing that orders components: Cluster API applies them, not Flux.
func resourceSetComponents(byCluster map[string][]*model.Component, w *wiring) map[string][]*model.Component {
	local := byCluster[""]
	var held map[string][]model.Object
	capiLabels := map[string]map[string]string{} // "namespace/name" of rendered CAPI Clusters
	for _, c := range local {
		for _, o := range c.Objects {
			if o.Group() == "cluster.x-k8s.io" && o.Kind() == "Cluster" {
				capiLabels[nn(o.Namespace(), o.Name())] = labelsOf(o)
			}
		}
	}
	out := map[string][]*model.Component{}
	for _, c := range local {
		for _, o := range c.Objects {
			if o.Group() != resourceSetGroup || o.Kind() != "ClusterResourceSet" {
				continue
			}
			if held == nil {
				held = payloads(local, w)
			}
			var objs []model.Object
			for _, res := range model.List(o, "spec", "resources") {
				kind, name := model.Str(res, "kind"), model.Str(res, "name")
				found := held[kind+"|"+nn(o.Namespace(), name)]
				if len(found) == 0 {
					found = held[kind+"|"+nn("*", name)]
				}
				objs = append(objs, found...)
			}
			if len(objs) == 0 {
				continue
			}
			for cluster := range byCluster {
				// a target cluster is named after its kubeconfig Secret, which
				// Cluster API calls <cluster>-kubeconfig, in the Cluster's namespace
				ns, secret, _ := strings.Cut(cluster, "/")
				if cluster == "" || ns != o.Namespace() {
					continue
				}
				secret, _, _ = strings.Cut(secret, "#")
				name := strings.TrimSuffix(secret, "-kubeconfig")
				// the Cluster object is often created by something else; when
				// it is in Git its labels decide, otherwise the namespace does
				if labels, known := capiLabels[nn(ns, name)]; known && !selectorMatches(model.Get(o, "spec", "clusterSelector"), labels) {
					continue
				}
				out[cluster] = append(out[cluster], &model.Component{
					Kind: model.KindKustomization, Namespace: o.Namespace(), Name: "ClusterResourceSet:" + o.Name(),
					Cluster: cluster, Synthetic: true, Objects: objs,
					Interval: c.Interval, RetryInterval: c.RetryInterval,
				})
			}
		}
	}
	return out
}
