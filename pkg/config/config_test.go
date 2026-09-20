package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, content string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoSource.Kind != "GitRepository" || c.RepoSource.Name != "flux-system" || c.RepoSource.Namespace != "flux-system" {
		t.Errorf("repoSource = %+v", c.RepoSource)
	}
	if c.KubeVersion != DefaultKubeVersion || c.Timing.DependencyRequeue.Duration != 30*time.Second || c.Timing.DominantShare != 0.4 {
		t.Errorf("defaults = %q, %v, %v", c.KubeVersion, c.Timing.DependencyRequeue.Duration, c.Timing.DominantShare)
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	c, err := load(t, `
entrypoints: [clusters/production]
kubeVersion: "1.33.2"
repoSource: {name: fleet}
externals:
  namespaces: [tenant-a]
  secrets:
    - {namespace: flux-system, name: api-credentials, keys: [token]}
  runtimeCRDs:
    - {group: infrastructure.cluster.x-k8s.io, providedBy: flux-system/providers}
rules:
  default: all
  disable: [FL-T007, timing]
  enable: [critical-path]
  severity:
    positional-patch: error
timing:
  maxBootstrapBound: 30m
  dependencyRequeue: "45s"
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoSource.Name != "fleet" || c.RepoSource.Kind != "GitRepository" {
		t.Errorf("a partial repoSource keeps the other defaults: %+v", c.RepoSource)
	}
	if c.KubeVersion != "1.33.2" || c.Timing.MaxBootstrapBound.Duration != 30*time.Minute || c.Timing.DependencyRequeue.Duration != 45*time.Second {
		t.Errorf("got %q, %v, %v", c.KubeVersion, c.Timing.MaxBootstrapBound.Duration, c.Timing.DependencyRequeue.Duration)
	}
	if c.Rules.Default != "all" || len(c.Rules.Disable) != 2 || c.Rules.Enable[0] != "critical-path" || c.Rules.Severity["positional-patch"] != "error" {
		t.Errorf("rules = %+v", c.Rules)
	}
	if len(c.Externals.Secrets) != 1 || c.Externals.Secrets[0].Keys[0] != "token" || c.Externals.RuntimeCRDs[0].ProvidedBy != "flux-system/providers" {
		t.Errorf("externals = %+v", c.Externals)
	}
}

// A misspelt key must not silently turn a setting off.
func TestUnknownKeysAndBadValuesAreErrors(t *testing.T) {
	for name, content := range map[string]string{
		"unknown top-level key": "entrypoint: clusters/production\n",
		"unknown nested key":    "externals:\n  namespace: [a]\n",
		"bad duration":          "timing:\n  maxBootstrapBound: soon\n",
		"not YAML":              "entrypoints: [\n",
	} {
		if _, err := load(t, content); err == nil {
			t.Errorf("%s: no error", name)
		} else if !strings.Contains(err.Error(), DefaultFile) {
			t.Errorf("%s: the error should name the file: %v", name, err)
		}
	}
}

func TestPerEntrypointSettings(t *testing.T) {
	c, err := load(t, `
kubeVersion: "1.35.0"
externals:
  namespaces: [shared]
timing:
  maxBootstrapBound: 30m
entrypoints:
  - clusters/production
  - path: clusters/staging
    kubeVersion: "1.36.0"
    repoSource: {name: staging-fleet}
    externals:
      namespaces: [sandbox]
      secrets: [{namespace: flux-system, name: staging-only}]
    timing:
      maxBootstrapBound: 45m
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Paths(); len(got) != 2 || got[0] != "clusters/production" || got[1] != "clusters/staging" {
		t.Fatalf("paths = %v", got)
	}

	prod := c.For("clusters/production")
	if prod.KubeVersion != "1.35.0" || len(prod.Externals.Namespaces) != 1 || prod.Timing.MaxBootstrapBound.Duration != 30*time.Minute {
		t.Errorf("production should have the top-level settings: %+v", prod)
	}

	staging := c.For("./clusters/staging/")
	if staging.KubeVersion != "1.36.0" || staging.Timing.MaxBootstrapBound.Duration != 45*time.Minute {
		t.Errorf("staging overrides: %q, %v", staging.KubeVersion, staging.Timing.MaxBootstrapBound.Duration)
	}
	if staging.RepoSource.Name != "staging-fleet" || staging.RepoSource.Kind != "GitRepository" || staging.RepoSource.Namespace != "flux-system" {
		t.Errorf("a partial repoSource keeps the defaults: %+v", staging.RepoSource)
	}
	if got := staging.Externals.Namespaces; len(got) != 2 || got[0] != "shared" || got[1] != "sandbox" {
		t.Errorf("externals are added to the shared ones: %v", got)
	}
	if len(staging.Externals.Secrets) != 1 || staging.Timing.DependencyRequeue.Duration != 30*time.Second {
		t.Errorf("staging = %+v", staging)
	}
	// one cluster's settings must not leak into another's, or into the shared ones
	if len(c.Externals.Namespaces) != 1 || len(c.For("clusters/production").Externals.Namespaces) != 1 {
		t.Error("per-entrypoint externals leaked")
	}
	if other := c.For("clusters/unlisted"); other != c {
		t.Error("an unlisted entrypoint gets the top-level settings")
	}
}

func TestEntrypointErrors(t *testing.T) {
	for name, content := range map[string]string{
		"unknown key in an entry": "entrypoints:\n  - path: clusters/a\n    kubeVerson: \"1.35.0\"\n",
		"entry without a path":    "entrypoints:\n  - kubeVersion: \"1.35.0\"\n",
	} {
		if _, err := load(t, content); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
