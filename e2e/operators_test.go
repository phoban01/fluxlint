//go:build e2e

package e2e

// An internal platform, entirely mocked: nothing here needs a network.
//
//   - an API repository (Go module + CRDs) and an operator repository
//     (kubebuilder layout, Go module, RBAC, contract), served as Git over file://
//   - external-secrets CRDs as an OCI artifact in an in-process registry
//   - a private Helm repository behind basic auth, laid out like a GitLab
//     package registry
//   - a cluster repository that adapts the operator the way platform teams do:
//     blue/green nested Kustomization, version / path / replicas from a shared
//     variables ConfigMap, the inline placeholder Secret deleted and env entries
//     rewired by index onto a ClusterExternalSecret-provided Secret
//
// This is the shape that cannot be exercised against public upstreams, so the
// rules about operators are proven here.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	helmrepo "helm.sh/helm/v3/pkg/repo"

	"archive/tar"
	"bytes"
	"compress/gzip"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func crd(group, kind, plural string) string {
	return fmt.Sprintf(`---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %[3]s.%[1]s
spec:
  group: %[1]s
  names: {kind: %[2]s, plural: %[3]s}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
`, group, kind, plural)
}

// apiUpstream: Go module example.test/platform-api with the Widget CRD.
func apiUpstream(t *testing.T) string {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "go.mod", "module example.test/platform-api\n\ngo 1.24\n")
	write(t, dir, "config/crd/widgets.yaml", crd("platform.example.test", "Widget", "widgets"))
	for _, tag := range []string{"v1.0.0", "v1.1.0"} {
		write(t, dir, "VERSION", tag)
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "--quiet", "-m", tag)
		gitIn(t, dir, "tag", tag)
	}
	return dir
}

const operatorEnvV100 = `            - name: WATCH_NAMESPACE
              value: ""
            - name: SERVICE_URL
              valueFrom: {secretKeyRef: {name: manager-api-credentials, key: SERVICE_URL}}
            - name: SERVICE_TOKEN
              valueFrom: {secretKeyRef: {name: manager-api-credentials, key: SERVICE_TOKEN}}
`

// v1.1.0 inserts an entry at index 1. Nothing about the patch in the cluster
// repository changes — which is the problem.
const operatorEnvV110 = `            - name: WATCH_NAMESPACE
              value: ""
            - name: TELEMETRY_TOKEN
              valueFrom: {secretKeyRef: {name: manager-telemetry, key: token}}
            - name: SERVICE_URL
              valueFrom: {secretKeyRef: {name: manager-api-credentials, key: SERVICE_URL}}
            - name: SERVICE_TOKEN
              valueFrom: {secretKeyRef: {name: manager-api-credentials, key: SERVICE_TOKEN}}
`

// operatorUpstream: kubebuilder layout. v1.0.0 is built against
// platform-api v1.0.0; v1.1.0 against v1.1.0 and with one more env entry.
func operatorUpstream(t *testing.T) string {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "fluxlint-contract.yaml", `requires:
  crds:
    - {group: platform.example.test, kind: Widget, version: v1}
  secrets:
    - {name: api-credentials, keys: [SERVICE_URL, SERVICE_TOKEN]}
`)
	write(t, dir, "config/default/kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: operator-system\nnamePrefix: operator-\nresources:\n  - ../manager\n  - ../rbac\n")
	write(t, dir, "config/manager/kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manager.yaml\n  - secrets.yaml\n")
	write(t, dir, "config/rbac/kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - rbac.yaml\n")
	write(t, dir, "config/rbac/rbac.yaml", `apiVersion: v1
kind: ServiceAccount
metadata: {name: manager, namespace: system}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: manager-role}
rules:
  - apiGroups: [platform.example.test]
    resources: [widgets, widgets/status]
    verbs: [get, list, watch, update, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: manager-rolebinding}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: manager-role}
subjects:
  - {kind: ServiceAccount, name: manager, namespace: system}
`)
	for _, v := range []struct{ tag, api, env, secrets string }{
		{"v1.0.0", "v1.0.0", operatorEnvV100, ""},
		{"v1.1.0", "v1.1.0", operatorEnvV110, "---\napiVersion: v1\nkind: Secret\nmetadata: {name: manager-telemetry, namespace: system}\nstringData: {token: placeholder}\n"},
	} {
		write(t, dir, "go.mod", "module example.test/operator\n\ngo 1.24\n\nrequire example.test/platform-api "+v.api+"\n")
		write(t, dir, "config/manager/manager.yaml", `apiVersion: apps/v1
kind: Deployment
metadata: {name: manager, namespace: system}
spec:
  replicas: 1
  selector: {matchLabels: {control-plane: manager}}
  template:
    metadata: {labels: {control-plane: manager}}
    spec:
      serviceAccountName: manager
      containers:
        - name: manager
          image: controller:latest
          env:
`+v.env)
		write(t, dir, "config/manager/secrets.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: manager-api-credentials, namespace: system}\nstringData: {SERVICE_URL: placeholder, SERVICE_TOKEN: placeholder}\n"+v.secrets)
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "--quiet", "-m", v.tag)
		gitIn(t, dir, "tag", v.tag)
	}
	return dir
}

