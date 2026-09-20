package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/model"
)

func TestIgnoredFilesAreNotScanned(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"apps/cm.yaml":              "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n",
		"apps/values.yaml":          "replicas: 3\n", // not a manifest: fails Flux's scan unless ignored
		"apps/charts/x/values.yaml": "replicas: 1\n",
		"apps/drafts/wip.yaml":      "not: a manifest\n",
		".sourceignore":             "values.yaml\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "apps")

	if _, err := generateResources(dir, nil, nil); err == nil {
		t.Fatal("without the ignore rules the scan fails, as it does in Flux")
	}
	src := model.Object{"spec": map[string]any{"ignore": "/apps/drafts/\n"}}
	got, err := generateResources(dir, ignoreFilter(root, src), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "apps/cm.yaml") {
		t.Errorf("got %v, want only apps/cm.yaml", got)
	}
}

// A kustomization.yaml that lists a file the source ignores fails in the
// cluster, because the file never reaches the artifact.
func TestKustomizationListingAnIgnoredFileFails(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"apps/kustomization.yaml": "resources:\n  - cm.yaml\n  - local/dev.yaml\n",
		"apps/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n",
		"apps/local/dev.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: dev}\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "apps")

	objs, _, err := build(dir, overlay{Ignored: ignoreFilter(root, nil)})
	if err != nil || len(objs) != 2 {
		t.Fatalf("nothing ignored: want 2 objects, got %d (%v)", len(objs), err)
	}
	src := model.Object{"spec": map[string]any{"ignore": "local/\n"}}
	_, _, err = build(dir, overlay{Ignored: ignoreFilter(root, src)})
	if err == nil || !strings.Contains(err.Error(), "local/dev.yaml") {
		t.Fatalf("want a build failure naming local/dev.yaml, got %v", err)
	}
}
