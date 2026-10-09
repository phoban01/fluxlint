package lint

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// NeedAPI is an API a controller is allowed to use, read off its RBAC. Most
// controllers watch what their ClusterRole names and do not start without it,
// but RBAC also lists optional integrations, so on its own this justifies an
// ordering and never demands one. A contract (FL-C001) is how a component
// says which of them are hard requirements.
const NeedAPI NeedKind = "API"

type crdInfo struct {
	provider *model.Component
	kind     string
	served   map[string]bool
}

// crdsByResource indexes rendered CRDs by "group/plural", which is how RBAC
// names them.
func (ix *Index) crdsByResource() (byResource, byKind map[string]crdInfo) {
	byResource, byKind = map[string]crdInfo{}, map[string]crdInfo{}
	for _, c := range ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() != "CustomResourceDefinition" {
				continue
			}
			info := crdInfo{provider: c, kind: model.Str(o, "spec", "names", "kind"), served: map[string]bool{}}
			for _, v := range model.List(o, "spec", "versions") {
				if model.Bool(v, "served") {
					info.served[model.Str(v, "name")] = true
				}
			}
			g := model.Str(o, "spec", "group")
			byResource[g+"/"+model.Str(o, "spec", "names", "plural")] = info
			byKind[g+"/"+info.kind] = info
		}
	}
	return
}

// linkControllers turns the RBAC of every workload's ServiceAccount into API
// needs on the components that install those CRDs.
func (ix *Index) linkControllers() {
	byResource, _ := ix.crdsByResource()
	if len(byResource) == 0 {
		return
	}
	type roleKey struct{ kind, ns, name string }
	roles := map[roleKey]model.Object{}
	bound := map[string][]roleKey{} // "namespace/serviceaccount" -> roles
	for _, c := range ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Group() != "rbac.authorization.k8s.io" {
				continue
			}
			switch o.Kind() {
			case "Role", "ClusterRole":
				roles[roleKey{o.Kind(), o.Namespace(), o.Name()}] = o
			case "RoleBinding", "ClusterRoleBinding":
				rk := roleKey{model.Str(o, "roleRef", "kind"), "", model.Str(o, "roleRef", "name")}
				if rk.kind == "Role" {
					rk.ns = o.Namespace()
				}
				for _, s := range model.List(o, "subjects") {
					if model.Str(s, "kind") == "ServiceAccount" {
						ns := model.Str(s, "namespace")
						if ns == "" {
							ns = o.Namespace()
						}
						bound[nn(ns, model.Str(s, "name"))] = append(bound[nn(ns, model.Str(s, "name"))], rk)
					}
				}
			}
		}
	}
	for _, c := range ix.Tree.Components {
		seen := map[string]bool{}
		for _, o := range c.Objects {
			wl, ok := podTemplate(o)
			if !ok {
				continue
			}
			sa := model.Str(wl.Spec, "serviceAccountName")
			if sa == "" {
				sa = "default"
			}
			for _, rk := range bound[nn(o.Namespace(), sa)] {
				for _, rule := range model.List(roles[rk], "rules") {
					for _, g := range model.List(rule, "apiGroups") {
						for _, res := range model.List(rule, "resources") {
							resource, _, _ := strings.Cut(fmt.Sprint(res), "/") // drop subresources
							info, ok := byResource[fmt.Sprint(g)+"/"+resource]
							if !ok || c.Within(info.provider) || seen[info.provider.Key()+resource] {
								continue
							}
							seen[info.provider.Key()+resource] = true
							ix.Needs = append(ix.Needs, Need{Kind: NeedAPI, What: fmt.Sprint(g) + "/" + info.kind,
								Provider: info.provider, Consumer: c, Object: o})
						}
					}
				}
			}
		}
	}
}

func (r *run) controllerRules() {
	r.contracts()
	r.containerContracts()
	r.moduleSkew()
}

