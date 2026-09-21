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
		// safe once; a Kustomization reconciles again, so it is still said, quietly
		{"applied before the webhook exists", "  dependsOn:\n    - name: tenants\n", "", "", "first apply is safe"},
		{"a label that only exists at runtime", "", "", "    namespaceSelector: {matchLabels: {enrolled: \"true\"}}", "needs a label that is not set in Git"},
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
			case strings.Contains(tc.want, "safe") && find(r, "FL-R008")[0].Severity != lint.Info, strings.Contains(tc.want, "not set in Git") && find(r, "FL-R008")[0].Severity != lint.Info:
				t.Errorf("a window that may never open is a suggestion, got %s", find(r, "FL-R008")[0].Severity)
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

const pullSecrets = `apiVersion: v1
kind: Namespace
metadata: {name: builders, labels: {pull: "true"}}
---
apiVersion: v1
kind: Namespace
metadata: {name: runners, labels: {pull: "true"}}
---
apiVersion: external-secrets.io/v1
kind: ClusterExternalSecret
metadata: {name: registry-login}
spec:
  externalSecretName: registry-login
  namespaces: [builders]
  externalSecretSpec:
    secretStoreRef: {name: vault, kind: ClusterSecretStore}
    dataFrom: [{extract: {key: registry}}]
---
apiVersion: external-secrets.io/v1
kind: ClusterExternalSecret
metadata: {name: registry-login-labelled}
spec:
  externalSecretName: registry-login-labelled
  namespaceSelectors: [{matchLabels: {pull: "true"}}]
  externalSecretSpec:
    secretStoreRef: {name: vault, kind: ClusterSecretStore}
    target: {name: registry-login%s}
    dataFrom: [{extract: {key: registry}}]
`

// Seen in a real cluster as "already owned by another ExternalSecret".
func TestContestedSecret(t *testing.T) {
	repo := func(policy string) string {
		dir := t.TempDir()
		write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "secrets", "secrets", ""))
		write(t, dir, "secrets/all.yaml", fmt.Sprintf(pullSecrets, policy))
		return dir
	}
	cfg := func() *config.Config {
		c := config.Default()
		c.Externals.CRDGroups = append(c.Externals.CRDGroups, "external-secrets.io")
		return c
	}
	got := find(analyseDir(t, repo(""), cfg()), "FL-R011")
	if len(got) != 1 || !strings.Contains(messages(got), "builders/registry-login") || strings.Contains(messages(got), "runners/") {
		t.Errorf("want one finding, for the one namespace both select:\n%s", messages(got))
	}
	if got := find(analyseDir(t, repo(", creationPolicy: Merge"), cfg()), "FL-R011"); len(got) != 0 {
		t.Errorf("Merge does not take ownership:\n%s", messages(got))
	}
}

func TestMissingIssuer(t *testing.T) {
	const certs = `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: internal-ca}
spec: {selfSigned: {}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: good, namespace: default}
spec: {secretName: good-tls, dnsNames: [good.example.test], issuerRef: {name: internal-ca, kind: ClusterIssuer}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: serving, namespace: default}
spec: {secretName: serving-tls, dnsNames: [serving.example.test], issuerRef: {name: selfsigned}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: from-elsewhere, namespace: default}
spec: {secretName: other-tls, dnsNames: [x.example.test], issuerRef: {name: pca, kind: AWSPCAIssuer, group: awspca.cert-manager.io}}
`
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "certs", "certs", ""))
	write(t, dir, "certs/all.yaml", certs)
	cfg := config.Default()
	cfg.Externals.CRDGroups = append(cfg.Externals.CRDGroups, "cert-manager.io")
	got := find(analyseDir(t, dir, cfg), "FL-R012")
	if len(got) != 1 || !strings.Contains(messages(got), "Certificate/default/serving") ||
		!strings.Contains(messages(got), "Issuer default/selfsigned") || !strings.Contains(messages(got), "default/serving-tls") {
		t.Errorf("want only the Certificate whose namespaced Issuer is missing:\n%s", messages(got))
	}
}

