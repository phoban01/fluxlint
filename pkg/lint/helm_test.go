package lint_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/source"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/repo"
)

// chartRepo serves a classic Helm repository with one chart, "widgets", in
// versions 1.0.0 and 1.1.0. The chart ships the Widget CRD in crds/ and a
// Deployment whose replica count and image tag come from values.
func chartRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, version := range []string{"1.0.0", "1.1.0"} {
		src := t.TempDir()
		write(t, src, "widgets/Chart.yaml", fmt.Sprintf("apiVersion: v2\nname: widgets\nversion: %s\n", version))
		write(t, src, "widgets/values.yaml", "replicas: 1\nimage:\n  tag: default\n")
		write(t, src, "widgets/crds/widget.yaml", widgetCRD)
		write(t, src, "widgets/templates/deploy.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-controller
  labels:
    chart-version: "{{ .Chart.Version }}"
spec:
  replicas: {{ .Values.replicas }}
  selector: {matchLabels: {app: widgets}}
  template:
    metadata: {labels: {app: widgets}}
    spec:
      containers:
        - name: manager
          image: "example.test/widgets:{{ .Values.image.tag }}"
`)
		write(t, src, "widgets/templates/role.yaml", "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: {{ .Release.Name }}\nrules: []\n")
		ch, err := loader.Load(filepath.Join(src, "widgets"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chartutil.Save(ch, dir); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	index, err := repo.IndexDirectory(dir, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(dir, "index.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	return srv.URL
}

// helmCluster: `operators` applies a HelmRepository, a values ConfigMap and the
// widgets HelmRelease; `widgets` applies a Widget custom resource.
func helmCluster(t *testing.T, repoURL, version string, widgetsDependsOn bool) string {
	t.Helper()
	dir := t.TempDir()
	dependsOn := ""
	if widgetsDependsOn {
		dependsOn = "  dependsOn:\n    - name: operators\n"
	}
	ks := func(name, extra string) string {
		return fmt.Sprintf(`---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: flux-system
spec:
  interval: 10m
  retryInterval: 1m
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  path: ./%s
%s`, name, name, extra)
	}
	write(t, dir, "clusters/prod/all.yaml", ks("operators", "  wait: true\n  timeout: 10m\n")+ks("widgets", dependsOn))
	write(t, dir, "operators/all.yaml", fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: widgets-system
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: widgets
  namespace: widgets-system
spec:
  interval: 1h
  url: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: widget-values
  namespace: widgets-system
data:
  values.yaml: |
    replicas: 2
    image:
      tag: from-configmap
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: widgets
  namespace: widgets-system
spec:
  interval: 1h
  chart:
    spec:
      chart: widgets
      version: %q
      sourceRef:
        kind: HelmRepository
        name: widgets
  valuesFrom:
    - kind: ConfigMap
      name: widget-values
  values:
    replicas: 3
`, repoURL, version))
	write(t, dir, "widgets/widget.yaml", "apiVersion: example.test/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: default\n")
	return dir
}

