package lint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

// keyed is a Secret or ConfigMap that something will create.
type keyed struct {
	Producer *model.Component
	By       model.Object // the object that leads to its creation
	// Keys is the closed set of keys, or nil when the producer does not
	// enumerate them (ExternalSecret dataFrom, templated data, …).
	Keys map[string]bool
}

// wiring indexes what workloads can mount or reference at runtime.
type wiring struct {
	secrets    map[string][]keyed // "namespace/name"
	configMaps map[string][]keyed
	accounts   map[string]*model.Component // "namespace/name"
	nsLabels   map[string]map[string]string
}

func nn(namespace, name string) string { return namespace + "/" + name }

func keysOf(o model.Object, fields ...string) map[string]bool {
	out := map[string]bool{}
	for _, f := range fields {
		if m, ok := model.Get(o, f).(map[string]any); ok {
			for k := range m {
				out[k] = true
			}
		}
	}
	return out
}

func buildWiring(ix *Index) *wiring {
	w := &wiring{
		secrets: map[string][]keyed{}, configMaps: map[string][]keyed{},
		accounts: map[string]*model.Component{}, nsLabels: map[string]map[string]string{},
	}
	for _, c := range ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() == "Namespace" && o.Group() == "" {
				labels := map[string]string{"kubernetes.io/metadata.name": o.Name()}
				if m, ok := model.Get(o, "metadata", "labels").(map[string]any); ok {
					for k, v := range m {
						labels[k] = fmt.Sprint(v)
					}
				}
				w.nsLabels[o.Name()] = labels
			}
		}
	}
	for _, e := range ix.Cfg.Externals.Secrets {
		var keys map[string]bool
		if len(e.Keys) > 0 {
			keys = map[string]bool{}
			for _, k := range e.Keys {
				keys[k] = true
			}
		}
		w.secrets[nn(e.Namespace, e.Name)] = append(w.secrets[nn(e.Namespace, e.Name)], keyed{Keys: keys})
	}

	for _, c := range ix.Tree.Components {
		for _, o := range c.Objects {
			ns, name := o.Namespace(), o.Name()
			switch {
			case o.Group() == "" && o.Kind() == "Secret":
				w.secrets[nn(ns, name)] = append(w.secrets[nn(ns, name)], keyed{c, o, keysOf(o, "data", "stringData")})
			case o.Group() == "" && o.Kind() == "ConfigMap":
				w.configMaps[nn(ns, name)] = append(w.configMaps[nn(ns, name)], keyed{c, o, keysOf(o, "data", "binaryData")})
			case o.Group() == "" && o.Kind() == "ServiceAccount":
				w.accounts[nn(ns, name)] = c
			case o.Group() == "external-secrets.io" && o.Kind() == "ExternalSecret":
				tn, keys := externalSecretTarget(model.Get(o, "spec"), name)
				w.secrets[nn(ns, tn)] = append(w.secrets[nn(ns, tn)], keyed{c, o, keys})
			case o.Group() == "external-secrets.io" && o.Kind() == "ClusterExternalSecret":
				esName := model.Str(o, "spec", "externalSecretName")
				if esName == "" {
					esName = name
				}
				tn, keys := externalSecretTarget(model.Get(o, "spec", "externalSecretSpec"), esName)
				for _, target := range w.clusterExternalSecretNamespaces(o) {
					w.secrets[nn(target, tn)] = append(w.secrets[nn(target, tn)], keyed{c, o, keys})
				}
			case o.Group() == "cert-manager.io" && o.Kind() == "Certificate":
				if sn := model.Str(o, "spec", "secretName"); sn != "" {
					keys := map[string]bool{"tls.crt": true, "tls.key": true, "ca.crt": true}
					for _, ks := range []string{"keystores"} {
						if model.Get(o, "spec", ks) != nil {
							keys = nil // keystore outputs add keys we do not enumerate
						}
					}
					w.secrets[nn(ns, sn)] = append(w.secrets[nn(ns, sn)], keyed{c, o, keys})
				}
			case o.Group() == "bitnami.com" && o.Kind() == "SealedSecret":
				tn := model.Str(o, "spec", "template", "metadata", "name")
				if tn == "" {
					tn = name
				}
				w.secrets[nn(ns, tn)] = append(w.secrets[nn(ns, tn)], keyed{c, o, keysOf(model.Object{"e": model.Get(o, "spec", "encryptedData")}, "e")})
			}
		}
	}
	return w
}

