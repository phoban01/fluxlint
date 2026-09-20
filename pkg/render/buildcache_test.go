package render

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/model"
)

const cacheKs = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: %s, namespace: flux-system}
spec:
  interval: 10m
  prune: true
  sourceRef: {kind: GitRepository, name: flux-system}
  path: ./%s
`

func cacheRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"clusters/prod/kustomization.yaml": "resources: [all.yaml]\n",
		"clusters/prod/all.yaml": "---\n" + sprintf(cacheKs, "a", "a") + "---\n" + sprintf(cacheKs, "b", "b") +
			"---\n" + sprintf(cacheKs, "c", "c"),
		// a: has a kustomization.yaml that reaches outside its directory
		"a/kustomization.yaml": "resources: [cm.yaml, ../shared/ns.yaml]\n",
		"a/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: shared}\ndata: {k: v}\n",
		"shared/ns.yaml":       "apiVersion: v1\nkind: Namespace\nmetadata: {name: shared}\n",
		// b: generated kustomization
		"b/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b, namespace: shared}\n",
		// c: untouched throughout
		"c/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: shared}\n",
	}
	for name, content := range files {
		writeFile(t, root, name, content)
	}
	return root
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func renderAll(t *testing.T, root string, cache *BuildCache) map[string][]model.Object {
	t.Helper()
	cfg, err := config.Load(filepath.Join(root, "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var opts []Option
	if cache != nil {
		opts = append(opts, WithBuildCache(cache))
	}
	tree, err := Tree(context.Background(), root, "clusters/prod", cfg, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]model.Object{}
	for _, c := range tree.Components {
		if c.BuildErr != nil {
			t.Fatalf("%s: %v", c, c.BuildErr)
		}
		out[c.Key()] = c.Objects
	}
	return out
}

// Every way a second tree can differ from the first must either miss the
// cache or give the same answer as a render without one.
func TestBuildCacheIsInvisible(t *testing.T) {
	changes := map[string]func(t *testing.T, root string){
		"nothing": func(*testing.T, string) {},
		"file in the directory": func(t *testing.T, r string) {
			writeFile(t, r, "a/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a2, namespace: shared}\n")
		},
		"file outside the directory": func(t *testing.T, r string) {
			writeFile(t, r, "shared/ns.yaml", "apiVersion: v1\nkind: Namespace\nmetadata: {name: other}\n")
		},
		"file added to generated dir": func(t *testing.T, r string) {
			writeFile(t, r, "b/extra.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n")
		},
		"kustomization.yaml appears": func(t *testing.T, r string) {
			writeFile(t, r, "b/kustomization.yaml", "resources: [cm.yaml]\nnamePrefix: x-\n")
		},
		"file becomes ignored": func(t *testing.T, r string) { writeFile(t, r, ".sourceignore", "c/cm.yaml\n") },
	}
	wantHits := map[string]int{"nothing": 4, "file in the directory": 3, "file outside the directory": 3,
		"file added to generated dir": 3, "kustomization.yaml appears": 3, "file becomes ignored": 3}

	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cache := NewBuildCache()
			renderAll(t, cacheRepo(t), cache)
			if hits, misses := cache.Stats(); hits != 0 || misses != 4 {
				t.Fatalf("first render: %d hits, %d misses, want 0 and 4", hits, misses)
			}

			second := cacheRepo(t)
			change(t, second)
			got := renderAll(t, second, cache)
			want := renderAll(t, second, nil)
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			if string(g) != string(w) {
				t.Errorf("cached render differs from a fresh one:\n got %s\nwant %s", g, w)
			}
			if hits, _ := cache.Stats(); hits != wantHits[name] {
				t.Errorf("%d builds reused, want %d", hits, wantHits[name])
			}
		})
	}
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