// containerContracts holds the containers a contract names to what their
// processes need: environment variables that are set, flags that are passed,
// and no flag this version does not accept.
func (r *run) containerContracts() {
	ix := r.ix
	for _, c := range ix.Tree.Components {
		if c.Contract == nil || len(c.Contract.Requires.Containers) == 0 {
			continue
		}
		for _, o := range c.Objects {
			wl, ok := podTemplate(o)
			if !ok {
				continue
			}
			containers := append(append([]any{}, model.List(wl.Spec, "initContainers")...), model.List(wl.Spec, "containers")...)
			for _, ctr := range containers {
				name, image := model.Str(ctr, "name"), model.Str(ctr, "image")
				for _, req := range c.Contract.Requires.Containers {
					if !selectsContainer(req, name, image) {
						continue
					}
					where := fmt.Sprintf("container %s of %s", name, o)
					for _, v := range req.Env {
						if !envSet(ctr, v, o.Namespace(), ix.Wiring) {
							r.report("FL-C001", c, o, fmt.Sprintf("its contract requires environment variable %s in %s, but nothing sets it", v, where))
						}
					}
					flags, seen := commandLine(ctr)
					if !seen {
						continue
					}
					for _, f := range req.Args.Required {
						if !flags[model.FlagName(f)] {
							r.report("FL-C001", c, o, fmt.Sprintf("its contract requires flag %s in %s, but the command line does not pass it", f, where))
						}
					}
					if len(req.Args.Accepted) == 0 {
						continue
					}
					accepted := map[string]bool{}
					for _, f := range req.Args.Accepted {
						accepted[model.FlagName(f)] = true
					}
					for _, f := range sortedKeys(flags) {
						if !accepted[f] {
							r.report("FL-C001", c, o, fmt.Sprintf("passes --%s to %s, which this version does not accept: the binary exits on an unknown flag", f, where),
								"remove the flag, or pin a version of the image that accepts it")
						}
					}
				}
			}
		}
	}
}

// selectsContainer reports whether a contract entry applies to a container.
// The image pattern is tried against the full reference and against the
// repository alone, so "registry/op" matches "registry/op:v1".
func selectsContainer(req model.ContractContainer, name, image string) bool {
	if req.Name != "" && req.Name != name {
		return false
	}
	if req.Image != "" {
		p := imageGlob(req.Image)
		return p.MatchString(image) || p.MatchString(imageRepository(image))
	}
	return true
}

