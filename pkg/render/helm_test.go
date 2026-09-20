package render

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestValuesFromTargetPath(t *testing.T) {
	r := &renderer{data: map[string]map[string]string{
		"ConfigMap/apps/settings": {"replicas": "3", "version": `"1.30"`, "hosts": "a.example,b.example"},
		"Secret/apps/token":       {"values.yaml": "s3cret"},
	}}
	hr := model.Object{
		"metadata": map[string]any{"name": "web", "namespace": "apps"},
		"spec": map[string]any{
			"values": map[string]any{"image": map[string]any{"tag": "inline"}},
			"valuesFrom": []any{
				map[string]any{"kind": "ConfigMap", "name": "settings", "valuesKey": "replicas", "targetPath": "replicaCount"},
				map[string]any{"kind": "ConfigMap", "name": "settings", "valuesKey": "version", "targetPath": "image.tag"},
				map[string]any{"kind": "ConfigMap", "name": "settings", "valuesKey": "hosts", "targetPath": "ingress.hosts"},
				map[string]any{"kind": "Secret", "name": "token", "targetPath": "auth.token"},
				map[string]any{"kind": "ConfigMap", "name": "settings", "valuesKey": "absent", "targetPath": "x"},
			},
		},
	}
	s := r.helmSpecOf(hr)
	want := map[string]any{
		"replicaCount": int64(3),                        // --set semantics: typed
		"image":        map[string]any{"tag": "inline"}, // inline values win
		"ingress":      map[string]any{"hosts": "a.example,b.example"},
		"auth":         map[string]any{"token": "s3cret"},
	}
	if !reflect.DeepEqual(s.Values, want) {
		t.Errorf("values:\n got %#v\nwant %#v", s.Values, want)
	}
	if len(s.Notes) != 1 {
		t.Errorf("want one note for the missing key, got %q", s.Notes)
	}
}

func TestSetPathValueQuotedStaysString(t *testing.T) {
	v := map[string]any{}
	if err := setPathValue(v, "kube.version", `"1.30"`); err != nil {
		t.Fatal(err)
	}
	if got := v["kube"].(map[string]any)["version"]; got != "1.30" {
		t.Errorf("got %#v, want the string 1.30", got)
	}
}

func TestAddDependenciesOfChartDirectory(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"app/Chart.yaml": `apiVersion: v2
name: app
version: 1.0.0
dependencies:
  - {name: common, version: 1.x, repository: "file://../common"}
  - {name: cache, version: 2.0.0, repository: "https://charts.example.test"}
  - {name: gone, version: 1.0.0, repository: "https://charts.example.test"}
`,
		"app/templates/cm.yaml":           "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n",
		"common/Chart.yaml":               "apiVersion: v2\nname: common\nversion: 1.2.0\n",
		"common/templates/cm.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: common}\n",
		"fetched/cache/Chart.yaml":        "apiVersion: v2\nname: cache\nversion: 2.0.0\n",
		"fetched/cache/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cache}\n",
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
	dir := filepath.Join(root, "app")
	ch, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	notes := addDependencies(ch, dir, func(repoURL, name, version string) (string, error) {
		if name == "cache" {
			return filepath.Join(root, "fetched", "cache"), nil
		}
		return "", fmt.Errorf("chart %s not found in %s", name, repoURL)
	})
	if len(notes) != 1 || !strings.Contains(notes[0], "gone") {
		t.Errorf("want one note about the dependency that cannot be fetched, got %q", notes)
	}
	objs, err := helmTemplate(ch, helmSpec{ReleaseName: "app", Namespace: "apps", Values: map[string]any{}}, "1.35.0")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range objs {
		names = append(names, o.Name())
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "app,cache,common" {
		t.Errorf("rendered %s, want app,cache,common", got)
	}
}