// ociRegistry serves the external-secrets CRDs as a Flux OCI artifact.
func ociRegistry(t *testing.T) string {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := crd("external-secrets.io", "ExternalSecret", "externalsecrets") +
		strings.Replace(crd("external-secrets.io", "ClusterExternalSecret", "clusterexternalsecrets"), "scope: Namespaced", "scope: Cluster", 1)
	tw.WriteHeader(&tar.Header{Name: "crds/eso.yaml", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()

	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(buf.Bytes(), types.MediaType("application/vnd.cncf.flux.content.v1.tar+gzip")))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference(host + "/platform/eso-crds:1.0.0")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return host
}

// privateCharts is a Helm repository behind basic auth, at a path shaped like
// GitLab's package registry.
func privateCharts(t *testing.T) (url, netrc string) {
	dir := t.TempDir()
	src := t.TempDir()
	write(t, src, "metrics-agent/Chart.yaml", "apiVersion: v2\nname: metrics-agent\nversion: 2.3.1\n")
	write(t, src, "metrics-agent/values.yaml", "cluster: unset\n")
	write(t, src, "metrics-agent/templates/deploy.yaml", `apiVersion: apps/v1
kind: Deployment
metadata: {name: {{ .Release.Name }}}
spec:
  replicas: 1
  selector: {matchLabels: {app: telemetry}}
  template:
    metadata: {labels: {app: telemetry}}
    spec:
      containers:
        - name: agent
          image: example.test/telemetry:1
          args: ["--cluster={{ required "cluster is required" .Values.cluster }}"]
`)
	ch, err := loader.Load(filepath.Join(src, "metrics-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chartutil.Save(ch, dir); err != nil {
		t.Fatal(err)
	}
	const prefix = "/api/v4/projects/42/packages/helm/stable"
	files := http.StripPrefix(prefix, http.FileServer(http.Dir(dir)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "gitlab-ci-token" || p != "job-token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitLab Packages Registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	index, err := helmrepo.IndexDirectory(dir, srv.URL+prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(dir, "index.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	netrc = filepath.Join(t.TempDir(), "netrc")
	os.WriteFile(netrc, []byte("machine 127.0.0.1 login gitlab-ci-token password job-token\n"), 0o600)
	return srv.URL + prefix, netrc
}

const flux = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: %s, namespace: flux-system}
spec:
  interval: 1h
  retryInterval: 1m
  prune: true
%s`

type platform struct {
	dir, netrc string
}

// internalPlatform writes the cluster repository.
func internalPlatform(t *testing.T) platform {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	api, operator, oci := apiUpstream(t), operatorUpstream(t), ociRegistry(t)
	charts, netrc := privateCharts(t)
	dir := t.TempDir()
	repoSource := "  sourceRef: {kind: GitRepository, name: flux-system}\n"

	write(t, dir, ".fluxlint.yaml", "kubeVersion: \"1.35.0\"\nentrypoints:\n  - clusters/prod\n")
	write(t, dir, "clusters/prod/flux-system/kustomization.yaml", "resources:\n  - gotk-sync.yaml\n")
	write(t, dir, "clusters/prod/flux-system/gotk-sync.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: flux-system, namespace: flux-system}
spec: {interval: 1m, ref: {branch: main}, url: https://example.test/cluster.git}
---
`+fmt.Sprintf(flux, "flux-system", repoSource+"  path: ./clusters/prod\n"))
	write(t, dir, "clusters/prod/cluster-vars.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: cluster-vars, namespace: flux-system}
data:
  cluster_name: "management"
  api_crds_version: "v1.0.0"
  ctrl_green_version: "v1.0.0"
  ctrl_green_path: "./config/default"
  ctrl_green_scale: "2"
`)
	vars := "  postBuild:\n    substituteFrom:\n      - {kind: ConfigMap, name: cluster-vars}\n"
	write(t, dir, "clusters/prod/platform.yaml",
		fmt.Sprintf(flux, "namespaces", repoSource+"  path: ./namespaces\n  wait: true\n  timeout: 2m\n")+"---\n"+
			fmt.Sprintf(flux, "eso-crds", "  sourceRef: {kind: OCIRepository, name: eso-crds}\n  path: ./crds\n  wait: true\n  timeout: 2m\n")+"---\n"+
			fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: {name: eso-crds, namespace: flux-system}
spec: {interval: 1h, insecure: true, url: "oci://%s/platform/eso-crds", ref: {tag: 1.0.0}}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: platform-api, namespace: flux-system}
spec: {interval: 1h, url: "file://%s", ref: {tag: "${api_crds_version}"}}
---
`, oci, api)+
			fmt.Sprintf(flux, "platform-api", "  sourceRef: {kind: GitRepository, name: platform-api}\n  path: ./config/crd\n  wait: true\n  timeout: 2m\n  dependsOn: [{name: flux-system}]\n")+"---\n"+
			fmt.Sprintf(flux, "secrets", repoSource+"  path: ./secrets\n  dependsOn: [{name: eso-crds}, {name: namespaces}]\n")+"---\n"+
			fmt.Sprintf(flux, "apps", repoSource+"  path: ./apps\n  wait: true\n  timeout: 10m\n  dependsOn: [{name: flux-system}, {name: platform-api}, {name: secrets}]\n"+vars))
	// the two sources above use variables too, so the root substitutes as well
	edit(t, dir, "clusters/prod/flux-system/gotk-sync.yaml", "  path: ./clusters/prod\n", "  path: ./clusters/prod\n"+vars)

	write(t, dir, "namespaces/ns.yaml", `apiVersion: v1
kind: Namespace
metadata:
  name: operator-system
  labels: {platform.example.test/inject-secrets: "true", pod-security.kubernetes.io/enforce: baseline}
---
apiVersion: v1
kind: Namespace
metadata:
  name: telemetry
  labels: {pod-security.kubernetes.io/enforce: baseline}
`)
	ces := func(name, target, keys string) string {
		return fmt.Sprintf(`---
apiVersion: external-secrets.io/v1
kind: ClusterExternalSecret
metadata: {name: %s}
spec:
  namespaceSelectors:
    - matchLabels: {platform.example.test/inject-secrets: "true"}
  externalSecretSpec:
    secretStoreRef: {kind: ClusterSecretStore, name: vault}
    target: {name: %s}
    data:
%s`, name, target, keys)
	}
	write(t, dir, "secrets/ces.yaml",
		ces("api-credentials", "api-credentials", "      - {secretKey: SERVICE_URL, remoteRef: {key: prod/bootstrap, property: endpoint}}\n      - {secretKey: SERVICE_TOKEN, remoteRef: {key: prod/bootstrap, property: key}}\n")+
			ces("regcred", "regcred", "      - {secretKey: .dockerconfigjson, remoteRef: {key: prod/regcred}}\n"))

	write(t, dir, "apps/operator-green.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: operator-green, namespace: operator-system}
spec: {interval: 30m, url: "file://%s", ref: {tag: "${ctrl_green_version}"}}
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: operator-green, namespace: operator-system}
spec:
  interval: 30m
  retryInterval: 1m
  timeout: 5m
  prune: true
  wait: true
  sourceRef: {kind: GitRepository, name: operator-green}
  path: ${ctrl_green_path}
  nameSuffix: -green
  images:
    - name: controller
      newName: registry.example.test/platform/operator
      newTag: ${ctrl_green_version}
  patches:
    - target: {group: apps, version: v1, kind: Deployment, name: manager}
      patch: |-
        - op: add
          path: /spec/template/spec/imagePullSecrets
          value: [{name: regcred}]
        - op: replace
          path: /spec/replicas
          value: ${ctrl_green_scale}
    # ClusterExternalSecrets land a bare-named Secret; drop the prefixed inline
    # placeholder and rewire the env entries onto the bare name
    - target: {version: v1, kind: Secret, name: manager-api-credentials}
      patch: |-
        apiVersion: v1
        kind: Secret
        metadata: {name: manager-api-credentials, namespace: system}
        $patch: delete
    - target: {group: apps, version: v1, kind: Deployment, name: manager}
      patch: |-
        - op: replace
          path: /spec/template/spec/containers/0/env/1/valueFrom/secretKeyRef/name
          value: api-credentials
        - op: replace
          path: /spec/template/spec/containers/0/env/2/valueFrom/secretKeyRef/name
          value: api-credentials
`, operator))
	write(t, dir, "apps/telemetry.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: {name: internal, namespace: telemetry}
spec: {interval: 1h, url: "%s"}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: metrics-agent, namespace: telemetry}
spec:
  interval: 30m
  chart:
    spec: {chart: metrics-agent, version: 2.3.1, sourceRef: {kind: HelmRepository, name: internal}}
  values:
    cluster: ${cluster_name}
`, charts))
	return platform{dir: dir, netrc: netrc}
}

