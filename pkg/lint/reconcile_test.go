package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/model"
)

// guard is a fail-closed webhook on namespaces, with its Service and pods.
const guard = `apiVersion: v1
kind: Namespace
metadata:
  name: guard-system
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: guard
webhooks:
  - name: namespaces.guard.example.test
    failurePolicy: Fail
    sideEffects: None
    admissionReviewVersions: [v1]
    clientConfig:
      service: {name: guard, namespace: guard-system, path: /validate}
    rules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [namespaces]
%s
---
apiVersion: v1
kind: Service
metadata:
  name: guard
  namespace: guard-system
spec:
  selector: {app: guard}
  ports: [{port: 443}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: guard
  namespace: guard-system
spec:
  selector: {matchLabels: {app: guard}}
  template:
    metadata: {labels: {app: guard}}
    spec:
      containers: [{name: guard, image: registry.example.test/guard:v1}]
`

const teamNamespace = `apiVersion: v1
kind: Namespace
metadata:
  name: team-a
  labels: {tier: tenant}
`

func windowRepo(t *testing.T, guardSpec, tenantSpec, hookExtra string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml",
		fmt.Sprintf(ksHeader, "guard", "guard", guardSpec)+"---\n"+fmt.Sprintf(ksHeader, "tenants", "tenants", tenantSpec))
	write(t, dir, "guard/all.yaml", fmt.Sprintf(guard, hookExtra))
	write(t, dir, "tenants/ns.yaml", teamNamespace)
	return dir
}

func TestAdmissionWindow(t *testing.T) {
	dependsOn := "  dependsOn:\n    - name: guard\n"
	for _, tc := range []struct {
		name, guard, tenants, hook string
		want                       string // substring of the finding, or "" for none
	}{
		{"unordered", "", "", "", "nothing orders the two"},
		{"ordered but the webhook's component does not wait", "", dependsOn, "", "does not wait for health"},
		{"ordered and waiting", "  wait: true\n", dependsOn, "", ""},
		{"applied before the webhook exists", "  dependsOn:\n    - name: tenants\n", "", "", ""},
		{"namespace not selected", "", "", "    namespaceSelector: {matchLabels: {tier: system}}", ""},
		{"namespace selected", "", "", "    namespaceSelector: {matchLabels: {tier: tenant}}", "nothing orders the two"},
		{"selected by the name label every namespace has", "", "", "    namespaceSelector:\n      matchExpressions:\n        - {key: kubernetes.io/metadata.name, operator: In, values: [team-a]}", "nothing orders the two"},
		{"conditions are not guessed at", "", "", "    matchConditions: [{name: x, expression: \"true\"}]", ""},
		{"an empty object selector selects everything", "", "", "    objectSelector: {}\n", "nothing orders the two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := analyseDir(t, windowRepo(t, tc.guard, tc.tenants, tc.hook), nil)
			got := messages(find(r, "FL-R008"))
			switch {
			case tc.want == "" && got != "":
				t.Errorf("unexpected finding:\n%s", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("want %q, got:\n%s", tc.want, got)
			case tc.want != "" && !strings.Contains(got, "Namespace/team-a"):
				t.Errorf("finding does not name the object:\n%s", got)
			}
		})
	}

	t.Run("a webhook that fails open rejects nothing", func(t *testing.T) {
		dir := windowRepo(t, "", "", "")
		write(t, dir, "guard/all.yaml", strings.Replace(fmt.Sprintf(guard, ""), "failurePolicy: Fail", "failurePolicy: Ignore", 1))
		if got := messages(find(analyseDir(t, dir, nil), "FL-R008")); got != "" {
			t.Errorf("unexpected finding:\n%s", got)
		}
	})
}

func TestContestedFields(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "app", "app", ""))
	write(t, dir, "app/all.yaml", deployment("scaled", "default", 3, "containers: [{name: a, image: registry.example.test/a:v1}]")+
		deployment("fixed", "default", 2, "containers: [{name: a, image: registry.example.test/a:v1}]")+`---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: scaled, namespace: default}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: scaled}
  minReplicas: 2
  maxReplicas: 8
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: pinned
  annotations: {cert-manager.io/inject-ca-from: default/serving}
webhooks:
  - name: a.example.test
    failurePolicy: Ignore
    sideEffects: None
    admissionReviewVersions: [v1]
    clientConfig: {url: "https://a.example.test", caBundle: Q0E=}
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: injected
  annotations: {cert-manager.io/inject-ca-from: default/serving}
webhooks:
  - name: b.example.test
    failurePolicy: Ignore
    sideEffects: None
    admissionReviewVersions: [v1]
    clientConfig: {url: "https://b.example.test"}
`)
	got := messages(find(analyseDir(t, dir, nil), "FL-R009"))
	for _, want := range []string{"Deployment/default/scaled", "HorizontalPodAutoscaler", "MutatingWebhookConfiguration/pinned", "caBundle"} {
		if !strings.Contains(got, want) {
			t.Errorf("FL-R009 lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"Deployment/default/fixed", "MutatingWebhookConfiguration/injected"} {
		if strings.Contains(got, not) {
			t.Errorf("FL-R009 must not report %q:\n%s", not, got)
		}
	}
}

// The renderer finds the objects (see pkg/render); the rule reports them.
func TestUnstableRender(t *testing.T) {
	secret := model.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "db-password", "namespace": "default"}}
	deploy := model.Object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "db", "namespace": "default"}}
	c := &model.Component{Kind: model.KindHelmRelease, Namespace: "default", Name: "db", IsRoot: true,
		Objects:  []model.Object{secret, deploy},
		Unstable: []string{secret.ID(), deploy.ID(), "ConfigMap/default/renamed-by-a-post-renderer"}}
	tree := &model.Tree{Entrypoint: "clusters/prod", Root: c, Components: []*model.Component{c}, ByKey: map[string]*model.Component{c.Key(): c}}
	cfg := config.Default()
	got := find(lint.Run(tree, cfg), "FL-R010")
	if len(got) != 2 {
		t.Fatalf("want two findings, got:\n%s", messages(got))
	}
	for _, f := range got {
		want := lint.Info
		if strings.Contains(f.Object, "Secret") {
			want = lint.Warning
		}
		if f.Severity != want {
			t.Errorf("%s: severity %s, want %s", f.Object, f.Severity, want)
		}
	}
}
