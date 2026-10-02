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
	objs, _, err := helmTemplate(ch, helmSpec{ReleaseName: "app", Namespace: "apps", Values: map[string]any{}}, "1.35.0")
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

func TestUnstableObjects(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: db\nversion: 1.0.0\n",
		"templates/password.yaml": `apiVersion: v1
kind: Secret
metadata: {name: db-password}
stringData:
  password: {{ randAlphaNum 24 | quote }}
`,
		// the usual way to keep a generated value: not reported, because the
		// value in the cluster cannot be seen from here
		"templates/kept.yaml": `{{- $old := lookup "v1" "Secret" .Release.Namespace "db-kept" }}
apiVersion: v1
kind: Secret
metadata: {name: db-kept}
stringData:
  password: {{ if $old }}{{ index $old.data "password" | b64dec | quote }}{{ else }}{{ randAlphaNum 24 | quote }}{{ end }}
`,
		"templates/rotate.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  name: rotate
  annotations: {helm.sh/hook: post-upgrade, started: {{ now | quote }}}
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{name: r, image: registry.example.test/r:1}]
`,
		"templates/stable.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: db-settings}\ndata: {mode: fast}\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, unstable, err := helmTemplate(ch, helmSpec{ReleaseName: "db", Namespace: "default", Values: map[string]any{}}, "1.35.0")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Secret/default/db-password", "batch/Job/default/rotate"}
	if strings.Join(unstable, " ") != strings.Join(want, " ") {
		t.Errorf("unstable = %v, want %v", unstable, want)
	}
}

// A chart that asks the cluster which APIs it serves must get the answer of
// the cluster's Kubernetes release, as helm-controller would give it, and not
// Helm's built-in list, which has no kinds and still has policy/v1beta1.
func TestChartSeesTheClustersAPIs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: web\nversion: 1.0.0\n",
		"templates/pdb.yaml": `{{- if .Capabilities.APIVersions.Has "policy/v1/PodDisruptionBudget" }}
apiVersion: policy/v1
{{- else }}
apiVersion: policy/v1beta1
{{- end }}
kind: PodDisruptionBudget
metadata: {name: web}
spec: {minAvailable: 1, selector: {matchLabels: {app: web}}}
{{- if .Capabilities.APIVersions.Has "monitoring.example.test/v1" }}
---
apiVersion: monitoring.example.test/v1
kind: Scrape
metadata: {name: web}
{{- end }}
`,
		"crds/thing.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: things.example.test}\nspec:\n  group: example.test\n  names: {kind: Thing, plural: things}\n  scope: Namespaced\n  versions: [{name: v1, served: true, storage: true, schema: {openAPIV3Schema: {type: object}}}]\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	render := func(apis []string) map[string]string {
		objs, _, err := helmTemplate(ch, helmSpec{ReleaseName: "web", Namespace: "default", Values: map[string]any{}, APIVersions: apis}, "1.35.0")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, o := range objs {
			out[o.Kind()] = o.APIVersion()
		}
		return out
	}
	if got := render(nil); got["PodDisruptionBudget"] != "policy/v1beta1" {
		t.Fatalf("the premise of this test is that Helm's built-in list lacks kinds: %v", got)
	}
	got := render([]string{"v1", "policy/v1", "policy/v1/PodDisruptionBudget"})
	if got["PodDisruptionBudget"] != "policy/v1" {
		t.Errorf("with the cluster's APIs the chart must choose policy/v1: %v", got)
	}
	if got["CustomResourceDefinition"] == "" {
		t.Errorf("CRDs from crds/ must still be included: %v", got)
	}
	if _, ok := got["Scrape"]; ok {
		t.Errorf("an API the cluster does not serve must not be assumed: %v", got)
	}
}
