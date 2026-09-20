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

	if _, err := generateResources(dir, nil); err == nil {
		t.Fatal("without the ignore rules the scan fails, as it does in Flux")
	}
	src := model.Object{"spec": map[string]any{"ignore": "/apps/drafts/\n"}}
	got, err := generateResources(dir, ignoreFilter(root, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "apps/cm.yaml") {
		t.Errorf("got %v, want only apps/cm.yaml", got)
	}
}
