//go:build e2e

// Package e2e runs the fluxlint binary against a realistic single-cluster
// platform repository (testdata/platform) built from real, pinned upstream components:
// cert-manager, kyverno, cluster-api-operator with the AWS provider, and
// podinfo both as blue/green Kustomizations from its Git repository and as an
// OCI Helm chart.
//
// The baseline must be clean. Every other test seeds exactly one defect of a
// kind that a throw-away-cluster test would (eventually) trip over, and asserts
// that fluxlint names it.
//
// These tests need network access on a cold cache:
//
//	go test -tags e2e ./e2e/...
//
// Set FLUXLINT_E2E_CACHE to keep the source cache between runs.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	binary   string
	cacheDir string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "fluxlint-e2e-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(tmp, "fluxlint")
	if out, err := exec.Command("go", "build", "-o", binary, "../cmd/fluxlint").CombinedOutput(); err != nil {
		panic("build: " + string(out))
	}
	if cacheDir = os.Getenv("FLUXLINT_E2E_CACHE"); cacheDir == "" {
		cacheDir = filepath.Join(tmp, "cache")
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

type finding struct {
	Rule, Severity, Entrypoint, Component, Object, Message string
	Detail                                                 []string
}

type result struct {
	ExitCode int
	Findings []finding
	Bound    map[string]time.Duration // entrypoint -> worst-case bound
	Elapsed  time.Duration
}

// repo copies the platform fixture so a test can break it.
func repo(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "platform")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func check(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"check", "--repo", dir, "--cache-dir", cacheDir, "--format", "json"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := result{Elapsed: time.Since(start), Bound: map[string]time.Duration{}}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	case err != nil:
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 2 {
		t.Fatalf("fluxlint failed to run: %s", stderr.String())
	}
	var out struct {
		Entrypoints []struct {
			Entrypoint string
			Findings   []finding
			Timing     *struct{ Bound time.Duration }
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, stdout.String())
	}
	for _, e := range out.Entrypoints {
		res.Findings = append(res.Findings, e.Findings...)
		if e.Timing != nil {
			res.Bound[e.Entrypoint] = e.Timing.Bound
		}
	}
	return res
}

// problems are findings that are not suggestions.
func (r result) problems() []finding {
	var out []finding
	for _, f := range r.Findings {
		if f.Severity != "info" {
			out = append(out, f)
		}
	}
	return out
}

func (r result) rule(id string) []finding {
	var out []finding
	for _, f := range r.Findings {
		if f.Rule == id {
			out = append(out, f)
		}
	}
	return out
}

func (f finding) text() string {
	return f.Component + " " + f.Object + " " + f.Message + " " + strings.Join(f.Detail, " ")
}

// expectOnly asserts that the only problems are of the given rule and that at
// least one of them mentions every needle.
func expectOnly(t *testing.T, r result, rule string, needles ...string) {
	t.Helper()
	var matched bool
	for _, f := range r.problems() {
		if f.Rule != rule {
			t.Errorf("unexpected %s %s: %s", f.Severity, f.Rule, f.text())
			continue
		}
		ok := true
		for _, n := range needles {
			ok = ok && strings.Contains(f.text(), n)
		}
		matched = matched || ok
	}
	if !matched {
		t.Errorf("no %s finding mentions %q; got %+v", rule, needles, r.rule(rule))
	}
}

func edit(t *testing.T, dir, rel, old, new string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------

func TestBaselineIsCleanAndFast(t *testing.T) {
	dir := repo(t)
	cold := check(t, dir)
	if p := cold.problems(); len(p) != 0 || cold.ExitCode != 0 {
		t.Fatalf("baseline must be clean (exit %d): %+v", cold.ExitCode, p)
	}
	if cold.Bound["clusters/production"] == 0 {
		t.Error("no timing result")
	}
	t.Logf("cold: %v", cold.Elapsed)

	// The lodestar: with sources cached, pass/fail in well under 10s — and
	// without touching the network at all.
	warm := check(t, dir, "--offline")
	t.Logf("warm, offline: %v", warm.Elapsed)
	if len(warm.problems()) != 0 {
		t.Fatalf("offline run with a warm cache must match the online one: %+v", warm.problems())
	}
	if warm.Elapsed > 10*time.Second {
		t.Errorf("warm run took %v; the budget is 10s", warm.Elapsed)
	}
}

func TestEverythingIsRendered(t *testing.T) {
	r := check(t, repo(t), "--offline")
	if got := r.rule("FL-X001"); len(got) != 0 {
		t.Fatalf("some sources were not rendered: %+v", got)
	}
	// Proof that real charts and the external repo were rendered and joined the
	// graph: their CRDs justify the dependencies that exist because of them.
	for _, f := range r.rule("FL-T004") {
		for _, justified := range []string{"infra-controllers", "infra-capi-providers"} {
			if strings.Contains(f.Message, "dependsOn flux-system/"+justified) {
				t.Errorf("dependency on %s should be justified by rendered CRDs: %s", justified, f.text())
			}
		}
	}
}

// A HelmRelease in a wait: true Kustomization whose HelmRepository lives in a
// Kustomization that dependsOn the first one.
func TestDeadlockHelmRepositoryBehindDependsOn(t *testing.T) {
	dir := repo(t)
	const repoYAML = `---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: kyverno
  namespace: kyverno
spec:
  interval: 24h
  url: https://kyverno.github.io/kyverno/
`
	edit(t, dir, "infrastructure/base/controllers/kyverno.yaml", repoYAML, "")
	write(t, dir, "infrastructure/production/configs/kyverno-repo.yaml", repoYAML)
	edit(t, dir, "infrastructure/production/configs/kustomization.yaml", "resources:\n", "resources:\n  - kyverno-repo.yaml\n")
	r := check(t, dir, "--offline")
	if r.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", r.ExitCode)
	}
	expectOnly(t, r, "FL-G002", "HelmRelease/kyverno/kyverno", "flux-system/infra-configs", "dependsOn")
}

// A parent applies objects into a namespace that only its own child creates.
func TestDeadlockNamespaceCreatedByOwnChild(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/namespaces/namespaces.yaml", "  name: team-a\n", "  name: team-a-moved\n")
	write(t, dir, "apps/base/team-namespaces/ns.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-a\n")
	write(t, dir, "apps/production/team-namespaces.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: team-namespaces
  namespace: flux-system
spec:
  interval: 1h
  retryInterval: 1m
  sourceRef:
    kind: GitRepository
    name: flux-system
  path: ./apps/base/team-namespaces
  prune: false
`)
	edit(t, dir, "apps/production/kustomization.yaml", "resources:\n", "resources:\n  - team-namespaces.yaml\n")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-G002", "flux-system/apps", "flux-system/team-namespaces", "namespace team-a")
}

// A namespace manifest whose metadata.name does not match what its consumers
// use (a typo, or a name copy-pasted from the file next to it).
func TestMislabelledNamespace(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/namespaces/namespaces.yaml", "  name: team-a\n", "  name: team-b\n")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-G004", `"team-a"`, "ServiceAccount/team-a/team-a")
}

// Two manifests that declare the same object make kustomize refuse the build,
// exactly as kustomize-controller would.
func TestDuplicateObjectFailsTheBuild(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/namespaces/namespaces.yaml", "  name: team-a\n", "  name: podinfo\n")
	r := check(t, dir, "--offline")
	var failed bool
	for _, f := range r.rule("FL-G008") {
		failed = failed || (f.Component == "flux-system/namespaces" && strings.Contains(f.text(), "already registered id"))
	}
	if !failed {
		t.Fatalf("duplicate Namespace must fail the namespaces build: %+v", r.problems())
	}
}

func TestUndefinedSubstitutionVariable(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/cluster-vars.yaml", "  cluster_name: \"management\"\n", "")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-S001", "${cluster_name}", "HelmRelease/podinfo-helm/podinfo")
}

func TestDualOwnership(t *testing.T) {
	dir := repo(t)
	write(t, dir, "apps/production/podinfo-ns.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: podinfo\n")
	edit(t, dir, "apps/production/kustomization.yaml", "resources:\n", "resources:\n  - podinfo-ns.yaml\n")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-G003", "Namespace/podinfo", "flux-system/apps", "flux-system/namespaces")
}

// Custom resources whose CRDs only a chart installs, with the ordering removed.
func TestCRDFromChartWithoutOrdering(t *testing.T) {
	dir := repo(t)
	// the first such block belongs to infra-configs
	edit(t, dir, "clusters/production/infrastructure.yaml", "  dependsOn:\n    - name: infra-controllers\n", "")
	r := check(t, dir, "--offline")
	for _, f := range r.problems() {
		if f.Rule != "FL-T006" {
			t.Errorf("unexpected %s: %s", f.Rule, f.text())
		}
	}
	var issuer, policy bool
	for _, f := range r.rule("FL-T006") {
		issuer = issuer || strings.Contains(f.text(), "cert-manager.io/ClusterIssuer from HelmRelease/cert-manager/cert-manager")
		policy = policy || strings.Contains(f.text(), "kyverno.io/ClusterPolicy from HelmRelease/kyverno/kyverno")
	}
	if !issuer || !policy {
		t.Errorf("both the ClusterIssuer and the ClusterPolicy depend on chart-installed CRDs: %+v", r.rule("FL-T006"))
	}
}

// CAPA's CRDs appear only once the operator has reconciled the provider.
func TestRuntimeInstalledCRDKeepsItsOrdering(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/apps.yaml", "    - name: infra-capi-providers\n", "")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-T006", "infrastructure.cluster.x-k8s.io/AWSClusterTemplate", "flux-system/infra-capi-providers")
}

// capi-operator dependsOn cert-manager inside infra-controllers, so the parent
// must allow for both release timeouts.
func TestTimeoutInversion(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/infrastructure.yaml", "  timeout: 15m\n", "  timeout: 5m\n")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-T008", "flux-system/infra-controllers", "HelmRelease/capi-operator-system/capi-operator", "10m30s")
}

func TestRetryCliff(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/podinfo/green/podinfo.yaml", "  retryInterval: 1m\n", "")
	r := check(t, dir, "--offline")
	expectOnly(t, r, "FL-T007", "podinfo/podinfo-green", "30m")
}

func TestBootstrapBudget(t *testing.T) {
	dir := repo(t)
	edit(t, dir, ".fluxlint.yaml", "entrypoints:\n", "timing:\n  maxBootstrapBound: 10m\nentrypoints:\n")
	r := check(t, dir, "--offline")
	if r.ExitCode != 1 {
		t.Errorf("exceeding the budget must fail the run, exit = %d", r.ExitCode)
	}
	expectOnly(t, r, "FL-T100", "exceeds the budget of 10m")
}

// The routine change in a repository like this: move a colour to a new
// version. The new tag is fetched once; the result stays clean.
func TestBlueGreenVersionBump(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/cluster-vars.yaml", `podinfo_blue_version: "6.14.1"`, `podinfo_blue_version: "6.14.0"`)
	edit(t, dir, "clusters/production/cluster-vars.yaml", `podinfo_blue_scale: "0"`, `podinfo_blue_scale: "3"`)
	r := check(t, dir)
	if p := r.problems(); len(p) != 0 {
		t.Fatalf("a version bump to an existing tag must stay clean: %+v", p)
	}
}

func TestVersionBumpToTagThatDoesNotExist(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/cluster-vars.yaml", `podinfo_green_version: "6.15.0"`, `podinfo_green_version: "6.15.0-does-not-exist"`)
	r := check(t, dir)
	expectOnly(t, r, "FL-X001", "podinfo/podinfo-green", "6.15.0-does-not-exist")
}

func TestChartVersionThatDoesNotExist(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "infrastructure/base/controllers/cert-manager.yaml", "version: v1.19.6", "version: v0.0.0-does-not-exist")
	r := check(t, dir)
	var unavailable bool
	for _, f := range r.rule("FL-X001") {
		unavailable = unavailable || strings.Contains(f.text(), "HelmRelease/cert-manager/cert-manager")
	}
	if !unavailable {
		t.Fatalf("missing chart version not reported: %+v", r.problems())
	}
	// and the ClusterIssuer's CRD is then unknown rather than assumed
	if got := r.rule("FL-G005"); len(got) == 0 || !strings.Contains(got[0].text(), "cert-manager.io") {
		t.Errorf("cert-manager.io CRDs should be unknown when the chart is unavailable: %+v", r.problems())
	}
}

func TestChartRejectsValues(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "infrastructure/base/controllers/cert-manager.yaml", "    replicaCount: 2\n", "    replicaCount: 2\n    thisValueDoesNotExist: true\n")
	r := check(t, dir, "--offline")
	// cert-manager ships a values.schema.json that forbids unknown keys
	var rejected bool
	for _, f := range r.rule("FL-G008") {
		rejected = rejected || strings.Contains(f.text(), "HelmRelease/cert-manager/cert-manager")
	}
	if !rejected {
		t.Fatalf("values the chart's schema rejects must fail: %+v", r.problems())
	}
}

func TestFloatingBranchIsReported(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/podinfo/green/podinfo.yaml", "    tag: ${podinfo_green_version}\n", "    branch: master\n")
	r := check(t, dir)
	if p := r.problems(); len(p) != 0 {
		t.Errorf("a branch ref is a suggestion, not a problem: %+v", p)
	}
	if got := r.rule("FL-X002"); len(got) != 1 || !strings.Contains(got[0].text(), "branch master") {
		t.Errorf("floating ref not reported: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Runtime wiring: things that apply cleanly and then never run.

// The pattern used to adapt an upstream operator: a JSON patch that rewires an
// env entry by position. fluxlint renders the external repository at the
// pinned tag, so it can say what the index hits today.
func TestPositionalPatchOnExternalRepository(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/podinfo/green/podinfo.yaml", "  patches:\n", `  patches:
    - target:
        kind: Deployment
        name: podinfo
      patch: |
        - op: replace
          path: /spec/template/spec/containers/0/env/0/value
          value: "#000000"
`)
	r := check(t, dir)
	expectOnly(t, r, "FL-R003", "podinfo/podinfo-green", "env/0/value", `currently "PODINFO_UI_COLOR"`)
}

func TestSecretKeyThatDoesNotExist(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/podinfo/green/podinfo.yaml", "  patches:\n", `  patches:
    - target:
        kind: Deployment
        name: podinfo
      patch: |
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: podinfo
        spec:
          template:
            spec:
              containers:
                - name: podinfod
                  env:
                    - name: API_TOKEN
                      valueFrom:
                        secretKeyRef:
                          name: podinfo-credentials
                          key: token
`)
	t.Run("nothing creates the secret", func(t *testing.T) {
		expectOnly(t, check(t, dir), "FL-R001", "Secret podinfo/podinfo-credentials", "which nothing creates")
	})
	t.Run("secret exists without the key", func(t *testing.T) {
		write(t, dir, "apps/production/podinfo-credentials.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: podinfo-credentials\n  namespace: podinfo\nstringData:\n  password: x\n")
		edit(t, dir, "apps/production/kustomization.yaml", "resources:\n", "resources:\n  - podinfo-credentials.yaml\n")
		expectOnly(t, check(t, dir), "FL-R001", `needs key "token"`, `only defines "password"`)
	})
	t.Run("secret has the key", func(t *testing.T) {
		edit(t, dir, "apps/production/podinfo-credentials.yaml", "  password: x\n", "  password: x\n  token: y\n")
		if p := check(t, dir).problems(); len(p) != 0 {
			t.Fatalf("resolved reference must be clean: %+v", p)
		}
	})
}

// cert-manager's webhook fails closed. Scaled to zero, every Certificate and
// Issuer apply in the cluster is rejected.
func TestFailClosedWebhookScaledToZero(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "infrastructure/base/controllers/cert-manager.yaml", "    replicaCount: 2\n", "    replicaCount: 2\n    webhook:\n      replicaCount: 0\n")
	r := check(t, dir)
	expectOnly(t, r, "FL-R004", "cert-manager-webhook", "scaled to 0")
}

// ---------------------------------------------------------------------------
// Admission: objects the API server would reject, checked against the real
// CRDs that the charts install.

func TestCustomResourceRejectedByChartCRD(t *testing.T) {
	dir := repo(t)
	// cert-manager's ClusterIssuer schema: acme.server is required and
	// privateKeySecretRef must be an object
	edit(t, dir, "infrastructure/production/configs/cluster-issuer.yaml", "  selfSigned: {}\n", "  acme:\n    email: ops@example.test\n    privateKeySecretRef: not-an-object\n")
	r := check(t, dir)
	expectOnly(t, r, "FL-V001", "ClusterIssuer/selfsigned", "spec.acme")
}

func TestCustomResourceVersionNotServed(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "infrastructure/production/configs/require-team-label.yaml", "apiVersion: kyverno.io/v1\n", "apiVersion: kyverno.io/v9\n")
	r := check(t, dir)
	expectOnly(t, r, "FL-G006", "ClusterPolicy/require-team-label", "kyverno.io/v9")
}

// Every namespace in the fixture enforces the baseline Pod Security level.
func TestPodSecurityViolation(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/podinfo/green/podinfo.yaml", "  patches:\n", `  patches:
    - target:
        kind: Deployment
        name: podinfo
      patch: |
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: podinfo
        spec:
          template:
            spec:
              hostNetwork: true
`)
	r := check(t, dir)
	expectOnly(t, r, "FL-V002", "Deployment/podinfo/podinfo-green", "baseline", "host namespaces")
}

// ---------------------------------------------------------------------------
// Reporting

// Findings point at the manifest to fix — also when the offending object lives
// inside a chart: then it is the HelmRelease that pulls the chart in.
func TestFindingsCarryRepositoryLocations(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "apps/base/namespaces/namespaces.yaml", "  name: team-a\n", "  name: team-b\n")
	edit(t, dir, "infrastructure/base/controllers/cert-manager.yaml", "    replicaCount: 2\n", "    replicaCount: 2\n    webhook:\n      replicaCount: 0\n")

	report := filepath.Join(t.TempDir(), "gl-code-quality-report.json")
	cmd := exec.Command(binary, "check", "--repo", dir, "--cache-dir", cacheDir, "--offline", "--format", "gitlab", "--output", report)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		t.Error("errors must fail the run (exit 1) even when a report file is written")
	}
	if !strings.Contains(stdout.String(), "FL-G004") {
		t.Errorf("with --output the readable report still goes to stdout:\n%s", stdout.String())
	}

	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var issues []struct {
		CheckName string `json:"check_name"`
		Location  struct {
			Path  string
			Lines struct{ Begin int }
		}
	}
	if err := json.Unmarshal(b, &issues); err != nil {
		t.Fatalf("not a Code Quality report: %v\n%s", err, b)
	}
	where := map[string]string{}
	for _, i := range issues {
		where[i.CheckName] = i.Location.Path
		if i.Location.Lines.Begin < 1 {
			t.Errorf("%s: line must be >= 1", i.CheckName)
		}
	}
	for rule, want := range map[string]string{
		"FL-G004": "apps/base/rbac/team-a.yaml",                        // the ServiceAccount whose namespace is missing
		"FL-R004": "infrastructure/base/controllers/cert-manager.yaml", // a webhook inside the chart -> its HelmRelease
	} {
		if where[rule] != want {
			t.Errorf("%s located at %q, want %q (all: %v)", rule, where[rule], want, where)
		}
	}
}

// ---------------------------------------------------------------------------
// Repository-specific invariants

// A blue/green convention: the colour that serves traffic must not be the one
// that is drained. The variables live on `apps`; the Deployments are rendered
// two levels below it, from an external repository.
func TestAssertionOverVariablesAndExternalObjects(t *testing.T) {
	dir := repo(t)
	edit(t, dir, "clusters/production/cluster-vars.yaml", "\ndata:\n", "\ndata:\n  podinfo_live: \"green\"\n")
	edit(t, dir, ".fluxlint.yaml", "entrypoints:\n", `assertions:
  - name: live podinfo colour serves traffic
    mustMatch: true
    match: {kind: Deployment, namespace: podinfo, name: "podinfo-*"}
    expr: '!object.metadata.name.endsWith("-" + vars.podinfo_live) || object.spec.replicas > 0'
    message: the colour named by podinfo_live is scaled to zero
entrypoints:
`)
	if p := check(t, dir).problems(); len(p) != 0 {
		t.Fatalf("green is live with 3 replicas: %+v", p)
	}
	// the mistake: flip the pointer without scaling the other colour up
	edit(t, dir, "clusters/production/cluster-vars.yaml", `podinfo_live: "green"`, `podinfo_live: "blue"`)
	r := check(t, dir)
	expectOnly(t, r, "FL-A001", "Deployment/podinfo/podinfo-blue", "scaled to zero")
}