func (p platform) check(t *testing.T, args ...string) result {
	t.Helper()
	t.Setenv("NETRC", p.netrc)
	return check(t, p.dir, args...)
}

// ---------------------------------------------------------------------------

// The adapted operator works, and the one standing warning is the fragile
// patch — with what each index hits at the pinned tag.
func TestOperatorBaseline(t *testing.T) {
	p := internalPlatform(t)
	r := p.check(t)
	expectOnly(t, r, "FL-R003", "operator-system/operator-green", `env/1/valueFrom/secretKeyRef/name  -> currently "SERVICE_URL"`, `env/2/valueFrom/secretKeyRef/name  -> currently "SERVICE_TOKEN"`)
	if len(r.problems()) != 1 {
		t.Errorf("exactly one standing warning expected: %+v", r.problems())
	}
	if got := r.rule("FL-X001"); len(got) != 0 {
		t.Fatalf("Git over file://, the OCI artifact and the private chart must all render: %+v", got)
	}
	// everything is offline-reproducible afterwards
	if off := p.check(t, "--offline"); len(off.problems()) != 1 {
		t.Errorf("offline run differs: %+v", off.problems())
	}
}

// The routine change: bump the operator's version variable. Upstream inserted
// an env entry, so the untouched patch now rewires the wrong variables; and the
// new operator is built against a newer API than the CRDs pinned beside it.
func TestOperatorVersionBumpShiftsPositionalPatch(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "clusters/prod/cluster-vars.yaml", `ctrl_green_version: "v1.0.0"`, `ctrl_green_version: "v1.1.0"`)
	r := p.check(t)
	if r.ExitCode != 1 {
		t.Errorf("exit = %d, want 1", r.ExitCode)
	}
	var wiring string
	for _, f := range r.rule("FL-R001") {
		wiring += f.text() + "\n"
	}
	for _, want := range []string{
		// index 1 is now TELEMETRY_TOKEN, pointed at a Secret without that key
		`env TELEMETRY_TOKEN needs key "token" of Secret operator-system/api-credentials`,
		// index 3 (SERVICE_TOKEN) was not rewired and still names the deleted placeholder
		`env SERVICE_TOKEN references Secret operator-system/manager-api-credentials, which nothing creates`,
	} {
		if !strings.Contains(wiring, want) {
			t.Errorf("FL-R001 lacks %q in:\n%s", want, wiring)
		}
	}
	var positional string
	for _, f := range r.rule("FL-R003") {
		positional += f.text()
	}
	if !strings.Contains(positional, `currently "TELEMETRY_TOKEN"`) {
		t.Errorf("the report should show what the index hits now: %s", positional)
	}
	skew := r.rule("FL-R007")
	if len(skew) != 1 || !strings.Contains(skew[0].text(), "example.test/platform-api v1.1.0") || !strings.Contains(skew[0].text(), "at v1.0.0") {
		t.Errorf("operator v1.1.0 is built against platform-api v1.1.0, CRDs are pinned at v1.0.0: %+v", r.problems())
	}
}

