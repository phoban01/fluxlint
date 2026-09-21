//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cli runs the binary as a user would and returns what it printed.
func cli(t *testing.T, env []string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return o.String(), e.String(), exit
}

func TestCLIVersionRulesExplain(t *testing.T) {
	if out, _, exit := cli(t, nil, "version"); exit != 0 || !strings.HasPrefix(out, "fluxlint ") {
		t.Errorf("version: exit %d, %q", exit, out)
	}
	out, _, exit := cli(t, nil, "rules")
	if exit != 0 || !strings.Contains(out, "FL-G002") || !strings.Contains(out, "bootstrap-deadlock") {
		t.Errorf("rules: exit %d, %q", exit, out)
	}
	if out, _, exit := cli(t, nil, "explain", "FL-G002"); exit != 0 || !strings.Contains(out, "bootstrap-deadlock") {
		t.Errorf("explain: exit %d, %q", exit, out)
	}
	if _, errOut, exit := cli(t, nil, "explain", "FL-NOPE"); exit != 2 || !strings.Contains(errOut, "unknown rule") {
		t.Errorf("explain of an unknown rule: exit %d, %q", exit, errOut)
	}
	if _, errOut, exit := cli(t, nil, "frobnicate"); exit != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown command: exit %d, %q", exit, errOut)
	}
	if _, _, exit := cli(t, nil); exit != 2 {
		t.Errorf("no arguments: exit %d, want 2", exit)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	p := internalPlatform(t)
	base := []string{"check", "--repo", p.dir, "--cache-dir", cacheDir}
	for name, args := range map[string][]string{
		"offline and refresh": {"--offline", "--refresh"},
		"unknown format":      {"--format", "xml"},
		"missing observed":    {"--observed", filepath.Join(p.dir, "absent.prom")},
	} {
		if _, errOut, exit := cli(t, []string{"NETRC=" + p.netrc}, append(base, args...)...); exit != 2 || !strings.Contains(errOut, "error:") {
			t.Errorf("%s: exit %d, stderr %q", name, exit, errOut)
		}
	}
	empty := t.TempDir()
	if _, errOut, exit := cli(t, nil, "check", "--repo", empty); exit != 2 || !strings.Contains(errOut, "no entrypoints") {
		t.Errorf("no entrypoints: exit %d, %q", exit, errOut)
	}
}

// The baseline carries one warning (the positional patch): it passes by
// default and fails under --fail-on warning.
func TestCLIFailOn(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	args := []string{"check", "--repo", p.dir, "--cache-dir", cacheDir}
	if out, _, exit := cli(t, env, args...); exit != 0 || !strings.Contains(out, "FL-R003") {
		t.Errorf("default: exit %d\n%s", exit, out)
	}
	if _, _, exit := cli(t, env, append(args, "--fail-on", "warning")...); exit != 1 {
		t.Errorf("--fail-on warning: exit %d, want 1", exit)
	}
}