// Kyverno's webhooks are in no manifest: it registers them once it runs. What
// it will register follows from its chart and from the policies.
const kyvernoInstall = `apiVersion: v1
kind: Namespace
metadata: {name: kyverno}
---
apiVersion: v1
kind: Service
metadata:
  name: kyverno-svc
  namespace: kyverno
  labels: {app.kubernetes.io/component: admission-controller, app.kubernetes.io/part-of: kyverno}
spec:
  selector: {app.kubernetes.io/component: admission-controller}
  ports: [{port: 443}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: kyverno, namespace: kyverno}
data:
  webhooks: '{"namespaceSelector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"NotIn","values":["exempt"]}]}}'
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: kyverno-admission-controller, namespace: kyverno}
spec:
  selector: {matchLabels: {app.kubernetes.io/component: admission-controller}}
  template:
    metadata: {labels: {app.kubernetes.io/component: admission-controller}}
    spec:
      containers: [{name: kyverno, image: registry.example.test/kyverno:v1}]
`

const kyvernoLabelPolicy = `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata: {name: copy-labels}
spec:%s
  rules:
    - name: copy
      match: {any: [{resources: {kinds: ["v1/ConfigMap", "example.test/*/*"]}}]}
      mutate: {patchStrategicMerge: {metadata: {labels: {+(copied): "true"}}}}
`

func TestKyvernoAdmissionWindow(t *testing.T) {
	repo := func(policySpec string) string {
		dir := t.TempDir()
		write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "kyverno", "kyverno", "")+"---\n"+
			fmt.Sprintf(ksHeader, "policies", "policies", "  dependsOn:\n    - name: kyverno\n")+"---\n"+
			fmt.Sprintf(ksHeader, "app", "app", ""))
		write(t, dir, "kyverno/all.yaml", kyvernoInstall)
		write(t, dir, "policies/all.yaml", fmt.Sprintf(kyvernoLabelPolicy, policySpec))
		write(t, dir, "app/all.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: settings, namespace: default}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: in-kube-system, namespace: kube-system}
---
apiVersion: v1
kind: Namespace
metadata: {name: exempt}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: left-alone, namespace: exempt}
---
apiVersion: v1
kind: Secret
metadata: {name: not-matched, namespace: default}
`)
		return dir
	}
	cfg := func() *config.Config {
		c := config.Default()
		c.Externals.CRDGroups = append(c.Externals.CRDGroups, "kyverno.io")
		c.Rules.Off = map[string]bool{"FL-V007": true} // the CLI is not needed here
		return c
	}
	got := find(analyseDir(t, repo(""), cfg()), "FL-R008")
	text := messages(got)
	for _, want := range []string{
		"ConfigMap/default/settings", "mutate.kyverno.svc-fail", "ClusterPolicy/copy-labels", // resources its policies match
		"ClusterPolicy/copy-labels is admitted by fail-closed webhook validate-policy.kyverno.svc", "does not wait for health", // and its own policy kinds
	} {
		if !strings.Contains(text, want) {
			t.Errorf("FL-R008 lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"in-kube-system", "left-alone", "not-matched"} {
		if strings.Contains(text, not) {
			t.Errorf("FL-R008 must not mention %q:\n%s", not, text)
		}
	}

	single := messages(find(analyseDir(t, repo(""), cfg()), "FL-R013"))
	if !strings.Contains(single, "Deployment/kyverno/kyverno-admission-controller") || !strings.Contains(single, "flux-system/app") {
		t.Errorf("one Kyverno pod stands in front of other components:\n%s", single)
	}

	// a policy that fails open registers a webhook that rejects nothing
	text = messages(find(analyseDir(t, repo("\n  failurePolicy: Ignore"), cfg()), "FL-R008"))
	if strings.Contains(text, "mutate.kyverno.svc-fail") || !strings.Contains(text, "validate-policy.kyverno.svc") {
		t.Errorf("failurePolicy: Ignore:\n%s", text)
	}
}

func TestSinglePodGate(t *testing.T) {
	one := windowRepo(t, "", "", "")
	if got := find(analyseDir(t, one, nil), "FL-R013"); len(got) != 1 || !strings.Contains(messages(got), "Deployment/guard-system/guard") || !strings.Contains(messages(got), "flux-system/tenants") {
		t.Errorf("one pod behind a webhook that admits another component's applies:\n%s", messages(got))
	}
	two := windowRepo(t, "", "", "")
	write(t, two, "guard/all.yaml", strings.Replace(fmt.Sprintf(guard, ""), "spec:\n  selector: {matchLabels: {app: guard}}", "spec:\n  replicas: 2\n  selector: {matchLabels: {app: guard}}", 1))
	if got := find(analyseDir(t, two, nil), "FL-R013"); len(got) != 0 {
		t.Errorf("two replicas:\n%s", messages(got))
	}
	// a webhook that only guards its own component's objects is its own business
	alone := windowRepo(t, "", "", "    namespaceSelector: {matchLabels: {tier: system}}")
	if got := find(analyseDir(t, alone, nil), "FL-R013"); len(got) != 0 {
		t.Errorf("nothing else goes through it:\n%s", messages(got))
	}
}

func TestSourceCredentials(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "sources", "sources", ""))
	write(t, dir, "sources/all.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: private, namespace: flux-system}
spec: {interval: 10m, url: "https://git.example.test/private.git", ref: {tag: v1}, secretRef: {name: git-token}}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: also-private, namespace: flux-system}
spec: {interval: 10m, url: "https://git.example.test/other.git", ref: {tag: v1}, secretRef: {name: fetched-token}}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: fetched-token, namespace: flux-system}
spec:
  secretStoreRef: {name: vault, kind: ClusterSecretStore}
  dataFrom: [{extract: {key: git}}]
`)
	cfg := config.Default()
	cfg.Externals.CRDGroups = append(cfg.Externals.CRDGroups, "external-secrets.io")
	got := find(analyseDir(t, dir, cfg), "FL-R014")
	if len(got) != 1 || !strings.Contains(messages(got), "GitRepository/flux-system/private") || !strings.Contains(messages(got), "flux-system/git-token") {
		t.Errorf("want only the source whose Secret has no producer:\n%s", messages(got))
	}
	cfg.Externals.Secrets = append(cfg.Externals.Secrets, config.ExternalSecretRef{Namespace: "flux-system", Name: "git-token"})
	if got := find(analyseDir(t, dir, cfg), "FL-R014"); len(got) != 0 {
		t.Errorf("declared under externals.secrets:\n%s", messages(got))
	}
}

