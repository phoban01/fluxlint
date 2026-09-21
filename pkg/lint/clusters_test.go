package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/lint"
)

// A management cluster that also applies to a workload cluster, through
// spec.kubeConfig. `addons` and `spoke-addons` render the same manifests, one
// per cluster.
func hubAndSpoke(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := "  kubeConfig:\n    secretRef: {name: spoke-kubeconfig}\n"
	write(t, dir, "clusters/prod/all.yaml",
		fmt.Sprintf(ksHeader, "addons", "addons", "")+"---\n"+
			fmt.Sprintf(ksHeader, "spoke-addons", "addons", remote)+"---\n"+
			fmt.Sprintf(ksHeader, "hub-app", "hub-app", "  dependsOn:\n    - name: addons\n")+"---\n"+
			fmt.Sprintf(ksHeader, "spoke-app", "spoke-app", remote+"  dependsOn:\n    - name: addons\n"))
	write(t, dir, "addons/all.yaml", `apiVersion: v1
kind: Namespace
metadata: {name: tools}
---
apiVersion: v1
kind: Secret
metadata: {name: tool-credentials, namespace: tools}
stringData: {token: x}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: tool, namespace: tools}
`)
	// only the hub creates this Secret
	write(t, dir, "hub-app/all.yaml", `apiVersion: v1
kind: Secret
metadata: {name: hub-only, namespace: tools}
stringData: {key: x}
`+deployment("hub-app", "tools", 1, "serviceAccountName: tool\ncontainers:\n  - name: a\n    image: registry.example.test/a:v1\n    envFrom: [{secretRef: {name: hub-only}}]\n"))
	// the spoke's pod wants it too, and it is not in the spoke
	write(t, dir, "spoke-app/all.yaml", deployment("spoke-app", "tools", 1,
		"serviceAccountName: tool\ncontainers:\n  - name: a\n    image: registry.example.test/a:v1\n    envFrom: [{secretRef: {name: hub-only}}, {secretRef: {name: tool-credentials}}]\n"))
	return dir
}

func TestObjectsInDifferentClustersNeverMeet(t *testing.T) {
	r := analyseDir(t, hubAndSpoke(t), nil)

	if got := find(r, "FL-G003"); len(got) != 0 {
		t.Errorf("the same manifests applied to two clusters are not one object with two owners:\n%s", messages(got))
	}
	// the namespace and ServiceAccount exist in both clusters, from each one's own addons
	for _, rule := range []string{"FL-G004", "FL-R002"} {
		if got := find(r, rule); len(got) != 0 {
			t.Errorf("unexpected %s:\n%s", rule, messages(got))
		}
	}
	// the hub's Secret does not satisfy the spoke's pod
	got := find(r, "FL-R001")
	if len(got) != 1 || got[0].Component != "flux-system/spoke-app" || !strings.Contains(messages(got), "tools/hub-only") {
		t.Errorf("want exactly the spoke's reference to the hub's Secret:\n%s", messages(got))
	}
}

// dependsOn lives in the cluster Flux runs in, so it orders across clusters.
// spoke-app needs the namespace from spoke-addons and says it depends on
// addons, which is the hub's: the right namespace is unordered.
func TestOrderingCrossesClusters(t *testing.T) {
	r := analyseDir(t, hubAndSpoke(t), nil)
	var hub, spoke bool
	for _, f := range find(r, "FL-T006") {
		if f.Component == "flux-system/spoke-app" && strings.Contains(f.Message, "namespace tools from flux-system/spoke-addons") {
			spoke = true
		}
		hub = hub || f.Component == "flux-system/hub-app"
	}
	if !spoke || hub {
		t.Errorf("spoke-app is not ordered after the component that creates its namespace in its cluster; hub-app is:\n%s", messages(find(r, "FL-T006")))
	}
	if got := find(r, "FL-G002"); len(got) != 0 {
		t.Errorf("no deadlock here:\n%s", messages(got))
	}
	_ = lint.Info
}

// Charts set metadata.namespace on cluster-scoped objects by mistake. The API
// server ignores it, so it does not need a namespace to exist.
func TestNamespaceOnClusterScopedObjectIsIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "rbac", "rbac", ""))
	write(t, dir, "rbac/all.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: scraper, namespace: monitoring}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects: [{kind: ServiceAccount, name: default, namespace: default}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings, namespace: nowhere}
`)
	got := find(analyseDir(t, dir, nil), "FL-G004")
	if len(got) != 1 || !strings.Contains(messages(got), `"nowhere"`) {
		t.Errorf("want only the ConfigMap's namespace:\n%s", messages(got))
	}
}
