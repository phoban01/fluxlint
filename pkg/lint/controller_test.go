package lint_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/source"
)

// apiRepo is a Go module that ships the Widget CRD, tagged v1.0.0 and v1.1.0.
func apiRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "go.mod", "module example.test/api\n\ngo 1.22\n")
	write(t, dir, "config/crd/widget.yaml", widgetCRD)
	for _, tag := range []string{"v1.0.0", "v1.1.0"} {
		write(t, dir, "VERSION", tag)
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "--quiet", "-m", tag)
		git(t, dir, "tag", tag)
	}
	return dir
}

// controllerRepo is an operator built against example.test/api v1.1.0. Its
// RBAC names widgets, and its contract says what it cannot run without.
func controllerRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "go.mod", "module example.test/operator\n\ngo 1.22\n\nrequire example.test/api v1.1.0\n")
	write(t, dir, "fluxlint-contract.yaml", `requires:
  crds:
    - {group: example.test, kind: Widget, version: v1}
  secrets:
    - {name: operator-credentials, keys: [token]}
`)
	write(t, dir, "config/manager.yaml", deployment("manager", "operator-system", 1, "serviceAccountName: manager\ncontainers:\n  - name: manager\n    image: example.test/operator:1\n")+`---
apiVersion: v1
kind: ServiceAccount
metadata: {name: manager, namespace: operator-system}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: manager}
rules:
  - apiGroups: [example.test]
    resources: [widgets, widgets/status]
    verbs: [get, list, watch, update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: manager}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: manager}
subjects:
  - {kind: ServiceAccount, name: manager, namespace: operator-system}
`)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "operator")
	git(t, dir, "tag", "v2.0.0")
	return dir
}

func controllerCluster(t *testing.T, api, operator, apiTag, operatorExtra, extraObjects string) string {
	t.Helper()
	dir := t.TempDir()
	gitSource := func(name, url, tag string) string {
		return fmt.Sprintf("---\napiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: %s\n  namespace: flux-system\nspec:\n  interval: 10m\n  url: file://%s\n  ref:\n    tag: %s\n", name, url, tag)
	}
	external := func(name, path, extra string) string {
		ks := fmt.Sprintf(ksHeader, name, strings.TrimPrefix(path, "./"), extra)
		return strings.Replace(ks, "    name: flux-system\n", "    name: "+name+"\n", 1)
	}
	write(t, dir, "clusters/prod/all.yaml",
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: operator-system\n"+extraObjects+
			gitSource("api", api, apiTag)+gitSource("operator", operator, "v2.0.0")+"---\n"+
			external("api", "./config/crd", "  wait: true\n  timeout: 2m\n")+"---\n"+
			external("operator", "./config", operatorExtra))
	return dir
}

func TestControllerContractSkewAndRBAC(t *testing.T) {
	api, operator := apiRepo(t), controllerRepo(t)
	opts := source.Options{CacheDir: t.TempDir()}

	r := analyseExternal(t, controllerCluster(t, api, operator, "v1.0.0", "", ""), opts)
	skew := find(r, "FL-R007")
	if len(skew) != 1 || !strings.Contains(skew[0].Message, "example.test/api v1.1.0") || !strings.Contains(skew[0].Message, "at v1.0.0") {
		t.Errorf("operator is built against api v1.1.0 but the CRDs are pinned at v1.0.0:\n%s", messages(r.Findings))
	}
	contract := messages(find(r, "FL-C001"))
	for _, want := range []string{
		"requires CRD example.test/Widget from flux-system/api, but nothing orders them",
		"requires Secret operator-system/operator-credentials, which nothing creates",
	} {
		if !strings.Contains(contract, want) {
			t.Errorf("FL-C001 lacks %q:\n%s", want, contract)
		}
	}

	// the same cluster with every requirement met
	secret := "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: operator-credentials\n  namespace: operator-system\nstringData:\n  token: x\n"
	r = analyseExternal(t, controllerCluster(t, api, operator, "v1.1.0", "  dependsOn:\n    - name: api\n", secret), opts)
	if got := problems(r); len(got) != 0 {
		t.Fatalf("contract met, versions aligned, ordering declared: %v\n%s", got, messages(r.Findings))
	}
	// and the dependsOn is justified: the operator's RBAC names an API that `api` installs
	if got := find(r, "FL-T004"); len(got) != 0 {
		t.Errorf("operator -> api is justified by its RBAC:\n%s", messages(got))
	}

	// a contract asking for a version the pinned CRD does not serve
	wrong := controllerRepo(t)
	write(t, wrong, "fluxlint-contract.yaml", "requires:\n  crds:\n    - {group: example.test, kind: Widget, version: v2}\n")
	git(t, wrong, "add", "-A")
	git(t, wrong, "commit", "--quiet", "-m", "v2 api")
	git(t, wrong, "tag", "-f", "v2.0.0")
	r = analyseExternal(t, controllerCluster(t, api, wrong, "v1.1.0", "  dependsOn:\n    - name: api\n", secret), source.Options{CacheDir: t.TempDir()})
	if got := find(r, "FL-C001"); len(got) != 1 || !strings.Contains(got[0].Message, "serves only v1") {
		t.Errorf("unserved version in contract:\n%s", messages(r.Findings))
	}
}