// Seen in a real cluster as "could not resolve Secret chart values reference
// … with key …: key not found".
func TestValuesReference(t *testing.T) {
	const release = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: runner, namespace: default}
spec:
  interval: 10m
  chart:
    spec: {chart: runner, version: 1.0.0, sourceRef: {kind: HelmRepository, name: charts, namespace: flux-system}}
  valuesFrom:
    - {kind: Secret, name: runner-credentials, valuesKey: serverUrl, targetPath: serverUrl}
    - {kind: Secret, name: runner-credentials, valuesKey: token, targetPath: token}
    - {kind: ConfigMap, name: runner-defaults}
    - {kind: ConfigMap, name: not-there}
    - {kind: ConfigMap, name: may-be-absent, optional: true}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: runner-credentials, namespace: default}
spec:
  secretStoreRef: {name: vault, kind: ClusterSecretStore}
  data:
    - {secretKey: token, remoteRef: {key: runner, property: token}}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: runner-defaults, namespace: default}
data: {values.yaml: "replicas: 1"}
`
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "runner", "runner", ""))
	write(t, dir, "runner/all.yaml", release)
	cfg := config.Default()
	cfg.Externals.CRDGroups = append(cfg.Externals.CRDGroups, "external-secrets.io")
	got := find(analyseDir(t, dir, cfg), "FL-R015")
	text := messages(got)
	if len(got) != 2 || !strings.Contains(text, `needs key "serverUrl" of Secret default/runner-credentials`) || !strings.Contains(text, "ConfigMap default/not-there, which nothing creates") {
		t.Errorf("want the missing key and the missing ConfigMap, and nothing else:\n%s", text)
	}
}
