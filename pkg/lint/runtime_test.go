package lint_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/render"
)

const ksHeader = `apiVersion: kustomize.toolkit.fluxcd.io/v1
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
%s`

// deployment renders a Deployment whose pod spec is given verbatim.
func deployment(name, namespace string, replicas int, podSpec string) string {
	return fmt.Sprintf(`---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: %[3]d
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
%[4]s`, name, namespace, replicas, indent(podSpec, 6))
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, pad+l)
	}
	return strings.Join(out, "\n") + "\n"
}

func analyseDir(t *testing.T, dir string, cfg *config.Config) *lint.Result {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	tree, err := render.Tree(context.Background(), dir, "clusters/prod", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return lint.Run(tree, cfg)
}

func messages(fs []lint.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Object+": "+f.Message+" "+strings.Join(f.Detail, " "))
	}
	return strings.Join(out, "\n")
}

const producers = `---
apiVersion: v1
kind: Namespace
metadata:
  name: apps
  labels: {pull-secrets: "true"}
---
apiVersion: v1
kind: Namespace
metadata:
  name: unlabelled
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: credentials
  namespace: apps
spec:
  target: {name: api-credentials}
  data:
    - {secretKey: SERVICE_TOKEN, remoteRef: {key: x}}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: everything
  namespace: apps
spec:
  dataFrom:
    - extract: {key: y}
---
apiVersion: external-secrets.io/v1
kind: ClusterExternalSecret
metadata:
  name: regcred
spec:
  namespaceSelectors:
    - matchLabels: {pull-secrets: "true"}
  externalSecretSpec:
    target: {name: regcred}
    data:
      - {secretKey: .dockerconfigjson, remoteRef: {key: z}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: serving
  namespace: apps
spec:
  secretName: serving-cert
  issuerRef: {name: ca}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: manager
  namespace: apps
`

func runtimeRepo(t *testing.T, workloads string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "apps", "apps", ""))
	write(t, dir, "apps/producers.yaml", producers)
	write(t, dir, "apps/workloads.yaml", workloads)
	return dir
}

var externalGroups = func() *config.Config {
	c := config.Default()
	c.Externals.CRDGroups = []string{"external-secrets.io", "cert-manager.io"}
	return c
}

func TestRuntimeReferencesThatResolve(t *testing.T) {
	r := analyseDir(t, runtimeRepo(t, deployment("ok", "apps", 1, `serviceAccountName: manager
imagePullSecrets: [{name: regcred}]
containers:
  - name: manager
    image: example.test/manager:1
    env:
      - name: SERVICE_TOKEN
        valueFrom: {secretKeyRef: {name: api-credentials, key: SERVICE_TOKEN}}
      - name: ANYTHING
        valueFrom: {secretKeyRef: {name: everything, key: whatever-the-store-has}}
      - name: MAYBE
        valueFrom: {secretKeyRef: {name: not-there, key: k, optional: true}}
volumes:
  - name: tls
    secret:
      secretName: serving-cert
      items: [{key: tls.crt, path: tls.crt}]
  - name: ca
    configMap: {name: kube-root-ca.crt}
`)), externalGroups())
	if got := problems(r); len(got) != 0 {
		t.Fatalf("every reference has a producer: %v\n%s", got, messages(r.Findings))
	}
}

func TestRuntimeReferencesThatDoNot(t *testing.T) {
	r := analyseDir(t, runtimeRepo(t, deployment("broken", "apps", 1, `serviceAccountName: does-not-exist
containers:
  - name: manager
    image: example.test/manager:1
    env:
      - name: API_SECRET
        valueFrom: {secretKeyRef: {name: api-credentials, key: API_SECRET}}
      - name: TOKEN
        valueFrom: {secretKeyRef: {name: never-created, key: token}}
volumes:
  - name: tls
    secret:
      secretName: serving-cert
      items: [{key: keystore.p12, path: keystore.p12}]
`)+deployment("elsewhere", "unlabelled", 1, `imagePullSecrets: [{name: regcred}]
containers:
  - name: c
    image: example.test/c:1
`)), externalGroups())

	config := messages(find(r, "FL-R001"))
	for _, want := range []string{
		`needs key "API_SECRET" of Secret apps/api-credentials, but ExternalSecret/apps/credentials only defines "SERVICE_TOKEN"`,
		`references Secret apps/never-created, which nothing creates`,
		`needs key "keystore.p12" of Secret apps/serving-cert`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("FL-R001 lacks %q in:\n%s", want, config)
		}
	}
	identity := messages(find(r, "FL-R002"))
	for _, want := range []string{
		"references ServiceAccount apps/does-not-exist",
		// the ClusterExternalSecret selects only namespaces labelled pull-secrets=true
		"imagePullSecrets references Secret unlabelled/regcred",
	} {
		if !strings.Contains(identity, want) {
			t.Errorf("FL-R002 lacks %q in:\n%s", want, identity)
		}
	}
	if n := len(find(r, "FL-R001")) + len(find(r, "FL-R002")); n != 5 {
		t.Errorf("want exactly 5 findings, got %d:\n%s\n%s", n, config, identity)
	}
}

