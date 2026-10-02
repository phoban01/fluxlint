package render

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/source"
)

// builtinGroup reports whether an API group is served by the API server
// itself: no dot, or under k8s.io.
func builtinGroup(g string) bool {
	return !strings.Contains(g, ".") || strings.HasSuffix(g, ".k8s.io")
}

// loadKubeSchemas fetches, for the Kubernetes release the entrypoint targets,
// the published API schema of every built-in group and version the tree uses.
func (r *renderer) loadKubeSchemas(ctx context.Context, t *model.Tree) {
	if r.resolver == nil {
		return
	}
	release, err := source.KubeRelease(r.cfg.KubeVersion)
	if err != nil {
		t.SchemaNote = err.Error()
		return
	}
	used := map[string]bool{}
	for _, c := range t.Components {
		for _, o := range c.Objects {
			if builtinGroup(o.Group()) && o.Version() != "" {
				used[o.Group()+"/"+o.Version()] = true
			}
		}
	}
	gvs := make([]string, 0, len(used))
	for gv := range used {
		gvs = append(gvs, gv)
	}
	sort.Strings(gvs)

	docs := make([][]byte, len(gvs))
	errs := make([]error, len(gvs))
	var wg sync.WaitGroup
	for i, gv := range gvs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			group, version, _ := strings.Cut(gv, "/")
			docs[i], errs[i] = r.resolver.KubeSchema(ctx, r.cfg.Sources.KubernetesSchemas, release, group, version)
		}()
	}
	wg.Wait()
	t.KubeRelease, t.KubeSchemas = release, map[string][]byte{}
	for i, gv := range gvs {
		switch {
		case errs[i] == nil:
			t.KubeSchemas[gv] = docs[i]
		case errors.Is(errs[i], source.ErrNotFound):
			t.KubeSchemas[gv] = nil
		default:
			// all or nothing: half a set of schemas would make the result
			// depend on which fetch happened to fail
			t.KubeRelease, t.KubeSchemas, t.SchemaNote = "", nil, errs[i].Error()
			return
		}
	}
}

// apiVersions is what a cluster of the configured Kubernetes release serves,
// for charts that ask. Loaded once; nil when it cannot be, and Helm's
// built-in list is used instead.
func (r *renderer) apiVersions(ctx context.Context) []string {
	r.apiOnce.Do(func() {
		if r.resolver == nil {
			return
		}
		release, err := source.KubeRelease(r.cfg.KubeVersion)
		if err != nil {
			return
		}
		if r.apis, err = r.resolver.KubeAPIVersions(ctx, r.cfg.Sources.KubernetesSchemas, release); err != nil {
			r.apiNote = "charts were rendered with Helm's built-in list of API versions, not the one Kubernetes " +
				strings.TrimPrefix(release, "v") + " serves: " + err.Error()
		}
	})
	return r.apis
}
