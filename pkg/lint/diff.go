package lint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

// Fingerprint identifies a finding independently of where it is reported, so
// that two commits can be compared and review tools can track it.
func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{f.Rule, f.Entrypoint, f.Component, f.Object, f.Message}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Compare analyses the transition from base to head. It marks head findings
// that already exist in base, and adds findings about what reconciling head on
// a cluster that runs base will do.
func Compare(base, head *Result, cfg *config.Config) {
	known := map[string]bool{}
	for _, f := range base.Findings {
		known[f.Fingerprint()] = true
	}
	for i := range head.Findings {
		head.Findings[i].Existing = known[head.Findings[i].Fingerprint()]
	}

	r := &run{ix: BuildIndex(head.Tree, cfg), cfg: cfg, files: map[string][]string{}}
	r.transitionRules(base.Tree, head.Tree)
	head.Findings = append(head.Findings, r.findings...)
	sortFindings(head.Findings)
}

type owned struct {
	c *model.Component
	o model.Object
}

func ownership(t *model.Tree) map[string]owned {
	out := map[string]owned{}
	for _, c := range t.Components {
		for _, o := range c.Objects {
			// the same name in another cluster is another object
			id := c.Cluster + "\x00" + o.ID()
			if _, dup := out[id]; !dup {
				out[id] = owned{c, o}
			}
		}
	}
	return out
}

// precious kinds hold state or contain everything else.
var precious = map[string]bool{
	"Namespace": true, "CustomResourceDefinition": true, "PersistentVolumeClaim": true,
	"PersistentVolume": true, "StatefulSet": true, "Secret": true,
}

func (r *run) transitionRules(base, head *model.Tree) {
	before, after := ownership(base), ownership(head)

	// A base component that head could not render tells us nothing about its
	// objects: do not mistake "not rendered" for "removed".
	unrendered := map[string]bool{}
	for _, c := range head.Components {
		if c.Opaque != "" {
			unrendered[c.Key()] = true
		}
	}

	// removed objects, grouped by the component that owned them
	removed := map[*model.Component][]model.Object{}
	for id, b := range before {
		if _, still := after[id]; !still && !unrendered[b.c.Key()] {
			removed[b.c] = append(removed[b.c], b.o)
		}
	}
	comps := make([]*model.Component, 0, len(removed))
	for c := range removed {
		comps = append(comps, c)
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Key() < comps[j].Key() })
	for _, c := range comps {
		objs := removed[c]
		sort.Slice(objs, func(i, j int) bool { return objs[i].String() < objs[j].String() })
		var names, dangerous []string
		for _, o := range objs {
			names = append(names, o.String())
			if precious[o.Kind()] {
				dangerous = append(dangerous, o.String())
			}
		}
		// the component itself may be gone, in which case its parent's prune decides
		pruner, at := c, head.ByKey[c.Key()]
		if at == nil && c.Parent != nil {
			pruner = c.Parent
		}
		prunes := pruner.Prune
		if h := head.ByKey[pruner.Key()]; h != nil {
			prunes = h.Prune
		}
		switch {
		case prunes && len(dangerous) > 0:
			r.reportAs(Warning, "FL-D001", head.ByKey[pruner.Key()], nil,
				fmt.Sprintf("this change makes Flux delete %d object(s) from %s, including state-bearing ones: %s", len(objs), c, strings.Join(dangerous, ", ")), names...)
		case prunes:
			r.report("FL-D001", head.ByKey[pruner.Key()], nil, fmt.Sprintf("this change makes Flux delete %d object(s) from %s", len(objs), c), names...)
		default:
			r.report("FL-D004", head.ByKey[pruner.Key()], nil,
				fmt.Sprintf("%d object(s) leave Git but %s has prune: false, so they stay in the cluster unmanaged", len(objs), pruner), names...)
		}
	}

	ids := make([]string, 0, len(after))
	for id := range after {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := after[id]
		b, existed := before[id]
		if !existed {
			continue
		}
		if b.c.Key() != a.c.Key() {
			detail := []string{}
			if b.c.Prune {
				detail = append(detail, fmt.Sprintf("%s has prune: true: if it reconciles after %s has applied the object, it deletes it; the next reconcile of %s recreates it", b.c, a.c, a.c))
			}
			r.report("FL-D003", a.c, a.o, fmt.Sprintf("moves from %s to %s", b.c, a.c), detail...)
		}
		if fields := immutableChanges(b.o, a.o); len(fields) > 0 && !model.Bool(a.c.Spec, "spec", "force") {
			r.report("FL-D002", a.c, a.o, "changes immutable field(s): the apply is rejected and the component stays NotReady until the object is deleted or the Kustomization sets force: true", fields...)
		}
	}
	r.updatePolicies(before, after)
}

// immutable lists, per kind, the paths the API server refuses to update.
var immutable = map[string][][]string{
	"apps/Deployment":                              {{"spec", "selector"}},
	"apps/DaemonSet":                               {{"spec", "selector"}},
	"apps/StatefulSet":                             {{"spec", "selector"}, {"spec", "serviceName"}, {"spec", "volumeClaimTemplates"}, {"spec", "podManagementPolicy"}},
	"batch/Job":                                    {{"spec", "selector"}, {"spec", "template"}, {"spec", "completions"}},
	"/PersistentVolumeClaim":                       {{"spec", "storageClassName"}, {"spec", "accessModes"}, {"spec", "volumeName"}, {"spec", "volumeMode"}},
	"/Service":                                     {{"spec", "clusterIP"}},
	"storage.k8s.io/StorageClass":                  {{"provisioner"}, {"parameters"}, {"reclaimPolicy"}, {"volumeBindingMode"}},
	"rbac.authorization.k8s.io/RoleBinding":        {{"roleRef"}},
	"rbac.authorization.k8s.io/ClusterRoleBinding": {{"roleRef"}},
}

func immutableChanges(before, after model.Object) []string {
	var out []string
	for _, path := range immutable[after.GK()] {
		x, y := model.Get(before, path...), model.Get(after, path...)
		if x == nil || y == nil { // unset -> set is defaulting territory, not an update
			continue
		}
		if !reflect.DeepEqual(normalise(x), normalise(y)) {
			out = append(out, fmt.Sprintf("%s: %s -> %s", strings.Join(path, "."), compact(x), compact(y)))
		}
	}
	return out
}

func normalise(v any) any {
	var out any
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity.rank() != b.Severity.rank() {
			return a.Severity.rank() < b.Severity.rank()
		}
		return a.Rule < b.Rule
	})
}