func TestExternalSecretDeclaration(t *testing.T) {
	cfg := externalGroups()
	cfg.Externals.Secrets = []config.ExternalSecretRef{{Namespace: "apps", Name: "bootstrap", Keys: []string{"token"}}}
	r := analyseDir(t, runtimeRepo(t, deployment("d", "apps", 1, `containers:
  - name: c
    image: example.test/c:1
    env:
      - name: A
        valueFrom: {secretKeyRef: {name: bootstrap, key: token}}
      - name: B
        valueFrom: {secretKeyRef: {name: bootstrap, key: password}}
`)), cfg)
	got := find(r, "FL-R001")
	if len(got) != 1 || !strings.Contains(got[0].Message, `"password"`) {
		t.Fatalf("declared keys are enforced: token resolves, password does not:\n%s", messages(got))
	}
}

// A pod that waits for a Secret made by a Kustomization which itself waits for
// the pod's Kustomization can never start.
func TestDeadlockThroughSecret(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml",
		fmt.Sprintf(ksHeader, "workloads", "workloads", "  wait: true\n  timeout: 5m\n")+"---\n"+
			fmt.Sprintf(ksHeader, "secrets", "secrets", "  dependsOn:\n    - name: workloads\n"))
	write(t, dir, "workloads/d.yaml", deployment("d", "default", 1, `containers:
  - name: c
    image: example.test/c:1
    envFrom:
      - secretRef: {name: settings}
`))
	write(t, dir, "secrets/s.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: settings\n  namespace: default\nstringData:\n  a: b\n")
	r := analyseDir(t, dir, nil)
	got := find(r, "FL-G002")
	if len(got) != 1 || !strings.Contains(strings.Join(got[0].Detail, "\n"), "Secret default/settings") {
		t.Fatalf("want a deadlock explained by the Secret:\n%s", messages(r.Findings))
	}
}

func TestPositionalPatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "operator", "operator", `  patches:
    - target: {kind: Deployment, name: manager}
      patch: |
        - op: replace
          path: /spec/template/spec/containers/0/env/1/value
          value: patched
        - op: add
          path: /spec/template/spec/containers/0/args/-
          value: --safe-append
`))
	write(t, dir, "operator/d.yaml", deployment("manager", "default", 1, `containers:
  - name: manager
    image: example.test/m:1
    args: [--first]
    env:
      - {name: FIRST, value: a}
      - {name: SECOND, value: b}
`))
	r := analyseDir(t, dir, nil)
	got := find(r, "FL-R003")
	if len(got) != 1 || len(got[0].Detail) != 1 {
		t.Fatalf("one positional path (the append is safe):\n%s", messages(got))
	}
	if d := got[0].Detail[0]; !strings.Contains(d, "env/1/value") || !strings.Contains(d, `currently "SECOND"`) {
		t.Errorf("detail should say what the index hits today: %s", d)
	}
}

func webhookRepo(t *testing.T, replicas int, failurePolicy string) string {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "policy", "policy", ""))
	write(t, dir, "policy/all.yaml", deployment("gate", "default", replicas, "containers:\n  - name: c\n    image: example.test/gate:1\n")+fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: gate
  namespace: default
spec:
  selector: {app: gate}
  ports: [{port: 443}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: gate
webhooks:
  - name: gate.example.test
    failurePolicy: %s
    clientConfig:
      service: {name: gate, namespace: default}
    rules: []
    sideEffects: None
    admissionReviewVersions: [v1]
`, failurePolicy))
	return dir
}

func TestWebhookScaledToZero(t *testing.T) {
	r := analyseDir(t, webhookRepo(t, 0, "Fail"), nil)
	if got := find(r, "FL-R004"); len(got) != 1 || !strings.Contains(got[0].Message, "scaled to 0") {
		t.Fatalf("fail-closed webhook with no endpoints:\n%s", messages(r.Findings))
	}
	for name, dir := range map[string]string{
		"running":   webhookRepo(t, 2, "Fail"),
		"fail-open": webhookRepo(t, 0, "Ignore"),
	} {
		if got := find(analyseDir(t, dir, nil), "FL-R004"); len(got) != 0 {
			t.Errorf("%s: not a problem, got:\n%s", name, messages(got))
		}
	}
}

func TestStaleSubstitution(t *testing.T) {
	vars := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: versions\n  namespace: flux-system\ndata:\n  tag: v1\n"
	reader := func(extra string) string {
		return fmt.Sprintf(ksHeader, "apps", "apps", "  postBuild:\n    substituteFrom:\n      - kind: ConfigMap\n        name: versions\n"+extra)
	}
	build := func(clusterYAML string) string {
		dir := t.TempDir()
		write(t, dir, "clusters/prod/all.yaml", clusterYAML)
		write(t, dir, "config/vars.yaml", vars)
		write(t, dir, "apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: default\ndata:\n  tag: ${tag}\n")
		return dir
	}
	owner := fmt.Sprintf(ksHeader, "config", "config", "")

	r := analyseDir(t, build(owner+"---\n"+reader("")), nil)
	got := find(r, "FL-T010")
	if len(got) != 1 || !strings.Contains(strings.Join(got[0].Detail, " "), "add dependsOn: config") {
		t.Fatalf("reader and owner of the ConfigMap are unordered:\n%s", messages(r.Findings))
	}
	r = analyseDir(t, build(owner+"---\n"+reader("  dependsOn:\n    - name: config\n")), nil)
	if got := find(r, "FL-T010"); len(got) != 0 {
		t.Fatalf("dependsOn makes the controller wait for the same revision:\n%s", messages(got))
	}
}
