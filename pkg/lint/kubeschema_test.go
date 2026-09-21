package lint_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
	"github.com/phoban01/fluxlint/pkg/source"
)

// release writes the API documents of one made-up Kubernetes release, laid
// out like the Kubernetes repository. jobFields are the fields of a Job's
// spec in that release; betaServed says whether batch/v1beta1 still exists.
func release(t *testing.T, dir, tag, jobFields string, betaServed bool) {
	t.Helper()
	meta := `"io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta": {"type": "object", "properties": {
      "name": {"type": "string"}, "namespace": {"type": "string"},
      "labels": {"type": "object", "additionalProperties": {"type": "string"}}}}`
	doc := func(group, version, kinds string) string {
		return fmt.Sprintf(`{"openapi": "3.0.0", "components": {"schemas": {%s, %s}}}`, meta, kinds)
	}
	job := func(version string) string {
		return fmt.Sprintf(`"io.k8s.api.batch.%[1]s.Job": {"type": "object",
      "x-kubernetes-group-version-kind": [{"group": "batch", "kind": "Job", "version": %[1]q}],
      "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"},
        "metadata": {"allOf": [{"$ref": "#/components/schemas/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"}]},
        "spec": {"type": "object", "required": ["template"], "properties": {%[2]s, "template": {"type": "object"}}}}}`, version, jobFields)
	}
	base := filepath.Join(dir, tag, "api", "openapi-spec", "v3")
	write(t, base, "api__v1_openapi.json", doc("", "v1", `"io.k8s.api.core.v1.ConfigMap": {"type": "object",
      "x-kubernetes-group-version-kind": [{"group": "", "kind": "ConfigMap", "version": "v1"}],
      "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"},
        "metadata": {"allOf": [{"$ref": "#/components/schemas/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"}]},
        "data": {"type": "object", "additionalProperties": {"type": "string"}}}}`))
	write(t, base, "apis__batch__v1_openapi.json", doc("batch", "v1", job("v1")))
	if betaServed {
		write(t, base, "apis__batch__v1beta1_openapi.json", doc("batch", "v1beta1", job("v1beta1")))
	}
}

const jobs = `apiVersion: batch/v1
kind: Job
metadata: {name: report, namespace: default}
spec:
  backoffLimit: 2
  podReplacementPolicy: Failed
  template: {}
---
apiVersion: batch/v1beta1
kind: Job
metadata: {name: legacy, namespace: default}
spec:
  backoffLimit: 2
  template: {}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings, namespace: default, labels: {tier: 1}}
data: {mode: fast}
`

// The same manifests, judged for the release each cluster runs.
func TestBuiltinsAgainstTheClustersRelease(t *testing.T) {
	schemas := t.TempDir()
	release(t, schemas, "v1.20.0", `"backoffLimit": {"type": "integer"}`, true)
	release(t, schemas, "v1.30.0", `"backoffLimit": {"type": "integer"}, "podReplacementPolicy": {"type": "string"}`, false)

	repo := t.TempDir()
	write(t, repo, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "jobs", "jobs", ""))
	write(t, repo, "jobs/all.yaml", jobs)

	check := func(kubeVersion string) string {
		res, err := source.New(source.Options{CacheDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.KubeVersion, cfg.Sources.KubernetesSchemas = kubeVersion, schemas
		tree, err := render.Tree(context.Background(), repo, "clusters/prod", cfg, res)
		if err != nil {
			t.Fatal(err)
		}
		return messages(find(lint.Run(tree, cfg), "FL-V003"))
	}

	old := check("1.20")
	for _, want := range []string{"Job/default/report", "spec.podReplacementPolicy: no such field", "as Kubernetes 1.20.0 defines it",
		"ConfigMap/default/settings", "metadata.labels.tier: must be a string, got 1"} {
		if !strings.Contains(old, want) {
			t.Errorf("1.20 lacks %q:\n%s", want, old)
		}
	}
	if strings.Contains(old, "Job/default/legacy") {
		t.Errorf("1.20 still serves batch/v1beta1:\n%s", old)
	}

	current := check("v1.30.0")
	if strings.Contains(current, "Job/default/report") {
		t.Errorf("1.30 has podReplacementPolicy:\n%s", current)
	}
	for _, want := range []string{"Job/default/legacy", "Kubernetes 1.30.0 does not serve batch/v1beta1"} {
		if !strings.Contains(current, want) {
			t.Errorf("1.30 lacks %q:\n%s", want, current)
		}
	}
}

// Without the schemas nothing is skipped silently: the bundled types are
// used, and the run says so.
func TestBuiltinsFallBackToBundledTypes(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "jobs", "jobs", ""))
	write(t, repo, "jobs/all.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: default}\ndatum: {mode: fast}\n")
	res, err := source.New(source.Options{CacheDir: t.TempDir(), Mode: source.Offline})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Sources.KubernetesSchemas = "https://schemas.example.test"
	tree, err := render.Tree(context.Background(), repo, "clusters/prod", cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	r := lint.Run(tree, cfg)
	if got := messages(find(r, "FL-V003")); !strings.Contains(got, "datum") {
		t.Errorf("the bundled types must still catch the unknown field:\n%s", got)
	}
	if got := messages(find(r, "FL-X003")); !strings.Contains(got, "bundled with fluxlint") || !strings.Contains(got, "running offline") {
		t.Errorf("the run must say which definition it used:\n%s", got)
	}
}