// externalSecretTarget returns the Secret an ExternalSecret spec creates and,
// when they can be enumerated, its keys.
func externalSecretTarget(spec any, defaultName string) (string, map[string]bool) {
	name := model.Str(spec, "target", "name")
	if name == "" {
		name = defaultName
	}
	if len(model.List(spec, "dataFrom")) > 0 {
		return name, nil // keys come from the remote store
	}
	keys := map[string]bool{}
	if tmpl, ok := model.Get(spec, "target", "template", "data").(map[string]any); ok {
		for k := range tmpl {
			keys[k] = true
		}
		if model.Str(spec, "target", "template", "mergePolicy") != "Merge" && len(tmpl) > 0 {
			return name, keys // a template replaces the fetched keys
		}
	}
	for _, d := range model.List(spec, "data") {
		if k := model.Str(d, "secretKey"); k != "" {
			keys[k] = true
		}
	}
	return name, keys
}

// clusterExternalSecretNamespaces evaluates spec.namespaces and the namespace
// selectors against the namespaces rendered from Git.
func (w *wiring) clusterExternalSecretNamespaces(o model.Object) []string {
	set := map[string]bool{}
	for _, n := range model.List(o, "spec", "namespaces") {
		if s, ok := n.(string); ok {
			set[s] = true
		}
	}
	selectors := model.List(o, "spec", "namespaceSelectors")
	if sel := model.Get(o, "spec", "namespaceSelector"); sel != nil {
		selectors = append(selectors, sel)
	}
	for ns, labels := range w.nsLabels {
		for _, sel := range selectors {
			if selectorMatches(sel, labels) {
				set[ns] = true
			}
		}
	}
	return sortedKeys(set)
}

// selectorMatches implements metav1.LabelSelector.
func selectorMatches(sel any, labels map[string]string) bool {
	if m, ok := model.Get(sel, "matchLabels").(map[string]any); ok {
		for k, v := range m {
			if labels[k] != fmt.Sprint(v) {
				return false
			}
		}
	}
	for _, e := range model.List(sel, "matchExpressions") {
		key, op := model.Str(e, "key"), model.Str(e, "operator")
		val, present := labels[key]
		in := false
		for _, v := range model.List(e, "values") {
			in = in || fmt.Sprint(v) == val
		}
		switch op {
		case "In":
			if !present || !in {
				return false
			}
		case "NotIn":
			if present && in {
				return false
			}
		case "Exists":
			if !present {
				return false
			}
		case "DoesNotExist":
			if present {
				return false
			}
		}
	}
	return true
}

// workload is a pod template found in an object.
type workload struct {
	Object   model.Object
	Labels   map[string]string
	Meta     any // pod template metadata
	Spec     any
	Replicas *int // nil when the kind has no replica count or it is unset
}