// The same bump done properly: CRDs first, and the patch keyed by name.
func TestOperatorVersionBumpDoneRight(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "clusters/prod/cluster-vars.yaml", `api_crds_version: "v1.0.0"`, `api_crds_version: "v1.1.0"`)
	edit(t, p.dir, "clusters/prod/cluster-vars.yaml", `ctrl_green_version: "v1.0.0"`, `ctrl_green_version: "v1.1.0"`)
	b, _ := os.ReadFile(filepath.Join(p.dir, "apps/operator-green.yaml"))
	positional := string(b)[strings.LastIndex(string(b), "    - target: {group: apps, version: v1, kind: Deployment, name: manager}"):]
	edit(t, p.dir, "apps/operator-green.yaml", positional, `    - target: {group: apps, version: v1, kind: Deployment, name: manager}
      patch: |-
        apiVersion: apps/v1
        kind: Deployment
        metadata: {name: manager, namespace: system}
        spec:
          template:
            spec:
              containers:
                - name: manager
                  env:
                    - name: SERVICE_URL
                      valueFrom: {secretKeyRef: {name: api-credentials, key: SERVICE_URL}}
                    - name: SERVICE_TOKEN
                      valueFrom: {secretKeyRef: {name: api-credentials, key: SERVICE_TOKEN}}
`)
	if got := p.check(t).problems(); len(got) != 0 {
		t.Fatalf("name-keyed patch survives the upstream change: %+v", got)
	}
}