// A release's documents never change, so each is fetched once, and a group
// version the release does not serve is remembered too.
func TestKubeSchemasAreFetchedOnce(t *testing.T) {
	schemas := t.TempDir()
	release(t, schemas, "v1.30.0", `"backoffLimit": {"type": "integer"}`, false)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.FileServer(http.Dir(schemas)).ServeHTTP(w, req)
	}))
	defer srv.Close()
	cache := t.TempDir()
	fetch := func(mode source.Mode, release, group, version string) error {
		res, err := source.New(source.Options{CacheDir: cache, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		_, err = res.KubeSchema(context.Background(), srv.URL, release, group, version)
		return err
	}
	if err := fetch(source.FetchMissing, "v1.30.0", "batch", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := fetch(source.FetchMissing, "v1.30.0", "batch", "v1beta1"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want not-found for a group version the release does not serve, got %v", err)
	}
	before := hits.Load()
	if err := fetch(source.Offline, "v1.30.0", "batch", "v1"); err != nil {
		t.Errorf("cached document must work offline: %v", err)
	}
	if err := fetch(source.FetchMissing, "v1.30.0", "batch", "v1beta1"); err == nil {
		t.Error("the absence must be remembered")
	}
	if hits.Load() != before {
		t.Errorf("%d more requests for documents that cannot change", hits.Load()-before)
	}
	if err := fetch(source.FetchMissing, "v9.99.0", "batch", "v1"); err == nil || !strings.Contains(err.Error(), "is kubeVersion a released version") {
		t.Errorf("a release that does not exist is not the same as an API that is not served: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "openapi")); err != nil {
		t.Error(err)
	}
}

// What a release serves is read from the discovery documents it publishes.
func TestKubeAPIVersions(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "v1.30.0", "api", "discovery")
	write(t, dir, "api__v1.json", `{"kind": "APIResourceList", "groupVersion": "v1", "resources": [
  {"name": "pods", "kind": "Pod"}, {"name": "pods/status", "kind": "Pod"}, {"name": "configmaps", "kind": "ConfigMap"}]}`)
	// an older release only has the beta form of the aggregated document
	write(t, dir, "aggregated_v2beta1.json", `{"items": [
  {"metadata": {"name": "policy"}, "versions": [{"version": "v1", "resources": [{"resource": "poddisruptionbudgets", "responseKind": {"kind": "PodDisruptionBudget"}}]}]},
  {"metadata": {"name": "resource.k8s.io"}, "versions": [{"version": "v1alpha3", "resources": [{"resource": "resourceclaims", "responseKind": {"kind": "ResourceClaim"}}]}]}]}`)
	res, err := source.New(source.Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := res.KubeAPIVersions(context.Background(), base, "v1.30.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "policy/v1 policy/v1/PodDisruptionBudget v1 v1/ConfigMap v1/Pod"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %s (alpha versions are not served unless a cluster is told to)", got, want)
	}
	if _, err := res.KubeAPIVersions(context.Background(), base, "v1.10.0"); err == nil {
		t.Error("a release without discovery documents must be an error, so that the run can say so")
	}
}