func TestHelmReleaseIsRenderedWithRealValues(t *testing.T) {
	repoURL := chartRepo(t)
	cache := t.TempDir()
	r := analyseExternal(t, helmCluster(t, repoURL, "1.0.0", true), source.Options{CacheDir: cache})
	if got := problems(r); len(got) != 0 {
		t.Fatalf("problems: %v\n%+v", got, r.Findings)
	}

	hr := component(t, r, "HelmRelease/widgets-system/widgets")
	byKind := map[string]model.Object{}
	for _, o := range hr.Objects {
		byKind[o.Kind()] = o
	}
	deploy := byKind["Deployment"]
	if deploy == nil || byKind["CustomResourceDefinition"] == nil || byKind["ClusterRole"] == nil {
		t.Fatalf("chart output incomplete: %v", hr.Objects)
	}
	// valuesFrom is merged first, inline values win
	if got := model.Get(deploy, "spec", "replicas"); fmt.Sprint(got) != "3" {
		t.Errorf("replicas = %v, want 3 (inline values win over valuesFrom)", got)
	}
	image := model.Str(model.List(deploy, "spec", "template", "spec", "containers")[0], "image")
	if image != "example.test/widgets:from-configmap" {
		t.Errorf("image = %q: valuesFrom ConfigMap not applied", image)
	}
	if deploy.Name() != "widgets-controller" || deploy.Namespace() != "widgets-system" {
		t.Errorf("release name/namespace: %s", deploy)
	}
	if ns := byKind["ClusterRole"].Namespace(); ns != "" {
		t.Errorf("cluster-scoped object was given namespace %q", ns)
	}

	// The CRD inside the chart now justifies widgets -> operators.
	if got := find(r, "FL-T004"); len(got) != 0 {
		t.Errorf("dependency is justified by the chart's CRD: %+v", got)
	}
	if got := find(r, "FL-G005"); len(got) != 0 {
		t.Errorf("CRD from the chart should be known: %+v", got)
	}
	// The release is scheduled with helm-controller's default 5m timeout. It
	// has slack: its wait: true parent allows 10m, which is what binds.
	s := r.Timing.Schedule
	ready := "ready(HelmRelease/widgets-system/widgets)"
	if got := s.Earliest[ready]; got != 5*time.Minute {
		t.Errorf("release ready bound = %v, want 5m", got)
	}
	if got := s.Slack(ready); got != 5*time.Minute {
		t.Errorf("release slack = %v, want 5m", got)
	}
}

func TestHelmChartCRDWithoutOrdering(t *testing.T) {
	r := analyseExternal(t, helmCluster(t, chartRepo(t), "1.0.0", false), source.Options{CacheDir: t.TempDir()})
	got := find(r, "FL-T006")
	if len(got) != 1 || !strings.Contains(got[0].Message, "example.test/Widget") ||
		!strings.Contains(got[0].Message, "HelmRelease/widgets-system/widgets") {
		t.Fatalf("Widget needs a CRD that only the chart installs, with no ordering: %+v", r.Findings)
	}
}

func TestHelmChartVersionRangeIsFloating(t *testing.T) {
	r := analyseExternal(t, helmCluster(t, chartRepo(t), ">=1.0.0", true), source.Options{CacheDir: t.TempDir()})
	hr := component(t, r, "HelmRelease/widgets-system/widgets")
	if hr.SourceRevision != "widgets 1.1.0" {
		t.Errorf("range should resolve to the newest chart, got %q", hr.SourceRevision)
	}
	if got := find(r, "FL-X002"); len(got) != 1 {
		t.Errorf("version range must be reported as floating: %+v", r.Findings)
	}
}

func TestHelmChartServedFromCacheOffline(t *testing.T) {
	repoURL := chartRepo(t)
	cache := t.TempDir()
	repoDir := helmCluster(t, repoURL, "1.0.0", true)
	analyseExternal(t, repoDir, source.Options{CacheDir: cache})

	r := analyseExternal(t, repoDir, source.Options{CacheDir: cache, Mode: source.Offline})
	if got := problems(r); len(got) != 0 {
		t.Fatalf("pinned chart must be served from cache: %v", got)
	}
	r = analyseExternal(t, repoDir, source.Options{CacheDir: t.TempDir(), Mode: source.Offline})
	if got := find(r, "FL-X001"); len(got) != 1 {
		t.Fatalf("cold cache offline must report the chart as unavailable: %+v", r.Findings)
	}
}

func TestHelmChartThatFailsToRender(t *testing.T) {
	repoDir := helmCluster(t, chartRepo(t), "1.0.0", true)
	// replicas as a map makes the Deployment template emit invalid YAML structure
	b, _ := os.ReadFile(filepath.Join(repoDir, "operators/all.yaml"))
	broken := strings.Replace(string(b), "    replicas: 3\n", "    image: not-a-map\n", 1)
	if err := os.WriteFile(filepath.Join(repoDir, "operators/all.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	r := analyseExternal(t, repoDir, source.Options{CacheDir: t.TempDir()})
	got := find(r, "FL-G008")
	if len(got) != 1 || got[0].Component != "HelmRelease/widgets-system/widgets" {
		t.Fatalf("a chart that cannot render with the given values must fail the build: %+v", r.Findings)
	}
}