// The ClusterExternalSecrets select namespaces by label.
func TestOperatorNamespaceLosesItsSecretLabel(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "namespaces/ns.yaml", `platform.example.test/inject-secrets: "true", `, "")
	r := p.check(t)
	var text string
	for _, f := range r.problems() {
		text += f.Rule + " " + f.text() + "\n"
	}
	for _, want := range []string{
		"FL-R001", "references Secret operator-system/api-credentials, which nothing creates",
		"FL-R002", "imagePullSecrets references Secret operator-system/regcred",
		"FL-C001", "its contract requires Secret operator-system/api-credentials",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestOperatorContractOrdering(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "clusters/prod/platform.yaml", "{name: flux-system}, {name: platform-api}, {name: secrets}", "{name: flux-system}, {name: secrets}")
	r := p.check(t)
	found := false
	for _, f := range r.rule("FL-C001") {
		found = found || (f.Severity == "warning" && strings.Contains(f.text(), "requires CRD platform.example.test/Widget from flux-system/platform-api, but nothing orders them"))
	}
	if !found {
		t.Errorf("the contract's CRD requirement is unordered: %+v", r.problems())
	}
}

func TestOperatorPathVariableTypo(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "clusters/prod/cluster-vars.yaml", `ctrl_green_path: "./config/default"`, `ctrl_green_path: "./config/defualt"`)
	r := p.check(t)
	failed := false
	for _, f := range r.rule("FL-G008") {
		failed = failed || (f.Component == "operator-system/operator-green" && strings.Contains(f.text(), "config/defualt"))
	}
	if !failed {
		t.Errorf("a path that does not exist at the pinned tag fails the build: %+v", r.problems())
	}
}

func TestPrivateChartNeedsCredentials(t *testing.T) {
	p := internalPlatform(t)
	t.Setenv("NETRC", filepath.Join(t.TempDir(), "none"))
	r := check(t, p.dir)
	var msg string
	for _, f := range r.rule("FL-X001") {
		msg += f.text()
	}
	if !strings.Contains(msg, "HelmRelease/telemetry/metrics-agent") || !strings.Contains(msg, "401") || !strings.Contains(msg, ".netrc") {
		t.Fatalf("an unauthenticated run must say why and how to fix it: %q", msg)
	}
	// with credentials the chart renders, and the variable reaches its values
	edit(t, p.dir, "clusters/prod/cluster-vars.yaml", "  cluster_name: \"management\"\n", "")
	r = p.check(t)
	var text string
	for _, f := range r.problems() {
		text += f.Rule + " " + f.text() + "\n"
	}
	for _, want := range []string{"FL-S001", "${cluster_name}", "FL-G008", "cluster is required"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: the chart must actually be rendered with the release's values:\n%s", want, text)
		}
	}
}

func TestOCIArtifactTagMissing(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, "clusters/prod/platform.yaml", "ref: {tag: 1.0.0}", "ref: {tag: 9.9.9}")
	r := p.check(t)
	var text string
	for _, f := range r.problems() {
		text += f.Rule + " " + f.text() + "\n"
	}
	for _, want := range []string{"FL-X001", "flux-system/eso-crds", "9.9.9", "FL-G005", "external-secrets.io"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}
