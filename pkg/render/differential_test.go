package render

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	fluxkustomize "github.com/fluxcd/pkg/kustomize"
	"github.com/phoban01/fluxlint/pkg/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// fluxBuild renders dir exactly as kustomize-controller does: its Generator
// writes (or edits) kustomization.yaml in place — which is why this works on a
// copy — and SecureBuild runs kustomize.
func fluxBuild(t *testing.T, root, dir string, ks map[string]any) map[string]any {
	t.Helper()
	copyRoot := t.TempDir()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(copyRoot, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(copyRoot, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(copyRoot, dir)
	gen := fluxkustomize.NewGenerator(copyRoot, unstructured.Unstructured{Object: ks})
	if _, err := gen.WriteFile(target); err != nil {
		t.Fatalf("flux generator: %v", err)
	}
	rm, err := fluxkustomize.SecureBuild(copyRoot, target, false)
	if err != nil {
		t.Fatalf("flux build: %v", err)
	}
	out := map[string]any{}
	for _, r := range rm.Resources() {
		m, err := r.Map()
		if err != nil {
			t.Fatal(err)
		}
		out[model.Object(m).ID()] = normal(t, m)
	}
	return out
}

func normal(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fluxlint wraps the directory instead of editing it. The result must be
// what Flux itself would apply, object for object.
func TestBuildMatchesKustomizeController(t *testing.T) {
	testdata, _ := filepath.Abs(filepath.Join("..", "lint", "testdata"))
	local, _ := filepath.Abs("testdata")
	_ = local
	platform, _ := filepath.Abs(filepath.Join("..", "..", "e2e", "testdata", "platform"))
	cases := []struct {
		name, root, dir string
		spec            string
	}{
		{"generated kustomization, nested dirs, overlay", filepath.Join(testdata, "overlay"), "apps", `
targetNamespace: overlay-ns
nameSuffix: -green
patches:
  - target: {kind: ConfigMap, name: settings}
    patch: |
      - op: replace
        path: /data/value
        value: patched
`},
		{"images and namePrefix", filepath.Join(testdata, "overlay"), "apps", `
namePrefix: team-
images:
  - name: example.test/unused
    newTag: v2
`},
		// The way upstream operators are adapted: the operator's own kustomization
		// sets namePrefix and images, and the Flux Kustomization patches by the
		// ORIGINAL name, deletes an inline Secret, rewires env by index and adds a
		// suffix and an image tag. Flux edits the file in place; fluxlint wraps it.
		{"operator adapted by original name", filepath.Join(local, "operator"), "config/default", `
nameSuffix: -green
images:
  - name: controller
    newName: registry.example.test/operator
    newTag: v1.2.3
patches:
  - target: {group: apps, version: v1, kind: Deployment, name: manager}
    patch: |
      - op: add
        path: /spec/template/spec/imagePullSecrets
        value: [{name: regcred}]
      - op: replace
        path: /spec/template/spec/containers/0/env/1/valueFrom/secretKeyRef/name
        value: api-credentials
  - target: {version: v1, kind: Secret, name: manager-credentials}
    patch: |
      apiVersion: v1
      kind: Secret
      metadata:
        name: manager-credentials
        namespace: system
      $patch: delete
`},
		{"plain directory with a kustomization", filepath.Join(testdata, "clean"), "apps", ``},
		{"directory without kustomization", filepath.Join(testdata, "timing"), "a", ``},
		{"overlay on a base", platform, "infrastructure/production/controllers", ``},
		{"apps with nested bases", platform, "apps/production", `
patches:
  - target: {kind: HelmRelease, name: podinfo}
    patch: |
      apiVersion: helm.toolkit.fluxcd.io/v2
      kind: HelmRelease
      metadata:
        name: podinfo
      spec:
        values:
          replicaCount: 5
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{}
			if err := yaml.Unmarshal([]byte(tc.spec), &spec); err != nil {
				t.Fatal(err)
			}
			spec["path"] = "./" + tc.dir
			ks := map[string]any{
				"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
				"metadata": map[string]any{"name": "x", "namespace": "flux-system"},
				"spec":     spec,
			}
			want := fluxBuild(t, tc.root, tc.dir, ks)

			objs, _, err := build(filepath.Join(tc.root, tc.dir), overlayFromSpec(model.Object(ks)))
			if err != nil {
				t.Fatalf("fluxlint build: %v", err)
			}
			got := map[string]any{}
			for _, o := range objs {
				got[o.ID()] = normal(t, o)
			}

			ids := func(m map[string]any) []string {
				var out []string
				for k := range m {
					out = append(out, k)
				}
				sort.Strings(out)
				return out
			}
			if !reflect.DeepEqual(ids(got), ids(want)) {
				t.Fatalf("object sets differ:\nfluxlint: %v\nflux:     %v", ids(got), ids(want))
			}
			if len(want) == 0 {
				t.Fatal("nothing rendered: the case proves nothing")
			}
			for id := range want {
				if !reflect.DeepEqual(got[id], want[id]) {
					g, _ := yaml.Marshal(got[id])
					w, _ := yaml.Marshal(want[id])
					t.Errorf("%s differs\n--- fluxlint\n%s--- flux\n%s", id, g, w)
				}
			}
		})
	}
}
