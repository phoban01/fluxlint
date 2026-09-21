package lint_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
)

const kyvernoPolicies = `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata: {name: require-team}
spec:
  validationFailureAction: Enforce
  background: true
  rules:
    - name: team-label
      match: {any: [{resources: {kinds: [Deployment]}}]}
      validate:
        message: the label team is required
        pattern: {metadata: {labels: {team: "?*"}}}
---
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata: {name: no-latest}
spec:
  validationFailureAction: Audit
  rules:
    - name: pinned-tag
      match: {any: [{resources: {kinds: [Deployment]}}]}
      validate:
        message: images must be pinned
        pattern: {spec: {template: {spec: {containers: [{image: "!*:latest"}]}}}}
---
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata: {name: copy-labels}
spec:
  rules:
    - name: only-mutates
      match: {any: [{resources: {kinds: [Deployment]}}]}
      mutate: {patchStrategicMerge: {metadata: {labels: {+(managed): "true"}}}}
`

func kyvernoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "clusters/prod/all.yaml", fmt.Sprintf(ksHeader, "policies", "policies", "")+"---\n"+fmt.Sprintf(ksHeader, "app", "app", ""))
	write(t, dir, "policies/all.yaml", kyvernoPolicies)
	labelled := strings.Replace(deployment("owned", "default", 1, "containers: [{name: a, image: \"registry.example.test/a:latest\"}]"),
		"  name: owned\n", "  name: owned\n  labels: {team: payments}\n", 1)
	write(t, dir, "app/all.yaml", labelled+deployment("stray", "default", 1, "containers: [{name: a, image: \"registry.example.test/a:v1\"}]"))
	return dir
}

// The verdict is Kyverno's own, so the test needs Kyverno's CLI. CI installs it.
func TestKyvernoPolicies(t *testing.T) {
	if _, err := exec.LookPath("kyverno"); err != nil {
		t.Skip("the kyverno CLI is not installed")
	}
	cfg := config.Default()
	cfg.Externals.CRDGroups = append(cfg.Externals.CRDGroups, "kyverno.io")
	got := find(analyseDir(t, kyvernoRepo(t), cfg), "FL-V007")
	var errs, infos []lint.Finding
	for _, f := range got {
		if f.Severity == lint.Error {
			errs = append(errs, f)
		} else {
			infos = append(infos, f)
		}
	}
	if len(errs) != 1 || !strings.Contains(messages(errs), "Deployment/default/stray") || !strings.Contains(messages(errs), "the label team is required") {
		t.Errorf("want the unlabelled Deployment rejected by the Enforce policy:\n%s", messages(errs))
	}
	if len(infos) != 1 || !strings.Contains(messages(infos), "Deployment/default/owned") || !strings.Contains(messages(infos), "(Audit)") {
		t.Errorf("want the Audit failure as a suggestion:\n%s", messages(infos))
	}
}

// Without the CLI the policies are not silently skipped.
func TestKyvernoNotInstalled(t *testing.T) {
	cfg := config.Default()
	cfg.Externals.CRDGroups = append(cfg.Externals.CRDGroups, "kyverno.io")
	cfg.Kyverno.Command = "kyverno-is-not-installed-here"
	got := find(analyseDir(t, kyvernoRepo(t), cfg), "FL-V007")
	if len(got) != 1 || got[0].Severity != lint.Info || !strings.Contains(messages(got), "2 Kyverno validation policies were not evaluated") {
		t.Errorf("got:\n%s", messages(got))
	}
}