func TestCLIFormats(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	args := []string{"check", "--repo", p.dir, "--cache-dir", cacheDir, "--format"}

	out, _, _ := cli(t, env, append(args, "sarif")...)
	var sarif struct {
		Version string
		Runs    []struct {
			Results []struct {
				RuleID    string `json:"ruleId"`
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &sarif); err != nil || sarif.Version != "2.1.0" || len(sarif.Runs) != 1 {
		t.Fatalf("sarif: %v\n%.300s", err, out)
	}
	located := false
	for _, res := range sarif.Runs[0].Results {
		if res.RuleID == "FL-R003" && len(res.Locations) > 0 && res.Locations[0].PhysicalLocation.ArtifactLocation.URI == "apps/operator-green.yaml" {
			located = true
		}
	}
	if !located {
		t.Errorf("sarif: the positional patch should be located in apps/operator-green.yaml\n%.600s", out)
	}

	out, _, _ = cli(t, env, append(args, "github")...)
	if !strings.Contains(out, "::warning file=apps/operator-green.yaml,line=") || !strings.Contains(out, "FL-R003") {
		t.Errorf("github annotations:\n%.400s", out)
	}

	// --output: the report goes to the file, readable text to stdout
	file := filepath.Join(t.TempDir(), "report.sarif")
	out, _, _ = cli(t, env, append(args, "sarif", "--output", file)...)
	if b, err := os.ReadFile(file); err != nil || !json.Valid(b) {
		t.Errorf("--output: %v", err)
	}
	if !strings.Contains(out, "clusters/prod:") || strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("--output should leave text on stdout:\n%.300s", out)
	}
}

func TestCLIGraph(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	args := []string{"graph", "--repo", p.dir, "--cache-dir", cacheDir}

	dot, _, exit := cli(t, env, append(args, "clusters/prod")...)
	if exit != 0 || !strings.HasPrefix(dot, "digraph") || !strings.Contains(dot, "operator-green") {
		t.Errorf("dot: exit %d\n%.300s", exit, dot)
	}
	mermaid, _, exit := cli(t, env, append(args, "--format", "mermaid", "clusters/prod")...)
	if exit != 0 || !strings.Contains(mermaid, "flowchart") && !strings.Contains(mermaid, "graph ") {
		t.Errorf("mermaid: exit %d\n%.300s", exit, mermaid)
	}
	if _, errOut, exit := cli(t, env, append(args, "--format", "png", "clusters/prod")...); exit != 2 || !strings.Contains(errOut, "dot and mermaid") {
		t.Errorf("unknown graph format: exit %d, %q", exit, errOut)
	}
	// the platform's .fluxlint.yaml names its one entrypoint
	if out, _, exit := cli(t, env, args...); exit != 0 || out != dot {
		t.Errorf("graph with the entrypoint from the config: exit %d", exit)
	}
	if _, errOut, exit := cli(t, nil, "graph", "--repo", t.TempDir()); exit != 2 || !strings.Contains(errOut, "one entrypoint") {
		t.Errorf("graph without an entrypoint: exit %d, %q", exit, errOut)
	}
}

// Observed reconcile durations add an expected time to the worst-case bound.
func TestCLIObserved(t *testing.T) {
	p := internalPlatform(t)
	observed := filepath.Join(t.TempDir(), "observed.yaml")
	if err := os.WriteFile(observed, []byte("flux-system/platform-api: 4s\nflux-system/apps: 20s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, exit := cli(t, []string{"NETRC=" + p.netrc}, "check", "--repo", p.dir, "--cache-dir", cacheDir, "--observed", observed)
	if exit != 0 || !strings.Contains(out, "expected about") || !strings.Contains(out, "observed") {
		t.Errorf("exit %d, stderr %q\n%.600s", exit, errOut, out)
	}
}

// A cluster test and fluxlint look at the same commit. Where the cluster
// fails and fluxlint said nothing, the run fails and says what the cluster saw.
func TestCLIClusterState(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	object := func(name, ready, message string) string {
		return fmt.Sprintf(`{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": {"namespace": "flux-system", "name": %q},
  "status": {"conditions": [{"type": "Ready", "status": %q, "reason": "HealthCheckFailed", "message": %q}]}}`, name, ready, message)
	}
	state := func(apps string) string {
		path := filepath.Join(t.TempDir(), "state.json")
		list := `{"kind": "List", "items": [` + object("platform-api", "True", "ok") + "," + apps + `]}`
		if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	args := []string{"check", "-v", "--repo", p.dir, "--cache-dir", cacheDir, "--cluster-state"}

	out, errOut, exit := cli(t, env, append(args, state(object("apps", "True", "ok")))...)
	if exit != 0 || strings.Contains(out, "FL-O001") {
		t.Errorf("the cluster agrees: exit %d, stderr %q\n%.600s", exit, errOut, out)
	}
	if !strings.Contains(out, "FL-O003") {
		t.Errorf("components the cluster state lacks should be listed:\n%.600s", out)
	}

	out, _, exit = cli(t, env, append(args, state(object("apps", "False", "timeout waiting for: [Deployment/apps/web status: 'InProgress']")))...)
	if exit != 1 || !strings.Contains(out, "FL-O001") || !strings.Contains(out, "Deployment/apps/web") {
		t.Errorf("exit %d: an unpredicted failure must fail the run and carry the cluster's message\n%.800s", exit, out)
	}

	_, errOut, exit = cli(t, env, append(args, filepath.Join(p.dir, ".fluxlint.yaml"))...)
	if exit != 2 || !strings.Contains(errOut, "no Flux Kustomization or HelmRelease found") {
		t.Errorf("the wrong file: exit %d, stderr %q", exit, errOut)
	}
}

// A typo in the rules: section must not silently leave the rule switched on.
func TestCLIRejectsUnknownRuleOverride(t *testing.T) {
	p := internalPlatform(t)
	edit(t, p.dir, ".fluxlint.yaml", "kubeVersion:", "rules:\n  disable: [FL-R03]\nkubeVersion:")
	_, errOut, exit := cli(t, []string{"NETRC=" + p.netrc}, "check", "--repo", p.dir, "--cache-dir", cacheDir)
	if exit != 2 || !strings.Contains(errOut, `"FL-R03" is not a rule or a family`) {
		t.Errorf("exit %d, stderr %q", exit, errOut)
	}
}

// rules: works like golangci-lint's linters: start from all or none, then
// enable or disable single rules or whole families.
func TestCLIRuleSelection(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	args := []string{"check", "-v", "--repo", p.dir, "--cache-dir", cacheDir}

	edit(t, p.dir, ".fluxlint.yaml", "kubeVersion:", "rules:\n  disable: [positional-patch, timing]\n  enable: [critical-path]\nkubeVersion:")
	out, _, exit := cli(t, env, args...)
	if exit != 0 || strings.Contains(out, "FL-R003") || !strings.Contains(out, "FL-T001") || strings.Contains(out, "FL-T004") {
		t.Errorf("exit %d: the patch warning and all of timing but the critical path should be gone\n%.800s", exit, out)
	}

	edit(t, p.dir, ".fluxlint.yaml", "  disable: [positional-patch, timing]\n  enable: [critical-path]\n", "  default: none\n  enable: [FL-R003]\n  severity: {FL-R003: error}\n")
	out, _, exit = cli(t, env, args...)
	if exit != 1 || !strings.Contains(out, "error   FL-R003") || strings.Contains(out, "FL-T001") {
		t.Errorf("exit %d: only the patch rule should run, as an error\n%.800s", exit, out)
	}
}

// Settings under an entrypoint apply to that cluster only.
func TestCLIPerEntrypointExternals(t *testing.T) {
	p := internalPlatform(t)
	env := []string{"NETRC=" + p.netrc}
	write(t, p.dir, "apps/legacy.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: legacy}\n")
	out, _, exit := cli(t, env, "check", "--repo", p.dir, "--cache-dir", cacheDir)
	if exit != 1 || !strings.Contains(out, `namespace "legacy" is not created by any component`) {
		t.Fatalf("exit %d: the namespace should be missing\n%.600s", exit, out)
	}
	edit(t, p.dir, ".fluxlint.yaml", "entrypoints:\n  - clusters/prod\n", "entrypoints:\n  - path: clusters/prod\n    externals:\n      namespaces: [legacy]\n")
	if out, _, exit := cli(t, env, "check", "--repo", p.dir, "--cache-dir", cacheDir); exit != 0 {
		t.Errorf("exit %d: this cluster declares the namespace\n%.600s", exit, out)
	}
}
