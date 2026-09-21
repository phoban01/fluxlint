package lint

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
)

// Kyverno registers its admission webhooks itself, once it runs, so they are
// in no manifest. What it will register follows from two things that are: the
// chart (the admission Service, and the ConfigMap with the webhooks' namespace
// selector) and the policies (the kinds they match, and their failurePolicy).

// kindPattern is one entry of a Kyverno match.resources.kinds list:
// Kind, version/Kind or group/version/Kind, each part possibly a wildcard.
type kindPattern struct{ group, version, kind string }

var versionLike = regexp.MustCompile(`^(v[0-9]|\*$)`)

func parseKind(s string) (kindPattern, bool) {
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 1:
		return kindPattern{"*", "*", parts[0]}, true
	case len(parts) == 2 && versionLike.MatchString(parts[0]):
		return kindPattern{"*", parts[0], parts[1]}, true
	case len(parts) == 3 && versionLike.MatchString(parts[1]):
		return kindPattern{parts[0], parts[1], parts[2]}, true
	}
	return kindPattern{}, false // Kind/subresource: not something Flux applies
}

func (k kindPattern) matches(o model.Object) bool {
	ok := func(pattern, s string) bool { m, _ := path.Match(pattern, s); return m }
	return ok(k.kind, o.Kind()) && ok(k.version, o.Version()) && ok(k.group, o.Group())
}

// kyvernoGates returns the fail-closed webhooks a rendered Kyverno will
// register.
func (r *run) kyvernoGates() []gate {
	var owner *model.Component
	var service model.Object
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			l := labelsOf(o)
			if o.Group() == "" && o.Kind() == "Service" && !strings.HasSuffix(o.Name(), "-metrics") && l["app.kubernetes.io/component"] == "admission-controller" &&
				strings.Contains(l["app.kubernetes.io/part-of"]+l["app.kubernetes.io/instance"]+o.Name(), "kyverno") {
				owner, service = c, o
			}
		}
	}
	if owner == nil {
		return nil
	}
	ns := service.Namespace()
	client := map[string]any{"service": map[string]any{"name": service.Name(), "namespace": ns}}

	// its own policy kinds always go through a fail-closed webhook
	gates := []gate{{owner: owner, config: service,
		hook: map[string]any{"name": "validate-policy.kyverno.svc", "clientConfig": client,
			"rules": []any{map[string]any{"apiGroups": []any{"kyverno.io", "policies.kyverno.io"}, "apiVersions": []any{"*"},
				"resources": []any{"*"}, "operations": []any{"CREATE", "UPDATE"}}}},
		why: "Kyverno registers this webhook for its own policy kinds when it starts"}}

	// the namespaces its resource webhooks leave alone
	excluded := []any{ns, "kube-system", "kube-public", "kube-node-lease"}
	selector := map[string]any{"matchExpressions": []any{
		map[string]any{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": excluded}}}
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			if o.Kind() != "ConfigMap" || o.Namespace() != ns {
				continue
			}
			raw := model.Str(o, "data", "webhooks")
			if raw == "" {
				continue
			}
			var one map[string]any
			var many []map[string]any
			if json.Unmarshal([]byte(raw), &one) != nil && json.Unmarshal([]byte(raw), &many) == nil && len(many) > 0 {
				one = many[0]
			}
			if sel, ok := one["namespaceSelector"].(map[string]any); ok {
				exprs, _ := sel["matchExpressions"].([]any)
				sel["matchExpressions"] = append(exprs, selector["matchExpressions"].([]any)...)
				selector = sel
			}
		}
	}

	var kinds []kindPattern
	var rules []any
	var from []string
	for _, c := range r.ix.Tree.Components {
		for _, o := range c.Objects {
			switch {
			case o.Group() == "kyverno.io" && (o.Kind() == "ClusterPolicy" || o.Kind() == "Policy"):
				if model.Str(o, "spec", "failurePolicy") == "Ignore" || model.Get(o, "spec", "admission") == false {
					continue
				}
				n := len(kinds)
				for _, rule := range model.List(o, "spec", "rules") {
					blocks := append(model.List(rule, "match", "any"), model.List(rule, "match", "all")...)
					blocks = append(blocks, model.Get(rule, "match"))
					for _, b := range blocks {
						for _, k := range model.List(b, "resources", "kinds") {
							if p, ok := parseKind(fmt.Sprint(k)); ok {
								kinds = append(kinds, p)
							}
						}
					}
				}
				if len(kinds) > n {
					from = append(from, o.String())
				}
			case o.Group() == "policies.kyverno.io" && strings.HasSuffix(o.Kind(), "Policy"):
				if model.Str(o, "spec", "failurePolicy") == "Ignore" {
					continue
				}
				if rr := model.List(o, "spec", "matchConstraints", "resourceRules"); len(rr) > 0 {
					rules = append(rules, rr...)
					from = append(from, o.String())
				}
			}
		}
	}
	if len(kinds) == 0 && len(rules) == 0 {
		return gates
	}
	sort.Strings(from)
	if len(from) > 4 {
		from = append(from[:4], fmt.Sprintf("and %d more", len(from)-4))
	}
	return append(gates, gate{owner: owner, config: service, kinds: kinds,
		hook: map[string]any{"name": "mutate.kyverno.svc-fail", "clientConfig": client, "rules": rules, "namespaceSelector": selector},
		why:  "Kyverno registers this webhook when it starts, for the kinds its fail-closed policies match: " + strings.Join(from, ", ")})
}