// imageGlob compiles "registry.example.com/team/*" into a matcher. A "*"
// matches anything, including "/" and ":".
func imageGlob(p string) *regexp.Regexp {
	parts := strings.Split(p, "*")
	for i, s := range parts {
		parts[i] = regexp.QuoteMeta(s)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// imageRepository strips the tag and digest from an image reference.
func imageRepository(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

// envSet reports whether a container's process sees the variable with a
// value. An explicit entry wins over envFrom; one set to "" counts as unset,
// which is what Flux leaves behind for an undefined ${var}. An envFrom source
// that enumerates its keys must have the key; one that does not (an
// ExternalSecret dataFrom, …) may provide anything. A source that nothing
// creates is FL-R001's finding, not this one's.
func envSet(ctr any, name, namespace string, w *wiring) bool {
	set, found := false, false
	for _, e := range model.List(ctr, "env") {
		if model.Str(e, "name") != name {
			continue
		}
		found = true
		set = model.Get(e, "valueFrom") != nil || model.Str(e, "value") != ""
	}
	if found {
		return set
	}
	for _, src := range model.List(ctr, "envFrom") {
		key, ok := strings.CutPrefix(name, model.Str(src, "prefix"))
		if !ok {
			continue
		}
		var producers []keyed
		if s := model.Get(src, "secretRef"); s != nil {
			producers = w.secrets[nn(namespace, model.Str(s, "name"))]
		}
		if s := model.Get(src, "configMapRef"); s != nil {
			producers = w.configMaps[nn(namespace, model.Str(s, "name"))]
		}
		switch {
		case len(producers) == 0 && !model.Bool(src, "optional"):
			return true
		case anyHasKey(producers, key):
			return true
		}
	}
	return false
}

// commandLine is the set of flag names a container's command and args pass.
// seen is false when the container sets neither, so the image's own command
// line applies, or when the command is a shell and the flags are inside a
// script string; fluxlint cannot see either.
func commandLine(ctr any) (flags map[string]bool, seen bool) {
	command, args := model.Get(ctr, "command"), model.Get(ctr, "args")
	if command == nil && args == nil {
		return nil, false
	}
	tokens := append(append([]any{}, model.List(ctr, "command")...), model.List(ctr, "args")...)
	if len(tokens) > 0 {
		switch path.Base(fmt.Sprint(tokens[0])) {
		case "sh", "bash", "ash", "dash", "zsh":
			return nil, false
		}
	}
	flags = map[string]bool{}
	for _, tok := range tokens {
		s := fmt.Sprint(tok)
		if s == "--" {
			break
		}
		if strings.HasPrefix(s, "-") && len(s) > 1 {
			flags[model.FlagName(s)] = true
		}
	}
	return flags, true
}

// contracts checks what components declare they cannot run without.
func (r *run) contracts() {
	ix := r.ix
	_, byKind := ix.crdsByResource()
	runtime := map[string]*model.Component{}
	for _, rc := range r.cfg.Externals.RuntimeCRDs {
		runtime[rc.Group] = ix.Tree.ByKey[rc.ProvidedBy]
	}
	external := map[string]bool{}
	for _, g := range r.cfg.Externals.CRDGroups {
		external[g] = true
	}
	declared := declaredGraph(ix)

	for _, c := range ix.Tree.Components {
		if c.Contract == nil {
			continue
		}
		for _, req := range c.Contract.Requires.CRDs {
			what := req.Group + "/" + req.Kind
			info, rendered := byKind[what]
			provider := info.provider
			if !rendered {
				provider = runtime[req.Group]
			}
			switch {
			case provider == nil && external[req.Group]:
				continue
			case provider == nil:
				r.report("FL-C001", c, nil, fmt.Sprintf("its contract requires CRD %s, which no component installs: the controller will not start", what))
				continue
			case rendered && req.Version != "" && !info.served[req.Version]:
				r.report("FL-C001", c, nil, fmt.Sprintf("its contract requires %s %s, but %s installs a CRD that serves only %s",
					what, req.Version, provider, strings.Join(sortedKeys(info.served), ", ")))
				continue
			}
			if c.Within(provider) || declared.Reachable(readyOf(provider), startOf(c), nil) {
				continue
			}
			r.reportAs(Warning, "FL-C001", c, nil,
				fmt.Sprintf("its contract requires CRD %s from %s, but nothing orders them: the controller crash-loops until the CRD appears", what, provider),
				fmt.Sprintf("add dependsOn: %s to %s", provider.Name, c))
		}

		ns := contractNamespace(c)
		check := func(kind string, reqs []model.ContractConfig, index map[string][]keyed) {
			for _, req := range reqs {
				target := req.Namespace
				if target == "" {
					target = ns
				}
				producers := index[nn(target, req.Name)]
				if len(producers) == 0 {
					r.report("FL-C001", c, nil, fmt.Sprintf("its contract requires %s %s, which nothing creates", kind, nn(target, req.Name)))
					continue
				}
				var missing []string
				for _, k := range req.Keys {
					if !anyHasKey(producers, k) {
						missing = append(missing, k)
					}
				}
				if len(missing) > 0 {
					r.report("FL-C001", c, nil, fmt.Sprintf("its contract requires key %s of %s %s, but %s only defines %s",
						quoted(missing), kind, nn(target, req.Name), describeProducers(producers), definedKeys(producers)))
				}
			}
		}
		check("Secret", c.Contract.Requires.Secrets, ix.Wiring.secrets)
		check("ConfigMap", c.Contract.Requires.ConfigMaps, ix.Wiring.configMaps)
	}
}

// contractNamespace is where a component's workloads run.
func contractNamespace(c *model.Component) string {
	if ns := model.Str(c.Spec, "spec", "targetNamespace"); ns != "" {
		return ns
	}
	for _, o := range c.Objects {
		if _, ok := podTemplate(o); ok {
			return o.Namespace()
		}
	}
	return c.Namespace
}

// moduleSkew compares the API module version a controller was built against
// with the tag the same cluster pins for the component that installs those
// CRDs. Both facts come from go.mod files in the rendered sources.
func (r *run) moduleSkew() {
	providers := map[string]*model.Component{} // Go module path -> CRD-installing component
	for _, c := range r.ix.Tree.Components {
		if c.GoModule == "" {
			continue
		}
		for _, o := range c.Objects {
			if o.Kind() == "CustomResourceDefinition" {
				providers[c.GoModule] = c
				break
			}
		}
	}
	for _, c := range r.ix.Tree.Components {
		for mod, built := range c.GoRequires {
			p := providers[mod]
			if p == nil || p == c {
				continue
			}
			pinned := pinnedTag(p)
			if !semver.IsValid(pinned) || !semver.IsValid(built) || module.IsPseudoVersion(built) {
				continue
			}
			if semver.Compare(built, pinned) > 0 {
				r.report("FL-R007", c, nil, fmt.Sprintf("is built against %s %s, but %s installs its CRDs at %s: fields and versions the controller expects may not exist", mod, built, p, pinned),
					fmt.Sprintf("bump %s to at least %s, in a change that lands before this one", p, built))
			}
		}
	}
}

// pinnedTag extracts "v1.2.3" from a revision like "tag v1.2.3@sha1:…".
func pinnedTag(c *model.Component) string {
	rev, ok := strings.CutPrefix(c.SourceRevision, "tag ")
	if !ok {
		return ""
	}
	tag, _, _ := strings.Cut(rev, "@")
	return tag
}
