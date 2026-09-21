package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
)

func fluxObject(kind, namespace, name, ready, message string, suspend bool) string {
	apiVersion := "kustomize.toolkit.fluxcd.io/v1"
	if kind == "HelmRelease" {
		apiVersion = "helm.toolkit.fluxcd.io/v2"
	}
	return fmt.Sprintf(`{"apiVersion": %q, "kind": %q, "metadata": {"namespace": %q, "name": %q}, "spec": {"suspend": %v},
  "status": {"conditions": [{"type": "Reconciling", "status": "False"}, {"type": "Ready", "status": %q, "reason": "R", "message": %q}]}}`,
		apiVersion, kind, namespace, name, suspend, ready, message)
}

func TestParseClusterState(t *testing.T) {
	list := `{"kind": "List", "items": [` + strings.Join([]string{
		fluxObject("Kustomization", "flux-system", "apps", "True", "Applied revision: main@sha1:abc", false),
		fluxObject("HelmRelease", "cert-manager", "cert-manager", "False", "install retries exhausted", false),
		fluxObject("Kustomization", "flux-system", "paused", "False", "x", true),
		`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x"}}`,
		`{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": {"namespace": "flux-system", "name": "new"}}`,
	}, ",") + `]}`
	got, err := lint.ParseClusterState(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 objects (suspended and foreign ones left out), got %v", got)
	}
	if !got["flux-system/apps"].Ready || got["HelmRelease/cert-manager/cert-manager"].Ready || got["flux-system/new"].Reason != "NoStatus" {
		t.Errorf("unexpected state: %+v", got)
	}
	if _, err := lint.ParseClusterState(strings.NewReader(`{"kind": "List", "items": []}`)); err == nil {
		t.Error("an empty list must be an error: it is almost certainly the wrong file")
	}
}

// Four Kustomizations: `broken` has an error fluxlint finds, `after` depends
// on it, `fine` is clean, `surprise` is clean as far as fluxlint can tell.
func TestClusterStateComparison(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml",
		fmt.Sprintf(ksHeader, "broken", "broken", "")+"---\n"+
			fmt.Sprintf(ksHeader, "after", "after", "  dependsOn:\n    - name: broken\n")+"---\n"+
			fmt.Sprintf(ksHeader, "fine", "fine", "")+"---\n"+
			fmt.Sprintf(ksHeader, "surprise", "surprise", "")+"---\n"+
			fmt.Sprintf(ksHeader, "surprise-extra", "surprise-extra", ""))
	write(t, dir, "broken/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: nowhere}\n")
	for _, name := range []string{"after", "fine", "surprise", "surprise-extra"} {
		write(t, dir, name+"/cm.yaml", fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: %s, namespace: default}\n", name))
	}

	run := func(state map[string]config.ClusterStatus) *lint.Result {
		cfg := config.Default()
		cfg.ClusterState = state
		return analyseDir(t, dir, cfg)
	}
	down := config.ClusterStatus{Reason: "HealthCheckFailed", Message: "timeout waiting for: [Deployment/default/x status: 'InProgress']"}
	up := config.ClusterStatus{Ready: true}

	t.Run("agreement is silent", func(t *testing.T) {
		r := run(map[string]config.ClusterStatus{"flux-system/broken": down, "flux-system/after": down, "flux-system/fine": up,
			"flux-system/surprise": up, "flux-system/surprise-extra": up})
		for _, rule := range []string{"FL-O001", "FL-O002", "FL-O003"} {
			if got := find(r, rule); len(got) > 0 {
				t.Errorf("unexpected %s:\n%s", rule, messages(got))
			}
		}
	})
	t.Run("a failure nothing predicted", func(t *testing.T) {
		r := run(map[string]config.ClusterStatus{"flux-system/broken": down, "flux-system/after": down, "flux-system/fine": up,
			"flux-system/surprise": down, "flux-system/surprise-extra": up})
		got := find(r, "FL-O001")
		if len(got) != 1 || got[0].Component != "flux-system/surprise" || !strings.Contains(messages(got), "HealthCheckFailed") {
			t.Errorf("want one finding on surprise with the cluster's message, got:\n%s", messages(got))
		}
	})
	t.Run("a name that only starts the same is not a prediction", func(t *testing.T) {
		// an error on broken-... must not excuse broken, and vice versa: here
		// surprise-extra fails and only `surprise` would be mentioned
		r := run(map[string]config.ClusterStatus{"flux-system/broken": down, "flux-system/after": down, "flux-system/fine": up,
			"flux-system/surprise": up, "flux-system/surprise-extra": down})
		if got := find(r, "FL-O001"); len(got) != 1 || got[0].Component != "flux-system/surprise-extra" {
			t.Errorf("got:\n%s", messages(got))
		}
	})
	t.Run("an error the cluster does not confirm", func(t *testing.T) {
		r := run(map[string]config.ClusterStatus{"flux-system/broken": up, "flux-system/after": up, "flux-system/fine": up,
			"flux-system/surprise": up, "flux-system/surprise-extra": up})
		got := find(r, "FL-O002")
		if len(got) != 1 || got[0].Component != "flux-system/broken" || !strings.Contains(messages(got), "FL-G004") {
			t.Errorf("got:\n%s", messages(got))
		}
	})
	t.Run("one side only", func(t *testing.T) {
		r := run(map[string]config.ClusterStatus{"flux-system/fine": up, "flux-system/elsewhere": up})
		got := messages(find(r, "FL-O003"))
		for _, want := range []string{"flux-system/surprise", "flux-system/elsewhere", "4 component(s)", "1 Flux object(s)"} {
			if !strings.Contains(got, want) {
				t.Errorf("FL-O003 lacks %q:\n%s", want, got)
			}
		}
	})
}