func podTemplate(o model.Object) (workload, bool) {
	var tmpl any
	switch {
	case o.Group() == "" && o.Kind() == "Pod":
		tmpl = map[string]any(o)
	case o.Group() == "apps" || (o.Group() == "batch" && o.Kind() == "Job"):
		tmpl = model.Get(o, "spec", "template")
	case o.Group() == "batch" && o.Kind() == "CronJob":
		tmpl = model.Get(o, "spec", "jobTemplate", "spec", "template")
	}
	spec := model.Get(tmpl, "spec")
	if spec == nil {
		return workload{}, false
	}
	w := workload{Object: o, Spec: spec, Meta: model.Get(tmpl, "metadata"), Labels: map[string]string{}}
	if m, ok := model.Get(tmpl, "metadata", "labels").(map[string]any); ok {
		for k, v := range m {
			w.Labels[k] = fmt.Sprint(v)
		}
	}
	switch n := model.Get(o, "spec", "replicas").(type) {
	case int:
		w.Replicas = &n
	case int64:
		v := int(n)
		w.Replicas = &v
	case float64:
		v := int(n)
		w.Replicas = &v
	}
	return w, true
}

// ref is one runtime reference made by a pod template.
type ref struct {
	Kind     string // Secret, ConfigMap, ServiceAccount
	Name     string
	Keys     []string // specific keys required, if any
	Optional bool
	Via      string // human readable location
}

func podRefs(spec any) []ref {
	var out []ref
	add := func(kind, name, via string, optional bool, keys ...string) {
		if name != "" {
			out = append(out, ref{Kind: kind, Name: name, Keys: keys, Optional: optional, Via: via})
		}
	}
	containers := append(append([]any{}, model.List(spec, "initContainers")...), model.List(spec, "containers")...)
	for _, c := range containers {
		cn := model.Str(c, "name")
		for _, e := range model.List(c, "env") {
			via := fmt.Sprintf("container %s env %s", cn, model.Str(e, "name"))
			if s := model.Get(e, "valueFrom", "secretKeyRef"); s != nil {
				add("Secret", model.Str(s, "name"), via, model.Bool(s, "optional"), model.Str(s, "key"))
			}
			if s := model.Get(e, "valueFrom", "configMapKeyRef"); s != nil {
				add("ConfigMap", model.Str(s, "name"), via, model.Bool(s, "optional"), model.Str(s, "key"))
			}
		}
		for _, e := range model.List(c, "envFrom") {
			via := fmt.Sprintf("container %s envFrom", cn)
			if s := model.Get(e, "secretRef"); s != nil {
				add("Secret", model.Str(s, "name"), via, model.Bool(s, "optional"))
			}
			if s := model.Get(e, "configMapRef"); s != nil {
				add("ConfigMap", model.Str(s, "name"), via, model.Bool(s, "optional"))
			}
		}
	}
	items := func(v any) []string {
		var keys []string
		for _, it := range model.List(v, "items") {
			keys = append(keys, model.Str(it, "key"))
		}
		return keys
	}
	for _, v := range model.List(spec, "volumes") {
		via := "volume " + model.Str(v, "name")
		if s := model.Get(v, "secret"); s != nil {
			add("Secret", model.Str(s, "secretName"), via, model.Bool(s, "optional"), items(s)...)
		}
		if s := model.Get(v, "configMap"); s != nil {
			add("ConfigMap", model.Str(s, "name"), via, model.Bool(s, "optional"), items(s)...)
		}
		for _, src := range model.List(v, "projected", "sources") {
			if s := model.Get(src, "secret"); s != nil {
				add("Secret", model.Str(s, "name"), via, model.Bool(s, "optional"), items(s)...)
			}
			if s := model.Get(src, "configMap"); s != nil {
				add("ConfigMap", model.Str(s, "name"), via, model.Bool(s, "optional"), items(s)...)
			}
		}
	}
	for _, s := range model.List(spec, "imagePullSecrets") {
		add("Secret", model.Str(s, "name"), "imagePullSecrets", false)
	}
	if sa := model.Str(spec, "serviceAccountName"); sa != "" && sa != "default" {
		add("ServiceAccount", sa, "serviceAccountName", false)
	}
	return out
}

func describeProducers(ps []keyed) string {
	var out []string
	for _, p := range ps {
		if p.By != nil {
			out = append(out, p.By.String())
		} else {
			out = append(out, "externals.secrets")
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
